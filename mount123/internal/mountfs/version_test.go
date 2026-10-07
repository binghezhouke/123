package mountfs

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/hanwen/go-fuse/v2/fs"
)

type versionAPI struct {
	mu           sync.RWMutex
	data         []byte
	version, url string
}

func (a *versionAPI) List(context.Context, int64) ([]panapi.File, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return []panapi.File{{ID: 1, Name: "photos.zip", Size: int64(len(a.data)), Version: a.version}}, nil
}
func (a *versionAPI) DownloadURL(context.Context, int64) (string, error) { return a.url, nil }
func TestArchiveVersionRefreshPreservesOldHandleSnapshot(t *testing.T) {
	makeZIP := func(content string) []byte {
		var b bytes.Buffer
		w := zip.NewWriter(&b)
		m, _ := w.CreateHeader(&zip.FileHeader{Name: "images/file.txt", Method: zip.Store})
		m.Write([]byte(content))
		w.Close()
		return b.Bytes()
	}
	api := &versionAPI{data: makeZIP("old content"), version: "v1"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.mu.RLock()
		data, version := api.data, api.version
		api.mu.RUnlock()
		w.Header().Set("ETag", `"`+version+`"`)
		http.ServeContent(w, r, "zip", time.Unix(100, 0), bytes.NewReader(data))
	}))
	defer server.Close()
	api.url = server.URL
	cache, err := storage.NewCache(t.TempDir(), 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	root := NewWithOptions(context.Background(), api, cache, 0, true, Options{DirectoryTTL: 20 * time.Millisecond})
	fs.NewNodeFS(root, &fs.Options{})
	old := lookup(t, lookup(t, lookup(t, root, "photos.zip"), "images"), "file.txt")
	if got := string(readNode(t, old, 0, 11)); got != "old content" {
		t.Fatal(got)
	}
	api.mu.Lock()
	api.data = makeZIP("new content")
	api.version = "v2"
	api.mu.Unlock()
	time.Sleep(30 * time.Millisecond)
	fresh := lookup(t, lookup(t, lookup(t, root, "photos.zip"), "images"), "file.txt")
	if got := string(readNode(t, fresh, 0, 11)); got != "new content" {
		t.Fatal(got)
	}
	if got := string(readNode(t, old, 0, 11)); got != "old content" {
		t.Fatal("old snapshot changed:", got)
	}
	if fresh.StableAttr().Ino == old.StableAttr().Ino {
		t.Fatal("versions reused inode")
	}
}
