package mountfs

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/hanwen/go-fuse/v2/fs"
)

type directorySnapshotAPI struct {
	mu       sync.Mutex
	files    []panapi.File
	listHits int
}

func (a *directorySnapshotAPI) List(context.Context, int64) ([]panapi.File, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.listHits++
	return append([]panapi.File(nil), a.files...), nil
}
func (*directorySnapshotAPI) DownloadURL(context.Context, int64) (string, error) { return "", nil }
func (*directorySnapshotAPI) ReadSmallFile(context.Context, int64, int64) ([]byte, error) {
	return []byte("secret"), nil
}

func TestCloudDirectorySnapshotSharedWithPasswordLookup(t *testing.T) {
	api := &directorySnapshotAPI{files: []panapi.File{
		{ID: 10, ParentID: 0, Name: "private.rar", Size: 42, Version: "v1"},
		{ID: 11, ParentID: 0, Name: "private.rar.pwd", Size: 6, Version: "p1"},
	}}
	root := NewWithOptions(context.Background(), api, nil, 0, true, Options{})
	fs.NewNodeFS(root, &fs.Options{})
	archiveNode := lookup(t, root, "private.rar")
	password, err := root.tree.archivePassword(context.Background(), &archiveDescriptor{
		id: 10, parentID: 0, name: "private.rar", version: "v1", size: 42,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer clear(password)
	if string(password) != "secret" {
		t.Fatalf("password = %q", password)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.listHits != 1 {
		t.Fatalf("List calls after cloud lookup and password discovery = %d, want 1", api.listHits)
	}
	if archiveNode.item.cloud.ID != 10 {
		t.Fatalf("lookup returned cloud ID %d", archiveNode.item.cloud.ID)
	}
}

func TestExpiredCloudDirectorySnapshotRefreshes(t *testing.T) {
	api := &directorySnapshotAPI{files: []panapi.File{{ID: 1, Name: "before.txt"}}}
	root := NewWithOptions(context.Background(), api, nil, 0, true, Options{})
	if _, err := root.tree.cloudDirectory(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	api.files = append(api.files, panapi.File{ID: 2, Name: "after.txt"})
	api.mu.Unlock()
	root.tree.mu.Lock()
	root.tree.meta["dir:0"].expires = time.Now().Add(-time.Second)
	root.tree.mu.Unlock()
	// The expired snapshot remains usable while exactly one refresh runs.
	for i := 0; i < 2; i++ {
		if _, err := root.tree.cloudDirectory(context.Background(), 0); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		directory, err := root.tree.cloudDirectory(context.Background(), 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := directory.byName["after.txt"]; ok {
			api.mu.Lock()
			hits := api.listHits
			api.mu.Unlock()
			if hits != 2 {
				t.Fatalf("List calls after one expiry = %d, want 2", hits)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	api.mu.Lock()
	hits := api.listHits
	api.mu.Unlock()
	t.Fatalf("refreshed snapshot did not include after.txt; List calls=%d", hits)
}

func TestZIPLookupBuildsOnlyRequestedEntry(t *testing.T) {
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	for i := 0; i < 2000; i++ {
		name := fmt.Sprintf("dir/file-%04d.txt", i)
		if _, err := writer.Create(name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := writer.Create("target.txt"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	root := fixtureArchive(t, archive.Bytes())
	fs.NewNodeFS(root, &fs.Options{})
	packed := lookup(t, root, "photos.zip")
	entries, pending, err := packed.lookupEntries(context.Background(), "target.txt")
	if err != nil || pending {
		t.Fatalf("lookupEntries() = (%v, %t, %v)", entries, pending, err)
	}
	if len(entries) != 1 || entries["target.txt"] == nil {
		t.Fatalf("lookupEntries() returned %d entries, want only target.txt", len(entries))
	}
}

func TestArchivePasswordFindsNearestAncestorSharedPassword(t *testing.T) {
	api := &recursivePasswordAPI{lists: map[int64][]panapi.File{
		7: {{ID: 70, ParentID: 7, Name: "archive.7z", Size: 10, Version: "a"}},
		3: {{ID: 31, ParentID: 3, Name: ".mount123.pwd", Size: 6, Version: "shared-v1"}},
		0: {{ID: 1, ParentID: 0, Name: ".mount123.pwd", Size: 6, Version: "root-v1"}},
	}, details: map[int64]panapi.File{7: {ID: 7, ParentID: 3, IsDir: true}, 3: {ID: 3, ParentID: 0, IsDir: true}}}
	root := NewWithOptions(context.Background(), api, nil, 0, true, Options{})
	found, key, ferr := root.tree.findArchivePassword(context.Background(), &archiveDescriptor{id: 70, parentID: 7, name: "archive.7z", version: "a", size: 10})
	if ferr != nil || found == nil {
		t.Fatalf("found=%#v key=%q err=%v", found, key, ferr)
	}
	pw, err := root.tree.archivePassword(context.Background(), &archiveDescriptor{id: 70, parentID: 7, name: "archive.7z", version: "a", size: 10})
	if err != nil || string(pw) != "secret" {
		t.Fatalf("password=%q err=%v", pw, err)
	}
}

func TestArchivePasswordAcceptsLegacySharedPasswordName(t *testing.T) {
	api := &recursivePasswordAPI{lists: map[int64][]panapi.File{
		0: {{ID: 123, ParentID: 0, Name: ".123mount.pwd", Size: 6, Version: "shared-v1"}},
	}, details: map[int64]panapi.File{}}
	root := NewWithOptions(context.Background(), api, nil, 0, true, Options{})
	pw, err := root.tree.archivePassword(context.Background(), &archiveDescriptor{id: 70, parentID: 0, name: "archive.7z", version: "a", size: 10})
	if err != nil || string(pw) != "secret" {
		t.Fatalf("password=%q err=%v", pw, err)
	}
}

type recursivePasswordAPI struct {
	lists   map[int64][]panapi.File
	details map[int64]panapi.File
}

func (a *recursivePasswordAPI) List(_ context.Context, id int64) ([]panapi.File, error) {
	return append([]panapi.File(nil), a.lists[id]...), nil
}
func (a *recursivePasswordAPI) DownloadURL(context.Context, int64) (string, error) { return "", nil }
func (a *recursivePasswordAPI) ReadSmallFile(context.Context, int64, int64) ([]byte, error) {
	return []byte("secret\n"), nil
}
func (a *recursivePasswordAPI) Infos(context.Context, []int64) ([]panapi.File, error) {
	return nil, nil
}
func (a *recursivePasswordAPI) Detail(_ context.Context, id int64) (panapi.File, error) {
	return a.details[id], nil
}
