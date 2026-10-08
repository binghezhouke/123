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

func TestImagePrefetchCancellationAndNaturalOrder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tree := &Tree{ctx: ctx, opts: Options{PrefetchFiles: 9}}
	p := newImagePrefetch(tree)
	child, stop := context.WithCancel(ctx)
	defer stop()
	p.cancel = stop
	p.interrupt()
	finish := p.foreground(&Node{item: &entry{name: "x.jpg"}})
	generation := p.generation
	p.observe(&Node{})
	if p.generation != generation {
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

func waitPrefetchIdle(t *testing.T, p *imagePrefetch) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for len(p.slots) > 0 {
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
	source, err := dir.source(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"2.jpg", "10.jpg"} {
		key := source.Key() + ":member:" + name + fmt.Sprintf(":%08x:%d", crc32.ChecksumIEEE(content), len(content))
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
	p.cancel = stop
	p.running[prefetchTarget{parent, "2.jpg"}] = 1
	finish := p.foreground(node)
	if job.Err() != nil {
		t.Fatal("foreground cancelled the fill it needs")
	}
	finish()
	finish = p.foreground(&Node{parent: parent, item: &entry{name: "9.jpg"}})
	if job.Err() != context.Canceled {
		t.Fatal("unrelated prefetch not cancelled")
	}
	finish()
}
