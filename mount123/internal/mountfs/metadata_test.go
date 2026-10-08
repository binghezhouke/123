package mountfs

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type countingAPI struct {
	base     *fakeAPI
	lists    atomic.Int32
	urls     atomic.Int32
	urlDelay time.Duration
}

type blockingInfoAPI struct {
	*fakeAPI
	started   chan struct{}
	canceled  chan struct{}
	startOnce sync.Once
}
type metadataFixtureAPI struct {
	*fakeAPI
	infos     map[int64]panapi.File
	details   map[int64]panapi.File
	infoCalls atomic.Int32
}

func (a *metadataFixtureAPI) Infos(_ context.Context, ids []int64) ([]panapi.File, error) {
	a.infoCalls.Add(1)
	files := make([]panapi.File, 0, len(ids))
	for _, id := range ids {
		if f, ok := a.infos[id]; ok {
			files = append(files, f)
		}
	}
	return files, nil
}
func (a *metadataFixtureAPI) Detail(_ context.Context, id int64) (panapi.File, error) {
	return a.details[id], nil
}

func (a *blockingInfoAPI) Infos(ctx context.Context, ids []int64) ([]panapi.File, error) {
	a.startOnce.Do(func() { close(a.started) })
	<-ctx.Done()
	close(a.canceled)
	return nil, ctx.Err()
}
func (a *blockingInfoAPI) Detail(context.Context, int64) (panapi.File, error) {
	return panapi.File{}, nil
}

func (a *countingAPI) List(ctx context.Context, id int64) ([]panapi.File, error) {
	a.lists.Add(1)
	return a.base.List(ctx, id)
}
func (a *countingAPI) DownloadURL(ctx context.Context, id int64) (string, error) {
	a.urls.Add(1)
	if a.urlDelay > 0 {
		select {
		case <-time.After(a.urlDelay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return a.base.DownloadURL(ctx, id)
}

func TestMetadataCacheTTLAndBudget(t *testing.T) {
	tree := newMetadataTestTree(12)
	build := func(value int) func(context.Context) (any, int64, error) {
		return func(context.Context) (any, int64, error) { return value, 8, nil }
	}
	if _, err := tree.loadMeta(context.Background(), "one", time.Second, build(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := tree.loadMeta(context.Background(), "one", time.Second, build(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := tree.loadMeta(context.Background(), "two", time.Second, build(2)); err != nil {
		t.Fatal(err)
	}
	value, err := tree.loadMeta(context.Background(), "one", time.Second, build(3))
	if err != nil {
		t.Fatal(err)
	}
	if value != 3 {
		t.Fatal("least recently used entry survived budget eviction")
	}
	if _, err := tree.loadMeta(context.Background(), "expired", time.Millisecond, func(context.Context) (any, int64, error) { return 3, 1, nil }); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	value, err = tree.loadMeta(context.Background(), "expired", time.Second, func(context.Context) (any, int64, error) { return 4, 1, nil })
	if err != nil {
		t.Fatal(err)
	}
	if value != 4 {
		t.Fatal("expired metadata reused")
	}
}

func newMetadataTestTree(budget int64) *Tree {
	return &Tree{ctx: context.Background(), opts: defaults(Options{MetadataBytes: budget, MaxConcurrentBuilds: 1}), meta: map[string]*metaItem{}, builds: make(chan struct{}, 1), sources: map[string]*sourceCall{}}
}

func TestMetadataOwnerCancellationAndOversizeFailure(t *testing.T) {
	tree := newMetadataTestTree(8)
	ownerCtx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	ownerDone := make(chan error, 1)
	go func() {
		_, err := tree.loadMeta(ownerCtx, "shared", time.Second, func(ctx context.Context) (any, int64, error) { close(started); <-ctx.Done(); return nil, 0, ctx.Err() })
		ownerDone <- err
	}()
	<-started
	waiterDone := make(chan error, 1)
	go func() {
		value, err := tree.loadMeta(context.Background(), "shared", time.Second, func(context.Context) (any, int64, error) { return "waiter", 4, nil })
		if err == nil && value != "waiter" {
			err = context.Canceled
		}
		waiterDone <- err
	}()
	// Let the waiter observe the owner flight before canceling that owner.
	time.Sleep(10 * time.Millisecond)
	cancel()
	if err := <-ownerDone; err == nil {
		t.Fatal("canceled owner succeeded")
	}
	if err := <-waiterDone; err != nil {
		t.Fatalf("waiter did not rebuild after owner cancellation: %v", err)
	}
	value, err := tree.loadMeta(context.Background(), "shared", time.Second, func(context.Context) (any, int64, error) { t.Fatal("canceled result was cached"); return nil, 0, nil })
	if err != nil || value != "waiter" {
		t.Fatalf("cached waiter result = %v, %v", value, err)
	}

	var builds atomic.Int32
	_, err = tree.loadMeta(context.Background(), "too-large", time.Second, func(context.Context) (any, int64, error) { builds.Add(1); return 1, 9, nil })
	if err == nil {
		t.Fatal("oversize metadata accepted")
	}
	if builds.Load() != 1 {
		t.Fatalf("oversize builder ran %d times", builds.Load())
	}
}

func TestInfoBatchCancelsWhenEveryWaiterCancels(t *testing.T) {
	base, _ := fixture(t)
	defer base.tree.cache.Close()
	api := &blockingInfoAPI{fakeAPI: base.tree.api.(*fakeAPI), started: make(chan struct{}), canceled: make(chan struct{})}
	tree := base.tree
	tree.api = api
	ctx1, cancel1 := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())
	results := make(chan error, 2)
	go func() { _, err := tree.requestInfo(ctx1, api, 10); results <- err }()
	go func() { _, err := tree.requestInfo(ctx2, api, 11); results <- err }()
	select {
	case <-api.started:
	case <-time.After(time.Second):
		t.Fatal("Infos batch did not start")
	}
	cancel1()
	cancel2()
	for i := 0; i < 2; i++ {
		select {
		case err := <-results:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("waiter error = %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("canceled waiter did not return")
		}
	}
	select {
	case <-api.canceled:
	case <-time.After(time.Second):
		t.Fatal("batch API context was not canceled")
	}
}

func TestRootDetailAndOptionalFileInfoTimes(t *testing.T) {
	base, _ := fixture(t)
	defer base.tree.cache.Close()
	created := time.Date(2025, 10, 2, 4, 16, 19, 0, time.UTC)
	updated := time.Date(2025, 10, 3, 5, 6, 7, 123456789, time.UTC)
	rootInfo := panapi.File{ID: 42, Name: "mounted", IsDir: true, CreatedAt: created, UpdatedAt: updated}
	api := &metadataFixtureAPI{fakeAPI: base.tree.api.(*fakeAPI), infos: map[int64]panapi.File{1: {ID: 1, Name: "photos.zip", Size: int64(len(base.tree.api.(*fakeAPI).archive)), Version: "v1", CreatedAt: created, UpdatedAt: updated}}, details: map[int64]panapi.File{42: rootInfo, 43: {ID: 43, Name: "file", IsDir: false}}}
	root := NewWithOptions(context.Background(), api, base.tree.cache, 42, true, Options{RefreshFileMetadata: true})
	if err := root.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	var rootAttr fuse.AttrOut
	if errno := root.Getattr(context.Background(), nil, &rootAttr); errno != 0 {
		t.Fatal(errno)
	}
	if rootAttr.Mtime != uint64(updated.Unix()) || rootAttr.Mtimensec != uint32(updated.Nanosecond()) || rootAttr.Ctime != uint64(updated.Unix()) {
		t.Fatalf("root times mtime=%d.%09d ctime=%d", rootAttr.Mtime, rootAttr.Mtimensec, rootAttr.Ctime)
	}
	fs.NewNodeFS(root, &fs.Options{})
	var entry fuse.EntryOut
	inode, errno := root.Lookup(context.Background(), "photos.zip", &entry)
	if errno != 0 {
		t.Fatal(errno)
	}
	if entry.Attr.Mtime != uint64(updated.Unix()) || entry.Attr.Mtimensec != uint32(updated.Nanosecond()) {
		t.Fatalf("file times = %d.%09d", entry.Attr.Mtime, entry.Attr.Mtimensec)
	}
	if got := api.infoCalls.Load(); got != 1 {
		t.Fatalf("Infos calls after Lookup=%d, want 1", got)
	}
	if errno := inode.Operations().(*Node).Getattr(context.Background(), nil, &fuse.AttrOut{}); errno != 0 {
		t.Fatal(errno)
	}
	if got := api.infoCalls.Load(); got != 1 {
		t.Fatalf("Getattr bypassed metadata TTL cache: %d Infos calls", got)
	}
	if root.item.cloud.ID != 42 || root.item.cloud.Version != "" {
		t.Fatal("root detail unexpectedly changed the mounted root identity")
	}
	defaultRoot := New(context.Background(), api, base.tree.cache, 0, true)
	fs.NewNodeFS(defaultRoot, &fs.Options{})
	if err := defaultRoot.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, errno := defaultRoot.Lookup(context.Background(), "photos.zip", &fuse.EntryOut{}); errno != 0 {
		t.Fatal(errno)
	}
	if got := api.infoCalls.Load(); got != 1 {
		t.Fatalf("default Lookup made an Infos request: %d", got-1)
	}

	invalid := New(context.Background(), api, base.tree.cache, 43, true)
	if err := invalid.Prepare(context.Background()); err == nil {
		t.Fatal("non-directory root accepted")
	}
	trashedAPI := &metadataFixtureAPI{fakeAPI: api.fakeAPI, infos: map[int64]panapi.File{1: {ID: 1, Name: "photos.zip", Trashed: true}}, details: api.details}
	trashed := NewWithOptions(context.Background(), trashedAPI, base.tree.cache, 0, true, Options{RefreshFileMetadata: true})
	fs.NewNodeFS(trashed, &fs.Options{})
	if err := trashed.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, errno := trashed.Lookup(context.Background(), "photos.zip", &fuse.EntryOut{}); errno != syscall.ENOENT {
		t.Fatalf("trashed Lookup errno=%v, want ENOENT", errno)
	}
}

func TestFileInfoParallelDedupAndStaleVersion(t *testing.T) {
	base, _ := fixture(t)
	defer base.tree.cache.Close()
	archiveSize := int64(len(base.tree.api.(*fakeAPI).archive))
	fresh := panapi.File{ID: 1, Name: "photos.zip", Size: archiveSize, Version: "v1", UpdatedAt: time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)}
	api := &metadataFixtureAPI{fakeAPI: base.tree.api.(*fakeAPI), infos: map[int64]panapi.File{1: fresh}}
	root := NewWithOptions(context.Background(), api, base.tree.cache, 0, true, Options{RefreshFileMetadata: true})
	fs.NewNodeFS(root, &fs.Options{})
	if err := root.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan syscall.Errno, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, errno := root.Lookup(context.Background(), "photos.zip", &fuse.EntryOut{}); errno != 0 {
				errs <- errno
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if got := api.infoCalls.Load(); got != 1 {
		t.Fatalf("parallel same-ID lookups issued %d Infos batches", got)
	}
	if _, errno := root.Lookup(context.Background(), "photos.zip", &fuse.EntryOut{}); errno != 0 {
		t.Fatal(errno)
	}
	if got := api.infoCalls.Load(); got != 1 {
		t.Fatalf("repeat lookup bypassed TTL cache: %d", got)
	}

	staleAPI := &metadataFixtureAPI{fakeAPI: api.fakeAPI, infos: map[int64]panapi.File{1: {ID: 1, Name: "photos.zip", Size: archiveSize, Version: "changed"}}}
	staleRoot := NewWithOptions(context.Background(), staleAPI, base.tree.cache, 0, true, Options{RefreshFileMetadata: true})
	fs.NewNodeFS(staleRoot, &fs.Options{})
	if err := staleRoot.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, errno := staleRoot.Lookup(context.Background(), "photos.zip", &fuse.EntryOut{}); errno != syscall.ESTALE {
		t.Fatalf("changed file version errno=%v, want ESTALE", errno)
	}
}

func TestDirectoryAndZIPMetadataReuse(t *testing.T) {
	base, _ := fixture(t)
	defer base.tree.cache.Close()
	api := &countingAPI{base: base.tree.api.(*fakeAPI), urlDelay: 20 * time.Millisecond}
	root := NewWithOptions(context.Background(), api, base.tree.cache, 0, true, Options{DirectoryTTL: 25 * time.Millisecond, SourceTTL: 25 * time.Millisecond, ZIPIndexTTL: 25 * time.Millisecond})
	fs.NewNodeFS(root, &fs.Options{})
	if err := root.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := root.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := api.lists.Load(); got != 1 {
		t.Fatalf("directory listed %d times, want 1", got)
	}
	archive := lookup(t, root, "photos.zip")
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, errno := archive.Readdir(context.Background()); errno != 0 {
				errs <- errno
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if got := api.urls.Load(); got != 1 {
		t.Fatalf("source resolved %d times, want 1", got)
	}
	time.Sleep(35 * time.Millisecond)
	if _, errno := archive.Readdir(context.Background()); errno != 0 {
		t.Fatal(errno)
	}
	if got := api.urls.Load(); got != 1 {
		t.Fatalf("download URL resolved %d times after source TTL, want 1", got)
	}
	if got := api.lists.Load(); got != 1 {
		t.Fatalf("ZIP cache triggered cloud listings: %d", got)
	}
}

func TestZIPIndexLimitAndLightweightInodes(t *testing.T) {
	base, _ := fixture(t)
	defer base.tree.cache.Close()
	root := NewWithOptions(context.Background(), base.tree.api, base.tree.cache, 0, true, Options{MaxExpandedNodes: 1})
	fs.NewNodeFS(root, &fs.Options{})
	archive := lookup(t, root, "photos.zip")
	if _, errno := archive.Readdir(context.Background()); errno == 0 {
		t.Fatal("oversized expanded tree accepted")
	}

	root = New(context.Background(), base.tree.api, base.tree.cache, 0, true)
	fs.NewNodeFS(root, &fs.Options{})
	archive = lookup(t, root, "photos.zip")
	images := lookup(t, archive, "images")
	file := lookup(t, images, "0.txt")
	if file.item.member.file != nil || file.item.member.reader != nil {
		t.Fatal("inode retained the central-directory zip.File or reader")
	}
	if file.item.source == nil || file.item.archiveSize == 0 {
		t.Fatal("inode omitted source/version identity")
	}
}
