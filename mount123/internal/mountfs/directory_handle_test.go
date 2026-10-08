package mountfs

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type snapshotMetadataAPI struct {
	mu       sync.Mutex
	files    []panapi.File
	listHits int
	infoHits int
}

func (a *snapshotMetadataAPI) List(context.Context, int64) ([]panapi.File, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.listHits++
	return append([]panapi.File(nil), a.files...), nil
}
func (*snapshotMetadataAPI) DownloadURL(context.Context, int64) (string, error) { return "", nil }
func (*snapshotMetadataAPI) Detail(context.Context, int64) (panapi.File, error) {
	return panapi.File{ID: 1, IsDir: true}, nil
}
func (a *snapshotMetadataAPI) Infos(_ context.Context, ids []int64) ([]panapi.File, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.infoHits++
	byID := make(map[int64]panapi.File, len(a.files))
	for _, f := range a.files {
		byID[f.ID] = f
	}
	out := make([]panapi.File, 0, len(ids))
	for _, id := range ids {
		f := byID[id]
		f.UpdatedAt = time.Unix(200, 0)
		out = append(out, f)
	}
	return out, nil
}

func TestDirectoryHandleUsesFrozenCloudSnapshotAndAttrs(t *testing.T) {
	api := &snapshotMetadataAPI{files: []panapi.File{{
		ID: 10, Name: "before.txt", Size: 9, Version: "v1", UpdatedAt: time.Unix(100, 0),
	}}}
	root := NewWithOptions(context.Background(), api, nil, 0, true, Options{RefreshFileMetadata: true})
	fs.NewNodeFS(root, &fs.Options{})
	h, _, errno := root.OpendirHandle(context.Background(), 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	defer h.(fs.FileReleasedirer).Releasedir(context.Background(), 0)
	dir := h.(fs.FileReaddirenter)
	lookuper := h.(fs.FileLookuper)
	first, errno := dir.Readdirent(context.Background())
	if errno != 0 || first == nil || first.Name != "before.txt" {
		t.Fatalf("first entry = %#v, errno=%v", first, errno)
	}
	var frozen fuse.EntryOut
	fromHandle, errno := lookuper.Lookup(context.Background(), first.Name, &frozen)
	if errno != 0 {
		t.Fatal(errno)
	}
	if frozen.Attr.Mtime != 100 {
		t.Fatalf("snapshot mtime=%d, want 100", frozen.Attr.Mtime)
	}
	api.mu.Lock()
	if api.infoHits != 0 {
		t.Fatalf("directory-handle lookup called Infos %d times", api.infoHits)
	}
	api.mu.Unlock()

	// The handle's name and attribute view remains frozen even after metadata
	// refresh. A fresh direct lookup still shares its stable inode.
	var refreshed fuse.EntryOut
	fromNode, errno := root.Lookup(context.Background(), "before.txt", &refreshed)
	if errno != 0 {
		t.Fatal(errno)
	}
	if fromNode.StableAttr().Ino != fromHandle.StableAttr().Ino {
		t.Fatalf("inode changed across direct lookup: %d != %d", fromNode.StableAttr().Ino, fromHandle.StableAttr().Ino)
	}
	if refreshed.Attr.Mtime != 200 || frozen.Attr.Mtime != 100 {
		t.Fatalf("frozen/refreshed mtimes = %d/%d, want 100/200", frozen.Attr.Mtime, refreshed.Attr.Mtime)
	}
	if fromNode.StableAttr() != fromHandle.StableAttr() {
		t.Fatalf("direct and directory-handle StableAttr differs: %#v != %#v", fromNode.StableAttr(), fromHandle.StableAttr())
	}
	if errno := h.(fs.FileSeekdirer).Seekdir(context.Background(), 0); errno != 0 {
		t.Fatal(errno)
	}
	if got, errno := dir.Readdirent(context.Background()); errno != 0 || got == nil || got.Name != "before.txt" {
		t.Fatalf("rewound entry=%#v errno=%v", got, errno)
	}
	if got, errno := dir.Readdirent(context.Background()); errno != 0 || got != nil {
		t.Fatalf("old handle exposed refreshed entry: %#v errno=%v", got, errno)
	}

	api.mu.Lock()
	api.files[0].Version = "v2"
	api.files[0].UpdatedAt = time.Unix(300, 0)
	api.files = append(api.files, panapi.File{ID: 11, Name: "after.txt", Size: 4, Version: "v1"})
	api.mu.Unlock()
	root.tree.mu.Lock()
	root.tree.meta["dir:0"].expires = time.Now().Add(-time.Second)
	root.tree.mu.Unlock()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		fresh, err := root.tree.cloudDirectory(context.Background(), 0)
		if err == nil {
			if _, ok := fresh.entries["after.txt"]; ok {
				break
			}
		}
		time.Sleep(time.Millisecond)
	}
	newHandle, _, errno := root.OpendirHandle(context.Background(), 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	defer newHandle.(fs.FileReleasedirer).Releasedir(context.Background(), 0)
	newDir := newHandle.(fs.FileReaddirenter)
	firstNew, errno := newDir.Readdirent(context.Background())
	if errno != 0 || firstNew == nil || firstNew.Name != "after.txt" {
		t.Fatalf("new handle first entry = %#v errno=%v", firstNew, errno)
	}
	if errno := newHandle.(fs.FileSeekdirer).Seekdir(context.Background(), 1); errno != 0 {
		t.Fatal(errno)
	}
	updated, errno := newDir.Readdirent(context.Background())
	if errno != 0 || updated == nil || updated.Name != "before.txt" {
		t.Fatalf("new snapshot first entry=%#v errno=%v", updated, errno)
	}
	var updatedAttr fuse.EntryOut
	updatedNode, errno := newHandle.(fs.FileLookuper).Lookup(context.Background(), updated.Name, &updatedAttr)
	if errno != 0 || updatedAttr.Mtime != 300 {
		t.Fatalf("new version attr mtime=%d errno=%v", updatedAttr.Mtime, errno)
	}
	if updatedNode.StableAttr().Ino == fromHandle.StableAttr().Ino {
		t.Fatal("new version reused the old snapshot inode")
	}
}

func TestDirectoryHandleLookupCanRaceWithRelease(t *testing.T) {
	api := &directorySnapshotAPI{files: []panapi.File{{ID: 10, Name: "one.txt", Size: 1, Version: "v1"}}}
	root := New(context.Background(), api, nil, 0, true)
	fs.NewNodeFS(root, &fs.Options{})
	h, _, errno := root.OpendirHandle(context.Background(), 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	de := h.(fs.FileReaddirenter)
	first, errno := de.Readdirent(context.Background())
	if errno != 0 || first == nil {
		t.Fatalf("first entry=%#v errno=%v", first, errno)
	}
	start := make(chan struct{})
	lookupDone := make(chan syscall.Errno, 1)
	releaseDone := make(chan struct{})
	go func() {
		<-start
		_, lookupErrno := h.(fs.FileLookuper).Lookup(context.Background(), first.Name, &fuse.EntryOut{})
		lookupDone <- lookupErrno
	}()
	go func() {
		<-start
		h.(fs.FileReleasedirer).Releasedir(context.Background(), 0)
		close(releaseDone)
	}()
	close(start)
	lookupErrno := <-lookupDone
	<-releaseDone
	if lookupErrno != 0 && lookupErrno != syscall.EBADF {
		t.Fatalf("concurrent lookup errno=%v", lookupErrno)
	}
	if _, errno := h.(fs.FileLookuper).Lookup(context.Background(), first.Name, &fuse.EntryOut{}); errno != syscall.EBADF {
		t.Fatalf("lookup after release errno=%v, want EBADF", errno)
	}
}

func TestDirectoryHandleZIPReadDirPlusUsesIndexSnapshot(t *testing.T) {
	var data bytes.Buffer
	w := zip.NewWriter(&data)
	for _, name := range []string{"images/a.txt", "images/b.txt"} {
		if _, err := w.Create(name); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	root := fixtureArchive(t, data.Bytes())
	fs.NewNodeFS(root, &fs.Options{})
	archive := lookup(t, root, "photos.zip")
	h, _, errno := archive.OpendirHandle(context.Background(), 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	defer h.(fs.FileReleasedirer).Releasedir(context.Background(), 0)
	first, errno := h.(fs.FileReaddirenter).Readdirent(context.Background())
	if errno != 0 || first == nil || first.Name != "images" || first.Mode != fuse.S_IFDIR {
		t.Fatalf("ZIP entry = %#v errno=%v", first, errno)
	}
	var out fuse.EntryOut
	child, errno := h.(fs.FileLookuper).Lookup(context.Background(), first.Name, &out)
	if errno != 0 || out.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		t.Fatalf("ZIP READDIRPLUS attr mode=%o errno=%v", out.Mode, errno)
	}
	fromNode, errno := archive.Lookup(context.Background(), first.Name, &fuse.EntryOut{})
	if errno != 0 || child.StableAttr().Ino != fromNode.StableAttr().Ino {
		t.Fatalf("ZIP inode not reused: handle=%v node=%v errno=%v", child, fromNode, errno)
	}
}

func TestDirectoryHandleLookupCountsAtScale(t *testing.T) {
	for _, count := range []int{1000, 10000} {
		t.Run(fmt.Sprintf("entries-%d", count), func(t *testing.T) {
			api := &snapshotMetadataAPI{files: makeDirectoryFiles(count)}
			root := NewWithOptions(context.Background(), api, nil, 0, true, Options{RefreshFileMetadata: true})
			fs.NewNodeFS(root, &fs.Options{})
			h, _, errno := root.OpendirHandle(context.Background(), 0)
			if errno != 0 {
				t.Fatal(errno)
			}
			defer h.(fs.FileReleasedirer).Releasedir(context.Background(), 0)
			dir := h.(fs.FileReaddirenter)
			lookuper := h.(fs.FileLookuper)
			for i := 0; i < count; i++ {
				de, errno := dir.Readdirent(context.Background())
				if errno != 0 || de == nil {
					t.Fatalf("entry %d = %#v errno=%v", i, de, errno)
				}
				if _, errno := lookuper.Lookup(context.Background(), de.Name, &fuse.EntryOut{}); errno != 0 {
					t.Fatalf("entry %d lookup errno=%v", i, errno)
				}
			}
			if de, errno := dir.Readdirent(context.Background()); errno != 0 || de != nil {
				t.Fatalf("unexpected trailing entry=%#v errno=%v", de, errno)
			}
			api.mu.Lock()
			defer api.mu.Unlock()
			if api.listHits != 1 || api.infoHits != 0 {
				t.Fatalf("1 listing produced List=%d Infos=%d for %d entries; want List=1 Infos=0", api.listHits, api.infoHits, count)
			}
		})
	}
}

func TestProgressiveDirectoryHandleFreezesDiscoveredEntries(t *testing.T) {
	root := New(context.Background(), &directorySnapshotAPI{}, nil, 0, true)
	fs.NewNodeFS(root, &fs.Options{})
	archive := root
	dir := &zipDir{order: []string{"one.txt"}, dirs: map[string]*zipDir{}, files: map[string]*member{
		"one.txt": {name: "one.txt", size: 7},
	}}
	idx := &zipIndex{root: dir}
	desc := &archiveDescriptor{name: "sample.rar", size: 100}
	idx.mu.RLock()
	names := dir.order
	entries := map[string]*entry{"one.txt": indexEntry(dir, "", "one.txt", nil, desc)}
	idx.mu.RUnlock()
	snapshot := &frozenDirectorySnapshot{entries: entries}
	h := newFrozenDirectoryHandle(archive, snapshot, names, nil)
	defer h.Releasedir(context.Background(), 0)

	// Simulate the background scanner discovering another entry after open.
	idx.mu.Lock()
	dir.order = append(dir.order, "two.txt")
	dir.files["two.txt"] = &member{name: "two.txt", size: 9}
	idx.mu.Unlock()

	first, errno := h.Readdirent(context.Background())
	if errno != 0 || first == nil || first.Name != "one.txt" {
		t.Fatalf("frozen first entry=%#v errno=%v", first, errno)
	}
	var attr fuse.EntryOut
	child, errno := h.Lookup(context.Background(), first.Name, &attr)
	if errno != 0 || attr.Size != 7 || child.StableAttr().Ino == 0 {
		t.Fatalf("frozen lookup attr=%#v inode=%v errno=%v", attr.Attr, child, errno)
	}
	if next, errno := h.Readdirent(context.Background()); errno != 0 || next != nil {
		t.Fatalf("old handle exposed later RAR entry %#v errno=%v", next, errno)
	}

	idx.mu.RLock()
	names = append([]string(nil), dir.order...)
	entries = map[string]*entry{
		"one.txt": indexEntry(dir, "", "one.txt", nil, desc),
		"two.txt": indexEntry(dir, "", "two.txt", nil, desc),
	}
	idx.mu.RUnlock()
	newHandle := newFrozenDirectoryHandle(archive, &frozenDirectorySnapshot{entries: entries}, names, nil)
	newFirst, errno := newHandle.Readdirent(context.Background())
	if errno != 0 || newFirst == nil || newFirst.Name != "one.txt" {
		t.Fatalf("new handle first entry=%#v errno=%v", newFirst, errno)
	}
	newSecond, errno := newHandle.Readdirent(context.Background())
	if errno != 0 || newSecond == nil || newSecond.Name != "two.txt" {
		t.Fatalf("new handle missed discovered RAR entry=%#v errno=%v", newSecond, errno)
	}
	newHandle.Releasedir(context.Background(), 0)
}

func TestDirectoryHandlePinsAreSharedAndBudgetedAcrossRefreshes(t *testing.T) {
	api := &directorySnapshotAPI{files: makeDirectoryFiles(5)}
	root := NewWithOptions(context.Background(), api, nil, 0, true, Options{MetadataBytes: 5000})
	current, err := root.tree.cloudDirectory(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	firstRelease, err := root.tree.pinDirectorySnapshot(current, current.bytes)
	if err != nil {
		t.Fatal(err)
	}
	secondRelease, err := root.tree.pinDirectorySnapshot(current, current.bytes)
	if err != nil {
		t.Fatalf("same shared snapshot was charged twice: %v", err)
	}

	var releases []func()
	for generation := 0; generation < 2; generation++ {
		root.tree.mu.Lock()
		root.tree.meta["dir:0"].expires = time.Now().Add(-time.Second)
		root.tree.mu.Unlock()
		deadline := time.Now().Add(2 * time.Second)
		var refreshed *cloudDirectory
		for time.Now().Before(deadline) {
			refreshed, err = root.tree.cloudDirectory(context.Background(), 0)
			if err == nil && refreshed != current {
				break
			}
			time.Sleep(time.Millisecond)
		}
		if refreshed == nil || refreshed == current {
			t.Fatal("directory refresh did not publish a new snapshot")
		}
		release, pinErr := root.tree.pinDirectorySnapshot(refreshed, refreshed.bytes)
		if generation == 0 {
			if pinErr != nil {
				t.Fatal(pinErr)
			}
			releases = append(releases, release)
		} else if pinErr == nil {
			release()
			t.Fatal("third retained snapshot exceeded the directory pin budget")
		}
		current = refreshed
	}

	firstRelease()
	secondRelease()
	for _, release := range releases {
		release()
	}
	root.tree.mu.Lock()
	root.tree.meta["dir:0"].expires = time.Now().Add(-time.Second)
	root.tree.mu.Unlock()
	refreshed, err := root.tree.cloudDirectory(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if release, err := root.tree.pinDirectorySnapshot(refreshed, refreshed.bytes); err != nil {
		t.Fatalf("released handles kept snapshots pinned: %v", err)
	} else {
		release()
	}
}

func TestCloudParentHandleCanOpenProgressiveRARChildWithinBudget(t *testing.T) {
	data := sparseRARFixture()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"directory-pin-rar"`)
		http.ServeContent(w, r, "archive.rar", time.Unix(1, 0), io.NewSectionReader(data, 0, data.size))
	}))
	defer server.Close()
	cache, err := storage.NewCache(t.TempDir(), 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api := &archiveAPI{url: server.URL, files: []panapi.File{{ID: 77, Name: "archive.rar", Size: data.size, Version: "v1"}}}
	root := NewWithOptions(ctx, api, cache, 0, true, Options{MetadataBytes: 16 << 10})
	fs.NewNodeFS(root, &fs.Options{})
	parentHandle, _, errno := root.OpendirHandle(ctx, 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	defer parentHandle.(fs.FileReleasedirer).Releasedir(ctx, 0)
	de := parentHandle.(fs.FileReaddirenter)
	archiveEntry, errno := de.Readdirent(ctx)
	if errno != 0 || archiveEntry == nil || archiveEntry.Name != "archive.rar" {
		t.Fatalf("cloud parent entry=%#v errno=%v", archiveEntry, errno)
	}
	archiveInode, errno := parentHandle.(fs.FileLookuper).Lookup(ctx, archiveEntry.Name, &fuse.EntryOut{})
	if errno != 0 {
		t.Fatal(errno)
	}
	archiveNode := archiveInode.Operations().(*Node)
	rarHandle, _, errno := archiveNode.OpendirHandle(ctx, 0)
	if errno != 0 {
		t.Fatalf("open RAR child while parent snapshot held: %v", errno)
	}
	defer rarHandle.(fs.FileReleasedirer).Releasedir(ctx, 0)
	firstRAR, errno := rarHandle.(fs.FileReaddirenter).Readdirent(ctx)
	if errno != 0 || firstRAR == nil || firstRAR.Name != "0000.jpg" {
		t.Fatalf("RAR first entry=%#v errno=%v", firstRAR, errno)
	}
}

func BenchmarkDirectorySnapshotLookup(b *testing.B) {
	for _, size := range []int{1000, 10000} {
		b.Run(fmt.Sprintf("handle/%d", size), func(b *testing.B) {
			api := &snapshotMetadataAPI{files: makeDirectoryFiles(size)}
			root := New(context.Background(), api, nil, 0, true)
			fs.NewNodeFS(root, &fs.Options{})
			entries, err := root.tree.cloudDirectory(context.Background(), 0)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				h := newDirectoryHandle(root, entries.entries, entries.names, nil)
				for {
					de, errno := h.Readdirent(context.Background())
					if errno != 0 {
						b.Fatal(errno)
					}
					if de == nil {
						break
					}
					if _, errno := h.Lookup(context.Background(), de.Name, &fuse.EntryOut{}); errno != 0 {
						b.Fatal(errno)
					}
				}
			}
		})
		b.Run(fmt.Sprintf("node-lookup/%d", size), func(b *testing.B) {
			api := &snapshotMetadataAPI{files: makeDirectoryFiles(size)}
			root := New(context.Background(), api, nil, 0, true)
			fs.NewNodeFS(root, &fs.Options{})
			entries, err := root.tree.cloudDirectory(context.Background(), 0)
			if err != nil {
				b.Fatal(err)
			}
			names := make([]string, 0, size)
			for name := range entries.entries {
				names = append(names, name)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for _, name := range names {
					if _, errno := root.Lookup(context.Background(), name, &fuse.EntryOut{}); errno != 0 {
						b.Fatal(errno)
					}
				}
			}
		})
	}
}

func makeDirectoryFiles(count int) []panapi.File {
	files := make([]panapi.File, count)
	for i := range files {
		files[i] = panapi.File{ID: int64(i + 1), Name: fmt.Sprintf("file-%05d.txt", i), Size: 1, Version: "v1"}
	}
	return files
}
