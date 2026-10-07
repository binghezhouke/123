package mountfs

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/hanwen/go-fuse/v2/fs"
)

type countingAPI struct {
	base     *fakeAPI
	lists    atomic.Int32
	urls     atomic.Int32
	urlDelay time.Duration
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
	if got := api.urls.Load(); got != 2 {
		t.Fatalf("source resolved %d times after TTL, want 2", got)
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
