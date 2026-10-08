package mountfs

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type pageCacheAPI struct {
	mu      sync.RWMutex
	url     string
	data    []byte
	version string
}

func (a *pageCacheAPI) CacheIdentity() string { return "page-cache-test-account" }
func (a *pageCacheAPI) List(_ context.Context, id int64) ([]panapi.File, error) {
	if id != 0 {
		return nil, nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return []panapi.File{{ID: 77, Name: "archive.zip", Size: int64(len(a.data)), Version: a.version}}, nil
}
func (a *pageCacheAPI) DownloadURL(context.Context, int64) (string, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.url, nil
}
func (a *pageCacheAPI) update(data []byte, version string) {
	a.mu.Lock()
	a.data, a.version = append([]byte(nil), data...), version
	a.mu.Unlock()
}
func (a *pageCacheAPI) snapshot() ([]byte, string) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return append([]byte(nil), a.data...), a.version
}

func pageCacheZIP(t *testing.T, value byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	entry, err := w.CreateHeader(&zip.FileHeader{Name: "payload.bin", Method: zip.Deflate})
	if err != nil {
		t.Fatal(err)
	}
	content := bytes.Repeat([]byte{value}, 64<<10)
	if _, err := entry.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return append([]byte(nil), b.Bytes()...)
}

func TestActualFUSEMaterializedArchivePageCacheAndVersionIsolation(t *testing.T) {
	if os.Getenv("MOUNT123_FUSE_TEST") != "1" {
		t.Skip("set MOUNT123_FUSE_TEST=1 to exercise the kernel page cache")
	}
	const size = 64 << 10
	oldContent := bytes.Repeat([]byte{'A'}, size)
	newContent := bytes.Repeat([]byte{'B'}, size)
	api := &pageCacheAPI{data: pageCacheZIP(t, 'A'), version: "v1"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, version := api.snapshot()
		w.Header().Set("ETag", `"`+version+`"`)
		http.ServeContent(w, r, "archive.zip", time.Unix(1, 0), bytes.NewReader(data))
	}))
	defer server.Close()
	api.url = server.URL
	cache, err := storage.NewCache(t.TempDir(), 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	ctx := context.Background()
	directoryTTL := 30 * time.Millisecond
	root := NewWithOptions(ctx, api, cache, 0, true, Options{DirectoryTTL: directoryTTL, SourceTTL: directoryTTL})
	if err := root.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	mountpoint := t.TempDir()
	timeout := 20 * time.Millisecond
	mounted, err := fs.Mount(mountpoint, root, &fs.Options{MountOptions: fuse.MountOptions{Options: []string{"ro"}}, EntryTimeout: &timeout, AttrTimeout: &timeout})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := mounted.Unmount(); err != nil {
			t.Error(err)
		}
		mounted.Wait()
	}()
	path := filepath.Join(mountpoint, "archive.zip", "payload.bin")
	oldFile, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer oldFile.Close()
	first := make([]byte, size)
	if _, err := oldFile.ReadAt(first, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, oldContent) {
		t.Fatal("initial archive member content mismatch")
	}
	// mmap reads the same fully materialized member through the kernel cache.
	mapped, err := syscall.Mmap(int(oldFile.Fd()), 0, size, syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mapped, oldContent) {
		t.Fatal("mmap returned stale or incorrect archive member data")
	}
	if err := syscall.Munmap(mapped); err != nil {
		t.Fatal(err)
	}

	api.update(pageCacheZIP(t, 'B'), "v2")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, _ = root.list(ctx) // expire the old snapshot and launch its coalesced refresh
		root.tree.mu.Lock()
		entries := root.tree.meta["dir:0"]
		updated := false
		if entries != nil {
			listing := entries.value.(*cloudDirectory).entries
			updated = listing["archive.zip"] != nil && listing["archive.zip"].cloud.Version == "v2"
		}
		root.tree.mu.Unlock()
		if updated {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	root.tree.mu.Lock()
	listing := root.tree.meta["dir:0"].value.(map[string]*entry)
	updated := listing["archive.zip"].cloud.Version == "v2"
	root.tree.mu.Unlock()
	if !updated {
		t.Fatal("directory metadata did not refresh to the new content version")
	}

	oldAgain := make([]byte, size)
	if _, err := oldFile.ReadAt(oldAgain, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(oldAgain, oldContent) {
		t.Fatal("old open handle lost its immutable content snapshot")
	}
	newFile, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer newFile.Close()
	current := make([]byte, size)
	if _, err := newFile.ReadAt(current, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(current, newContent) {
		t.Fatal(fmt.Sprintf("new handle returned %q instead of the new version", current[:1]))
	}
}
