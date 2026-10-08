package mountfs

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/faults"
	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/hanwen/go-fuse/v2/fs"
)

type durableDirectoryAPI struct {
	mu      sync.Mutex
	account string
	files   []panapi.File
	err     error
	calls   int
}

func (a *durableDirectoryAPI) CacheIdentity() string { return a.account }
func (a *durableDirectoryAPI) List(context.Context, int64) ([]panapi.File, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	return append([]panapi.File(nil), a.files...), a.err
}
func (*durableDirectoryAPI) DownloadURL(context.Context, int64) (string, error) { return "", nil }

func (a *durableDirectoryAPI) setError(err error) {
	a.mu.Lock()
	a.err = err
	a.mu.Unlock()
}
func (a *durableDirectoryAPI) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

func openDirectoryCache(t *testing.T, dir string) *storage.Cache {
	t.Helper()
	cache, err := storage.NewCache(dir, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	return cache
}

func TestCloudDirectorySnapshotRestoresAfterCacheReopen(t *testing.T) {
	dir := t.TempDir()
	api := &durableDirectoryAPI{account: "account-a", files: []panapi.File{
		{ID: 2, Name: "same.txt"}, {ID: 1, Name: "same.txt"},
		{ID: 3, Name: "same [id=2].txt"}, {ID: 4, Name: "secret.rar.pwd"},
	}}
	cache := openDirectoryCache(t, dir)
	root := NewWithOptions(context.Background(), api, cache, 0, true, Options{DirectoryTTL: time.Hour})
	fs.NewNodeFS(root, &fs.Options{})
	got := cloudNames(t, root)
	if len(got) != 4 || got["same.txt"] != 1 || got["same [id=2-2].txt"] != 2 || got["same [id=2].txt"] != 3 {
		t.Fatalf("mounted names = %#v", got)
	}
	if api.callCount() != 1 {
		t.Fatalf("initial List calls = %d", api.callCount())
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}

	api.setError(errors.New("offline"))
	cache = openDirectoryCache(t, dir)
	defer cache.Close()
	restarted := NewWithOptions(context.Background(), api, cache, 0, true, Options{DirectoryTTL: time.Hour})
	fs.NewNodeFS(restarted, &fs.Options{})
	got = cloudNames(t, restarted)
	if len(got) != 4 || got["same.txt"] != 1 || got["same [id=2-2].txt"] != 2 {
		t.Fatalf("restored names = %#v", got)
	}
	if api.callCount() != 1 {
		t.Fatalf("fresh disk snapshot made another List call: %d", api.callCount())
	}
	if stats := restarted.DirectoryStats(); stats.DiskRestores != 1 || stats.ListCalls != 0 {
		t.Fatalf("directory stats after restore = %+v", stats)
	}
}

func TestExpiredDirectoryDiskSnapshotKeepsOriginalAge(t *testing.T) {
	dir := t.TempDir()
	api := &durableDirectoryAPI{account: "account-age", files: []panapi.File{{ID: 1, Name: "old.txt"}}}
	cache := openDirectoryCache(t, dir)
	root := NewWithOptions(context.Background(), api, cache, 0, true, Options{DirectoryTTL: 35 * time.Millisecond})
	if _, err := root.tree.cloudDirectory(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(55 * time.Millisecond)
	api.mu.Lock()
	api.files = []panapi.File{{ID: 2, Name: "new.txt"}}
	api.mu.Unlock()
	cache = openDirectoryCache(t, dir)
	defer cache.Close()
	restarted := NewWithOptions(context.Background(), api, cache, 0, true, Options{DirectoryTTL: 35 * time.Millisecond})
	directory, err := restarted.tree.cloudDirectory(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if directory.byName["old.txt"].ID != 1 {
		t.Fatal("expired snapshot did not serve the saved listing")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		restarted.tree.mu.Lock()
		_, exists := restarted.tree.meta["dir:0"]
		pending := len(restarted.tree.refreshing) != 0
		restarted.tree.mu.Unlock()
		if exists && !pending && api.callCount() > 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if api.callCount() != 2 {
		t.Fatalf("expired restore List calls = %d, want 2 total", api.callCount())
	}
	refreshed, err := restarted.tree.cloudDirectory(context.Background(), 0)
	if err != nil || refreshed.byName["new.txt"].ID != 2 {
		t.Fatalf("refreshed disk snapshot = %#v, err=%v", refreshed, err)
	}
}

func TestDirectorySnapshotsAreAccountScoped(t *testing.T) {
	dir := t.TempDir()
	cache := openDirectoryCache(t, dir)
	apiA := &durableDirectoryAPI{account: "account-a", files: []panapi.File{{ID: 1, Name: "a.txt"}}}
	rootA := NewWithOptions(context.Background(), apiA, cache, 0, true, Options{})
	if _, err := rootA.tree.cloudDirectory(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	apiB := &durableDirectoryAPI{account: "account-b", files: []panapi.File{{ID: 2, Name: "b.txt"}}}
	rootB := NewWithOptions(context.Background(), apiB, cache, 0, true, Options{})
	directory, err := rootB.tree.cloudDirectory(context.Background(), 0)
	if err != nil || directory.byName["b.txt"].ID != 2 || apiB.callCount() != 1 {
		t.Fatalf("account B directory = %#v, calls=%d, err=%v", directory, apiB.callCount(), err)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestManualDirectoryRefreshPersistsReplacement(t *testing.T) {
	dir := t.TempDir()
	api := &durableDirectoryAPI{account: "manual-refresh", files: []panapi.File{{ID: 1, Name: "before.txt"}}}
	cache := openDirectoryCache(t, dir)
	root := NewWithOptions(context.Background(), api, cache, 0, true, Options{DirectoryTTL: time.Hour})
	if _, err := root.tree.cloudDirectory(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	api.files = []panapi.File{{ID: 2, Name: "after.txt"}}
	api.mu.Unlock()
	if result, err := root.RefreshDirectory(context.Background(), "."); err != nil || result.Entries != 1 {
		t.Fatalf("manual refresh = %+v, err=%v", result, err)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	api.setError(errors.New("offline"))
	cache = openDirectoryCache(t, dir)
	defer cache.Close()
	restarted := NewWithOptions(context.Background(), api, cache, 0, true, Options{DirectoryTTL: time.Hour})
	directory, err := restarted.tree.cloudDirectory(context.Background(), 0)
	if err != nil || directory.byName["after.txt"].ID != 2 {
		t.Fatalf("manual refresh snapshot = %#v, err=%v", directory, err)
	}
	if api.callCount() != 2 {
		t.Fatalf("manual refresh reopen List calls = %d, want 2 total", api.callCount())
	}
}

func TestManualRefreshKeepsWorkingWhenDiskAdmissionFails(t *testing.T) {
	dir := t.TempDir()
	const capacity = 1 << 20
	cache, err := storage.NewCache(dir, capacity)
	if err != nil {
		t.Fatal(err)
	}
	api := &durableDirectoryAPI{account: "manual-capacity", files: []panapi.File{{ID: 1, Name: "before.txt"}}}
	root := NewWithOptions(context.Background(), api, cache, 0, true, Options{DirectoryTTL: time.Hour})
	if _, err := root.tree.cloudDirectory(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	free := capacity - cache.Stats().UsedBytes
	if free <= 0 {
		t.Fatal("directory snapshot unexpectedly filled the test cache")
	}
	if err := cache.Store(context.Background(), "pinned-filler", make([]byte, free)); err != nil {
		t.Fatal(err)
	}
	filler, err := cache.Open("pinned-filler")
	if err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	api.files = []panapi.File{{ID: 2, Name: "after.txt"}}
	api.mu.Unlock()
	if _, err := root.RefreshDirectory(context.Background(), "."); err != nil {
		t.Fatalf("manual refresh failed when disk admission was full: %v", err)
	}
	memory, err := root.tree.cloudDirectory(context.Background(), 0)
	if err != nil || memory.byName["after.txt"].ID != 2 {
		t.Fatalf("manual refresh memory snapshot = %#v, err=%v", memory, err)
	}
	if stats := root.DirectoryStats(); stats.PersistenceFailures != 1 {
		t.Fatalf("persistence failure stats = %+v", stats)
	}
	_ = filler.Close()
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}

	api.setError(errors.New("offline"))
	cache = openDirectoryCache(t, dir)
	defer cache.Close()
	restarted := NewWithOptions(context.Background(), api, cache, 0, true, Options{DirectoryTTL: time.Hour})
	disk, err := restarted.tree.cloudDirectory(context.Background(), 0)
	if err != nil || disk.byName["before.txt"].ID != 1 {
		t.Fatalf("previous disk snapshot after failed admission = %#v, err=%v", disk, err)
	}
}

func TestCorruptDirectorySnapshotFallsBackToList(t *testing.T) {
	dir := t.TempDir()
	api := &durableDirectoryAPI{account: "corrupt", files: []panapi.File{{ID: 1, Name: "from-api.txt"}}}
	cache := openDirectoryCache(t, dir)
	root := NewWithOptions(context.Background(), api, cache, 0, true, Options{})
	if _, err := root.tree.cloudDirectory(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if err := cache.ReplaceArchiveIndex(context.Background(), root.tree.directorySnapshotKey(0), []byte("broken snapshot")); err != nil {
		t.Fatal(err)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	cache = openDirectoryCache(t, dir)
	defer cache.Close()
	restarted := NewWithOptions(context.Background(), api, cache, 0, true, Options{})
	directory, err := restarted.tree.cloudDirectory(context.Background(), 0)
	if err != nil || directory.byName["from-api.txt"].ID != 1 {
		t.Fatalf("corrupt snapshot fallback = %#v, err=%v", directory, err)
	}
	if api.callCount() != 2 || restarted.DirectoryStats().CorruptSnapshots != 1 {
		t.Fatalf("corrupt fallback calls=%d stats=%+v", api.callCount(), restarted.DirectoryStats())
	}
}

func TestTransientDirectoryRefreshServesStaleWithBackoff(t *testing.T) {
	api := &durableDirectoryAPI{account: "transient", files: []panapi.File{{ID: 1, Name: "known.txt"}}}
	root := NewWithOptions(context.Background(), api, nil, 0, true, Options{})
	if _, err := root.tree.cloudDirectory(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	api.setError(&faults.Error{Kind: faults.Network, Message: "temporary", Retryable: true})
	root.tree.mu.Lock()
	root.tree.meta["dir:0"].expires = time.Now().Add(-time.Second)
	root.tree.mu.Unlock()
	directory, err := root.tree.cloudDirectory(context.Background(), 0)
	if err != nil || directory.byName["known.txt"].ID != 1 {
		t.Fatalf("stale listing = %#v, err=%v", directory, err)
	}
	waitDirectoryRefresh(t, root)
	for i := 0; i < 4; i++ {
		directory, err = root.tree.cloudDirectory(context.Background(), 0)
		if err != nil || directory.byName["known.txt"].ID != 1 {
			t.Fatalf("stale retry listing = %#v, err=%v", directory, err)
		}
	}
	if got := api.callCount(); got != 2 {
		t.Fatalf("transient refresh made %d List calls, want 2", got)
	}
	if stats := root.DirectoryStats(); stats.StaleFailures == 0 || stats.RetryBackoffs == 0 {
		t.Fatalf("transient directory stats = %+v", stats)
	}
}

func TestPermanentDirectoryRefreshDropsStaleDiskSnapshot(t *testing.T) {
	dir := t.TempDir()
	cache := openDirectoryCache(t, dir)
	api := &durableDirectoryAPI{account: "permanent", files: []panapi.File{{ID: 1, Name: "known.txt"}}}
	root := NewWithOptions(context.Background(), api, cache, 0, true, Options{})
	if _, err := root.tree.cloudDirectory(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	api.setError(&faults.Error{Kind: faults.Unauthorized, Message: "unauthorized"})
	root.tree.mu.Lock()
	root.tree.meta["dir:0"].expires = time.Now().Add(-time.Second)
	root.tree.mu.Unlock()
	if _, err := root.tree.cloudDirectory(context.Background(), 0); err != nil {
		t.Fatal("stale snapshot should serve while refresh runs:", err)
	}
	waitDirectoryRefresh(t, root)
	root.tree.mu.Lock()
	_, retained := root.tree.meta["dir:0"]
	root.tree.mu.Unlock()
	if retained {
		t.Fatal("permanent refresh failure retained stale memory metadata")
	}
	if _, err := cache.OpenArchiveIndex(root.tree.directorySnapshotKey(0)); err == nil {
		t.Fatal("permanent refresh failure retained stale disk metadata")
	}
	if _, err := root.tree.cloudDirectory(context.Background(), 0); err == nil {
		t.Fatal("cold lookup hid permanent authorization failure")
	}
	if api.callCount() != 3 {
		t.Fatalf("permanent failure List calls = %d, want 3", api.callCount())
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
}

func waitDirectoryRefresh(t *testing.T, root *Node) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		root.tree.mu.Lock()
		pending := len(root.tree.refreshing) != 0
		root.tree.mu.Unlock()
		if !pending {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("directory refresh did not finish")
}
