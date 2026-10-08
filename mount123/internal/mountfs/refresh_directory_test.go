package mountfs

import (
	"context"
	"errors"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/hanwen/go-fuse/v2/fs"
)

type refreshFixtureAPI struct {
	mu          sync.Mutex
	files       map[int64][]panapi.File
	listHits    map[int64]int
	err         error
	block       <-chan struct{}
	started     chan struct{}
	blockParent int64
}

func (a *refreshFixtureAPI) List(ctx context.Context, parent int64) ([]panapi.File, error) {
	a.mu.Lock()
	block := a.block
	started := a.started
	shouldBlock := parent == a.blockParent
	a.listHits[parent]++
	err := a.err
	files := append([]panapi.File(nil), a.files[parent]...)
	a.mu.Unlock()
	if started != nil && shouldBlock {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if block != nil && shouldBlock {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-block:
		}
	}
	if err != nil {
		return nil, err
	}
	return files, nil
}

func (*refreshFixtureAPI) DownloadURL(context.Context, int64) (string, error) { return "", nil }

func (a *refreshFixtureAPI) ReadSmallFile(context.Context, int64, int64) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.files[0]) > 1 && a.files[0][1].ID == 12 {
		return []byte("new-secret"), nil
	}
	return []byte("old-secret"), nil
}

func TestRefreshDirectoryUpdatesOnlyTargetAndPreservesOldStream(t *testing.T) {
	api := &refreshFixtureAPI{files: map[int64][]panapi.File{
		0:  {{ID: 10, ParentID: 0, Name: "folder", IsDir: true}},
		10: {{ID: 1, ParentID: 10, Name: "before.txt", Size: 3, Version: "v1"}},
	}, listHits: map[int64]int{}}
	root := NewWithOptions(context.Background(), api, nil, 0, true, Options{})
	fs.NewNodeFS(root, &fs.Options{})
	folder := &Node{tree: root.tree, item: &entry{name: "folder", cloud: &panapi.File{ID: 10, Name: "folder", IsDir: true}, directory: true}, parent: root}
	oldStream, errno := folder.Readdir(context.Background())
	if errno != 0 {
		t.Fatal(errno)
	}
	api.mu.Lock()
	api.files[10] = []panapi.File{{ID: 2, ParentID: 10, Name: "after.txt", Size: 2, Version: "v2"}}
	api.mu.Unlock()
	result, err := root.RefreshDirectory(context.Background(), "folder")
	if err != nil {
		t.Fatal(err)
	}
	if result.Entries != 1 {
		t.Fatalf("RefreshDirectory entries=%d, want 1", result.Entries)
	}
	oldEntry, errno := oldStream.Next()
	if errno != 0 || oldEntry.Name != "before.txt" {
		t.Fatalf("old stream entry=%#v errno=%v", oldEntry, errno)
	}
	newEntries, err := folder.list(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(newEntries) != 1 || newEntries["after.txt"] == nil || newEntries["before.txt"] != nil {
		t.Fatalf("refreshed directory names = %#v", newEntries)
	}
	api.mu.Lock()
	hits := map[int64]int{0: api.listHits[0], 10: api.listHits[10]}
	api.mu.Unlock()
	if hits[0] != 1 || hits[10] != 2 {
		t.Fatalf("List calls root=%d target=%d, want 1 and 2", hits[0], hits[10])
	}
}

func TestRefreshDirectoryFailureKeepsPreviousSnapshot(t *testing.T) {
	api := &refreshFixtureAPI{files: map[int64][]panapi.File{
		0:  {{ID: 10, ParentID: 0, Name: "folder", IsDir: true}},
		10: {{ID: 1, ParentID: 10, Name: "stable.txt", Version: "v1"}},
	}, listHits: map[int64]int{}}
	root := NewWithOptions(context.Background(), api, nil, 0, true, Options{})
	folder := &Node{tree: root.tree, item: &entry{name: "folder", cloud: &panapi.File{ID: 10, Name: "folder", IsDir: true}, directory: true}, parent: root}
	if _, err := folder.list(context.Background()); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	api.err = errors.New("temporary API failure")
	api.mu.Unlock()
	if _, err := root.RefreshDirectory(context.Background(), "folder"); err == nil {
		t.Fatal("RefreshDirectory unexpectedly succeeded")
	}
	api.mu.Lock()
	api.err = nil
	api.mu.Unlock()
	entries, err := folder.list(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if entries["stable.txt"] == nil {
		t.Fatalf("failed refresh discarded old snapshot: %#v", entries)
	}
}

func TestRefreshDirectoryInvalidatesPasswordSidecarSnapshot(t *testing.T) {
	api := &refreshFixtureAPI{files: map[int64][]panapi.File{0: {
		{ID: 10, ParentID: 0, Name: "private.rar", Size: 50, Version: "archive-v1"},
		{ID: 11, ParentID: 0, Name: "private.rar.pwd", Size: 10, Version: "pwd-v1"},
	}}, listHits: map[int64]int{}}
	root := NewWithOptions(context.Background(), api, nil, 0, true, Options{})
	archive := &archiveDescriptor{id: 10, parentID: 0, name: "private.rar", size: 50, version: "archive-v1"}
	password, err := root.tree.archivePassword(context.Background(), archive)
	if err != nil || string(password) != "old-secret" {
		t.Fatalf("initial password=%q err=%v", password, err)
	}
	clear(password)
	api.mu.Lock()
	api.files[0][1].ID = 12
	api.files[0][1].Version = "pwd-v2"
	api.mu.Unlock()
	if _, err := root.RefreshDirectory(context.Background(), "."); err != nil {
		t.Fatal(err)
	}
	password, err = root.tree.archivePassword(context.Background(), archive)
	if err != nil || string(password) != "new-secret" {
		t.Fatalf("refreshed password=%q err=%v", password, err)
	}
	clear(password)
}

func TestPasswordSnapshotCompletesWhenDirectoryAndPasswordCannotCoexist(t *testing.T) {
	api := &refreshFixtureAPI{files: map[int64][]panapi.File{0: {
		{ID: 10, ParentID: 0, Name: "private.rar", Size: 50, Version: "archive-v1"},
		{ID: 11, ParentID: 0, Name: "private.rar.pwd", Size: 10, Version: "pwd-v1"},
	}}, listHits: map[int64]int{}}
	// Each snapshot fits on its own, but storing a password evicts its parent
	// listing. Generation isolation must not require both to remain resident.
	root := NewWithOptions(context.Background(), api, nil, 0, true, Options{MetadataBytes: 4600, MaxConcurrentBuilds: 1})
	archive := &archiveDescriptor{id: 10, parentID: 0, name: "private.rar", size: 50, version: "archive-v1"}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for i := 0; i < 2; i++ {
		password, err := root.tree.archivePassword(ctx, archive)
		if err != nil || string(password) != "old-secret" {
			t.Fatalf("password request %d failed under LRU pressure: %v", i, err)
		}
		clear(password)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.listHits[0] > 2 {
		t.Fatalf("directory/password eviction triggered repeated reloads: %d", api.listHits[0])
	}
}

func TestRefreshDirectoryRejectsNonCloudAndTraversal(t *testing.T) {
	api := &refreshFixtureAPI{files: map[int64][]panapi.File{0: {
		{ID: 1, Name: "archive.zip", Size: 1, Version: "v1"},
		{ID: 2, Name: "plain.txt", Size: 1, Version: "v1"},
	}}, listHits: map[int64]int{}}
	root := NewWithOptions(context.Background(), api, nil, 0, true, Options{})
	for _, tc := range []struct {
		path string
		want error
	}{{"archive.zip", syscall.EINVAL}, {"plain.txt", syscall.ENOTDIR}, {"../outside", syscall.EINVAL}} {
		_, err := root.RefreshDirectory(context.Background(), tc.path)
		if !errors.Is(err, tc.want) {
			t.Errorf("RefreshDirectory(%q) error=%v, want %v", tc.path, err, tc.want)
		}
	}
}

func TestManualRefreshSerializesWithColdDirectoryLoad(t *testing.T) {
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	api := &refreshFixtureAPI{files: map[int64][]panapi.File{
		0:  {{ID: 10, ParentID: 0, Name: "folder", IsDir: true}},
		10: {{ID: 1, ParentID: 10, Name: "before.txt", Version: "v1"}},
	}, listHits: map[int64]int{}, started: started, block: release}
	root := NewWithOptions(context.Background(), api, nil, 0, true, Options{})
	// Load the parent listing without blocking, then make the target list block.
	api.block = nil
	if _, err := root.tree.cloudDirectory(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	api.block = release
	api.blockParent = 10
	folderNode := &Node{tree: root.tree, item: &entry{name: "folder", cloud: &panapi.File{ID: 10, IsDir: true}, directory: true}, parent: root}
	loadDone := make(chan error, 1)
	go func() { _, err := folderNode.list(context.Background()); loadDone <- err }()
	<-started
	refreshDone := make(chan error, 1)
	go func() { _, err := root.RefreshDirectory(context.Background(), "folder"); refreshDone <- err }()
	select {
	case <-refreshDone:
		t.Fatal("manual refresh did not wait for the active listing flight")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-loadDone; err != nil {
		t.Fatal(err)
	}
	if err := <-refreshDone; err != nil {
		t.Fatal(err)
	}
}
