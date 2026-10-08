package mountfs

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func TestImagePredictionWindowsAndBudget(t *testing.T) {
	tree := &Tree{opts: Options{PrefetchFiles: 9, PrefetchBytes: 1000, PrefetchWorkers: 2}}
	p := newImagePrefetch(tree)
	parent := &Node{}
	entries := map[string]*entry{}
	for i := 1; i <= 20; i++ {
		name := fmt.Sprintf("%d.jpg", i)
		entries[name] = &entry{name: name, cloud: &panapi.File{Size: 10}}
	}
	for _, tc := range []struct {
		read  string
		count int
		next  string
	}{{"1.jpg", 2, "2.jpg"}, {"2.jpg", 4, "3.jpg"}, {"3.jpg", 9, "4.jpg"}, {"2.jpg", 1, "1.jpg"}, {"15.jpg", 2, "16.jpg"}, {"14.jpg", 2, "13.jpg"}, {"13.jpg", 4, "12.jpg"}, {"12.jpg", 9, "11.jpg"}} {
		got := p.plan(&Node{parent: parent, item: entries[tc.read]}, entries)
		if len(got) != tc.count || got[0].name != tc.next {
			t.Fatalf("%s: count=%d next=%v", tc.read, len(got), got)
		}
	}
	tree.opts.PrefetchBytes = 15
	got := p.plan(&Node{parent: parent, item: entries["5.jpg"]}, entries)
	if len(got) != 1 {
		t.Fatalf("budget ignored: %d", len(got))
	}
	entries["6.jpg"].cloud.Size = 100
	got = p.plan(&Node{parent: parent, item: entries["5.jpg"]}, entries)
	if len(got) != 1 || got[0].name != "7.jpg" {
		t.Fatal("oversized candidate not skipped")
	}
}

func TestImagePredictionResetsAfterIdleAndHonorsConfiguredWindow(t *testing.T) {
	tree := &Tree{opts: Options{PrefetchFiles: 64, PrefetchBytes: 1 << 30, PrefetchWorkers: 2}}
	p := newImagePrefetch(tree)
	parent := &Node{item: &entry{directory: true, cloud: &panapi.File{ID: 9, IsDir: true}}}
	entries := make(map[string]*entry, 100)
	for i := 1; i <= 100; i++ {
		name := fmt.Sprintf("%d.jpg", i)
		entries[name] = &entry{name: name, cloud: &panapi.File{ID: int64(i), Version: "v1", Size: 1}}
	}
	for _, name := range []string{"1.jpg", "2.jpg", "3.jpg"} {
		p.plan(&Node{parent: parent, item: entries[name]}, entries)
	}
	if got := len(p.plan(&Node{parent: parent, item: entries["4.jpg"]}, entries)); got != 64 {
		t.Fatalf("configured 64-image window was truncated: got %d", got)
	}
	p.lastRead = time.Now().Add(-31 * time.Second)
	if got := len(p.plan(&Node{parent: parent, item: entries["5.jpg"]}, entries)); got != 2 {
		t.Fatalf("idle prediction did not restart with a two-image window: got %d", got)
	}
}

func TestPrefetchDeadlineIsFailureButCancellationIsCancelled(t *testing.T) {
	tree := &Tree{ctx: context.Background(), opts: Options{PrefetchWorkers: 1}}
	p := newImagePrefetch(tree)
	target := prefetchTarget{directory: "d", name: "x.jpg", content: "v1"}
	jobCtx, cancel := context.WithCancel(context.Background())
	job := &prefetchJob{cancel: cancel}
	p.running[target] = job
	cancel()
	p.runTarget(jobCtx, target, job, &Node{})
	stats := p.treeStatsForTest()
	if stats.Cancelled != 1 || stats.Failed != 0 {
		t.Fatalf("active cancellation stats = cancelled %d failed %d", stats.Cancelled, stats.Failed)
	}

	deadlineCtx, deadlineCancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer deadlineCancel()
	deadlineJob := &prefetchJob{cancel: func() {}}
	p.running[target] = deadlineJob
	p.runTarget(deadlineCtx, target, deadlineJob, &Node{})
	stats = p.treeStatsForTest()
	if stats.Cancelled != 1 || stats.Failed != 1 {
		t.Fatalf("deadline stats = cancelled %d failed %d", stats.Cancelled, stats.Failed)
	}
}

func (p *imagePrefetch) treeStatsForTest() ImagePrefetchStatsSnapshot {
	p.statsMu.Lock()
	defer p.statsMu.Unlock()
	return p.stats
}

type prefetchAPI struct {
	url   string
	files []panapi.File
}

func (a prefetchAPI) List(context.Context, int64) ([]panapi.File, error) { return a.files, nil }
func (a prefetchAPI) DownloadURL(_ context.Context, id int64) (string, error) {
	return a.url + "/" + strconv.FormatInt(id, 10), nil
}

func TestImagePrefetchWarmsHTTPDataWithoutMetadataTrigger(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	hits := map[string]int{}
	done := make(chan string, 64)
	data := bytes.Repeat([]byte("image"), 2048)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits[r.URL.Path]++
		mu.Unlock()
		w.Header().Set("ETag", `"stable"`)
		http.ServeContent(w, r, "image", time.Unix(1, 0), bytes.NewReader(data))
		if r.Header.Get("Range") != "bytes=0-0" {
			done <- r.URL.Path
		}
	}))
	defer server.Close()
	cache, err := storage.NewCache(t.TempDir(), 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	files := []panapi.File{}
	for i := 1; i <= 12; i++ {
		files = append(files, panapi.File{ID: int64(i), Name: fmt.Sprintf("%d.jpg", i), Size: int64(len(data)), Version: "v1"})
	}
	root := NewWithOptions(ctx, prefetchAPI{server.URL, files}, cache, 0, true, Options{PrefetchFiles: 9, PrefetchBytes: 256 << 20, PrefetchWorkers: 2})
	fs.NewNodeFS(root, &fs.Options{})
	if _, errno := root.Readdir(ctx); errno != 0 {
		t.Fatal(errno)
	}
	first := lookup(t, root, "1.jpg")
	var attr fuse.AttrOut
	first.Getattr(ctx, nil, &attr)
	mu.Lock()
	count := len(hits)
	mu.Unlock()
	if count != 0 {
		t.Fatal("metadata traversal triggered reads")
	}
	h, _, errno := first.Open(ctx, syscall.O_RDONLY)
	if errno != 0 {
		t.Fatal(errno)
	}
	// Opening a file only probes that file, not its neighbours.
	mu.Lock()
	count = len(hits)
	mu.Unlock()
	if count != 1 {
		t.Fatal("Open triggered adjacent reads")
	}
	result, errno := h.(fs.FileReader).Read(ctx, make([]byte, len(data)), 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	result.Done()
	h.(fs.FileReleaser).Release(ctx)
	seen := map[string]bool{}
	deadline := time.After(3 * time.Second)
	for !seen["/2"] || !seen["/3"] {
		select {
		case name := <-done:
			seen[name] = true
		case <-deadline:
			t.Fatal("adjacent image prefetch did not complete")
		}
	}
	// Both HTTP responses have arrived; wait for workers to publish their fills.
	waitPrefetchIdle(t, root.tree.prefetch)
	// A foreground open/read now needs no further HTTP request for this image.
	second := lookup(t, root, "2.jpg")
	h, _, errno = second.Open(ctx, syscall.O_RDONLY)
	if errno != 0 {
		t.Fatal(errno)
	}
	mu.Lock()
	before := hits["/2"]
	mu.Unlock()
	result, errno = h.(fs.FileReader).Read(ctx, make([]byte, len(data)), 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	got, status := result.Bytes(nil)
	if status != fuse.OK || !bytes.Equal(got, data) {
		t.Fatal("prefetched bytes mismatch")
	}
	result.Done()
	h.(fs.FileReleaser).Release(ctx)
	mu.Lock()
	after := hits["/2"]
	mu.Unlock()
	if after != before {
		t.Fatal("prefetched image downloaded again")
	}
	cancel()
	root.tree.prefetch.interrupt()
}

func TestImagePrefetchReusesOverlappingWindowAcrossForegroundReads(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	hits := map[string]int{}
	startedThree := make(chan struct{}, 1)
	releaseThree := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseThree) }) }
	data := bytes.Repeat([]byte("window-image"), 1024)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if r.Header.Get("Range") != "bytes=0-0" {
			hits[r.URL.Path]++
		}
		mu.Unlock()
		if r.URL.Path == "/3" && r.Header.Get("Range") != "bytes=0-0" {
			select {
			case startedThree <- struct{}{}:
			default:
			}
			select {
			case <-releaseThree:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("ETag", `"window"`)
		http.ServeContent(w, r, "image", time.Unix(1, 0), bytes.NewReader(data))
	}))
	defer server.Close()
	cache, err := storage.NewCache(t.TempDir(), 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	files := make([]panapi.File, 0, 8)
	for i := 1; i <= 8; i++ {
		files = append(files, panapi.File{ID: int64(i), Name: fmt.Sprintf("%d.jpg", i), Size: int64(len(data)), Version: "v1"})
	}
	root := NewWithOptions(ctx, prefetchAPI{server.URL, files}, cache, 0, true, Options{PrefetchFiles: 9, PrefetchBytes: 256 << 20, PrefetchWorkers: 1})
	fs.NewNodeFS(root, &fs.Options{})
	if _, errno := root.Readdir(ctx); errno != 0 {
		t.Fatal(errno)
	}
	defer func() {
		release()
		root.tree.prefetch.interrupt()
		waitPrefetchIdle(t, root.tree.prefetch)
		cache.Close()
	}()
	read := func(name string) {
		t.Helper()
		node := lookup(t, root, name)
		h, _, errno := node.Open(ctx, syscall.O_RDONLY)
		if errno != 0 {
			t.Fatal(errno)
		}
		result, errno := h.(fs.FileReader).Read(ctx, make([]byte, len(data)), 0)
		if errno != 0 {
			t.Fatal(errno)
		}
		result.Done()
		if errno := h.(fs.FileReleaser).Release(ctx); errno != 0 {
			t.Fatal(errno)
		}
	}
	read("1.jpg")
	select {
	case <-startedThree:
	case <-time.After(3 * time.Second):
		t.Fatal("prefetch did not start the overlapping image")
	}
	// Let image 2 finish while image 3 remains blocked in HTTP.
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		twoFetched := hits["/2"] > 0
		mu.Unlock()
		if twoFetched {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first window did not request image 2")
		}
		time.Sleep(time.Millisecond)
	}
	read("2.jpg")
	deadline = time.Now().Add(3 * time.Second)
	for root.ImagePrefetchStats().Reused == 0 {
		if time.Now().After(deadline) {
			t.Fatal("overlapping image task was not reused")
		}
		time.Sleep(time.Millisecond)
	}
	release()
	waitPrefetchIdle(t, root.tree.prefetch)
	mu.Lock()
	threeHits := hits["/3"]
	mu.Unlock()
	if threeHits != 1 {
		t.Fatalf("overlapping target fetched %d times, want once", threeHits)
	}
}

func TestImagePrefetchCancellationAndNaturalOrder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tree := &Tree{ctx: ctx, opts: Options{PrefetchFiles: 9}}
	p := newImagePrefetch(tree)
	child, stop := context.WithCancel(ctx)
	defer stop()
	target := prefetchTarget{directory: "test-dir", name: "x.jpg", content: "v1"}
	p.running[target] = &prefetchJob{cancel: stop}
	p.interrupt()
	finish := p.foreground(&Node{item: &entry{name: "x.jpg"}})
	sequence := p.sequence
	p.observe(&Node{})
	if p.sequence != sequence {
		t.Fatal("prefetch started during foreground Open")
	}
	finish()
	if child.Err() != context.Canceled {
		t.Fatal("foreground did not cancel background")
	}
	if !naturalLess("2.jpg", "10.jpg") || !naturalLess("0002.jpg", "10.jpg") {
		t.Fatal("numeric order")
	}
	if imageName("archive.zip") || !imageName("Photo.JPEG") {
		t.Fatal("image filter")
	}
}

func TestPrefetchTargetIdentityTracksDirectoryAndContentVersion(t *testing.T) {
	tree := &Tree{meta: map[string]*metaItem{}}
	parent := &Node{tree: tree, item: &entry{name: "folder", directory: true, cloud: &panapi.File{ID: 7, IsDir: true}}}
	tree.meta["dir:7"] = &metaItem{value: &cloudDirectory{generation: 11}}
	p := newImagePrefetch(tree)
	first := &Node{tree: tree, parent: parent, item: &entry{name: "same.jpg", cloud: &panapi.File{ID: 20, Version: "v1", Size: 100}}}
	initial := p.targetIdentity(first)
	newVersion := &Node{tree: tree, parent: parent, item: &entry{name: "same.jpg", cloud: &panapi.File{ID: 20, Version: "v2", Size: 100}}}
	if p.targetIdentity(newVersion) == initial {
		t.Fatal("same-name content version change reused the old task identity")
	}
	tree.meta["dir:7"].value = &cloudDirectory{generation: 12}
	if p.targetIdentity(first) == initial {
		t.Fatal("refreshed directory generation reused old task identity")
	}
}

func TestPrefetchCompletionHistoryDeduplicatesBeforeEviction(t *testing.T) {
	tree := &Tree{}
	p := newImagePrefetch(tree)
	target := prefetchTarget{directory: "d", name: "same.jpg", content: "v1"}
	p.completed[target] = struct{}{}
	p.completedOrder = append(p.completedOrder, target, target)
	for i := 0; i < 63; i++ {
		other := prefetchTarget{directory: "d", name: fmt.Sprintf("%d.jpg", i), content: "v1"}
		p.completed[other] = struct{}{}
		p.completedOrder = append(p.completedOrder, other)
	}
	p.rememberCompletedLocked(target)
	if len(p.completedOrder) != 64 {
		t.Fatalf("completion recency has %d entries, want 64", len(p.completedOrder))
	}
	if _, exists := p.completed[target]; !exists {
		t.Fatal("duplicate stale recency key deleted the newest completion")
	}
}

func TestRapidPlannerCancellationFinishesEachStartedTaskOnce(t *testing.T) {
	tree := &Tree{ctx: context.Background(), opts: Options{PrefetchWorkers: 1}}
	p := newImagePrefetch(tree)
	p.slots <- struct{}{} // Keep each task pending in its dispatcher.
	defer func() { <-p.slots }()
	var firstTarget prefetchTarget
	var firstJob *prefetchJob
	for i := 0; i < 5; i++ {
		target := prefetchTarget{directory: "d", name: fmt.Sprintf("%d.jpg", i), content: "v1"}
		jobCtx, jobCancel := context.WithTimeout(context.Background(), time.Minute)
		job := &prefetchJob{cancel: jobCancel, ctx: jobCtx}
		p.mu.Lock()
		p.sequence++
		sequence := p.sequence
		p.running[target] = job
		p.mu.Unlock()
		context.AfterFunc(jobCtx, func() { p.finishUnstarted(target, job, jobCtx.Err()) })
		p.addStat(func(s *ImagePrefetchStatsSnapshot) { s.Started++ })
		if i == 0 {
			firstTarget, firstJob = target, job
		}
		plannerCtx, plannerCancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			p.dispatch(plannerCtx, sequence, []pendingPrefetch{{target: target, job: job}})
			close(done)
		}()
		plannerCancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("dispatcher waited for a worker after its planner was cancelled")
		}
	}
	p.interrupt()
	deadline := time.After(time.Second)
	for {
		stats := p.treeStatsForTest()
		if stats.Started == stats.Completed+stats.Cancelled+stats.Failed {
			break
		}
		select {
		case <-time.After(time.Millisecond):
		case <-deadline:
			t.Fatal("unstarted jobs were not cleaned up after cancellation")
		}
	}
	// A late duplicate terminal callback must not count the same job twice.
	p.finishTarget(firstTarget, firstJob, true, nil)
	stats := p.treeStatsForTest()
	if stats.Started != 5 || stats.Cancelled != 5 || stats.Completed != 0 || stats.Failed != 0 {
		t.Fatalf("non-terminal task accounting: started=%d completed=%d cancelled=%d failed=%d", stats.Started, stats.Completed, stats.Cancelled, stats.Failed)
	}
}

func waitPrefetchIdle(t *testing.T, p *imagePrefetch) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		p.mu.Lock()
		jobs := len(p.running)
		p.mu.Unlock()
		if jobs == 0 && len(p.slots) == 0 {
			return
		}
		select {
		case <-time.After(time.Millisecond):
		case <-deadline:
			t.Fatal("prefetch workers did not finish")
		}
	}
}

func TestImagePrefetchInflatesZIPMembersIntoSharedCache(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	content := bytes.Repeat([]byte("picture payload"), 100)
	for _, name := range []string{"1.jpg", "2.jpg", "10.jpg"} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(content)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"zip"`)
		http.ServeContent(w, r, "images.zip", time.Unix(1, 0), bytes.NewReader(archive.Bytes()))
	}))
	defer server.Close()
	cache, err := storage.NewCache(t.TempDir(), 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	root := NewWithOptions(ctx, &fakeAPI{url: server.URL, archive: archive.Bytes()}, cache, 0, true, Options{PrefetchFiles: 9})
	fs.NewNodeFS(root, &fs.Options{})
	dir := lookup(t, root, "photos.zip")
	first := lookup(t, dir, "1.jpg")
	// Run the same planner synchronously after checking that the foreground
	// member read succeeds; background workers use the public cache fill path.
	h, _, errno := first.openRaw(ctx, syscall.O_RDONLY)
	if errno != 0 {
		t.Fatal(errno)
	}
	result, errno := h.(fs.FileReader).Read(ctx, make([]byte, len(content)), 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	result.Done()
	h.(fs.FileReleaser).Release(ctx)
	p := root.tree.prefetch
	p.run(ctx, first, 0)
	waitPrefetchIdle(t, p)
	source, err := dir.source(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"2.jpg", "10.jpg"} {
		key := root.tree.diskCacheScope() + ":" + source.Key() + ":.zip:member:" + name + fmt.Sprintf(":%08x:%d", crc32.ChecksumIEEE(content), len(content))
		cached, err := cache.Acquire(ctx, key, int64(len(content)), func(context.Context, io.Writer) error { t.Errorf("%s was not prefetched", name); return syscall.EIO })
		if err != nil {
			t.Fatal(err)
		}
		got := make([]byte, len(content))
		if _, err := cached.ReadAt(got, 0); err != nil {
			t.Fatal(err)
		}
		cached.Close()
		if !bytes.Equal(got, content) {
			t.Fatal("ZIP prefetched payload mismatch")
		}
	}
}

func TestActualFUSEImagePrefetch(t *testing.T) {
	if os.Getenv("MOUNT123_FUSE_TEST") != "1" {
		t.Skip("requires FUSE")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	data := bytes.Repeat([]byte("image"), 1000)
	ready := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"fuse-image"`)
		http.ServeContent(w, r, "image", time.Unix(1, 0), bytes.NewReader(data))
		if r.URL.Path == "/2" && r.Header.Get("Range") != "bytes=0-0" {
			select {
			case ready <- struct{}{}:
			default:
			}
		}
	}))
	defer server.Close()
	cache, err := storage.NewCache(t.TempDir(), 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	files := []panapi.File{{ID: 1, Name: "1.jpg", Size: int64(len(data)), Version: "v1"}, {ID: 2, Name: "2.jpg", Size: int64(len(data)), Version: "v1"}}
	root := NewWithOptions(ctx, prefetchAPI{server.URL, files}, cache, 0, true, Options{PrefetchFiles: 9})
	point := t.TempDir()
	mounted, err := fs.Mount(point, root, &fs.Options{MountOptions: fuse.MountOptions{Options: []string{"ro"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cancel()
		root.tree.prefetch.interrupt()
		if err := mounted.Unmount(); err != nil {
			t.Error(err)
		}
		mounted.Wait()
	}()
	got, err := os.ReadFile(filepath.Join(point, "1.jpg"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("kernel read: %v", err)
	}
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("kernel Read did not trigger next-image prefetch")
	}
	waitPrefetchIdle(t, root.tree.prefetch)
	got, err = os.ReadFile(filepath.Join(point, "2.jpg"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("prefetched kernel read: %v", err)
	}
}

func TestPrefetchForegroundKeepsItsActiveTarget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tree := &Tree{ctx: ctx, opts: Options{PrefetchFiles: 9}}
	p := newImagePrefetch(tree)
	parent := &Node{}
	node := &Node{parent: parent, item: &entry{name: "2.jpg"}}
	job, stop := context.WithCancel(ctx)
	defer stop()
	p.running[p.targetIdentity(node)] = &prefetchJob{cancel: stop}
	finish := p.foreground(node)
	if job.Err() != nil {
		t.Fatal("foreground cancelled the fill it needs")
	}
	finish()
	finish = p.foreground(&Node{parent: parent, item: &entry{name: "9.jpg"}})
	if job.Err() != nil {
		t.Fatal("same-directory overlap cancelled before the next window was planned")
	}
	finish()
	otherParent := &Node{item: &entry{cloud: &panapi.File{ID: 42, IsDir: true}, directory: true}, tree: tree}
	otherNode := &Node{parent: otherParent, item: &entry{name: "10.jpg", cloud: &panapi.File{ID: 10, Version: "v1", Size: 1}}, tree: tree}
	otherJob, otherStop := context.WithCancel(ctx)
	defer otherStop()
	otherTarget := p.targetIdentity(&Node{parent: parent, item: &entry{name: "x.jpg"}})
	p.running[otherTarget] = &prefetchJob{cancel: otherStop}
	finish = p.foreground(otherNode)
	if otherJob.Err() != context.Canceled {
		t.Fatal("directory change did not cancel unrelated prefetch")
	}
	finish()
}
