package mountfs

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/hanwen/go-fuse/v2/fs"
)

type cachedOpenAPI struct {
	url      string
	files    []panapi.File
	resolves atomic.Int32
	offline  bool
}

func (a *cachedOpenAPI) CacheIdentity() string { return "cached-open-test-account" }
func (a *cachedOpenAPI) List(context.Context, int64) ([]panapi.File, error) {
	return append([]panapi.File(nil), a.files...), nil
}
func (a *cachedOpenAPI) DownloadURL(_ context.Context, id int64) (string, error) {
	a.resolves.Add(1)
	if a.offline {
		return "", fmt.Errorf("offline resolver called")
	}
	return fmt.Sprintf("%s/%d", a.url, id), nil
}

func makeCachedOpenArchive(t *testing.T) ([]byte, map[string][]byte) {
	t.Helper()
	members := map[string][]byte{
		"store.txt":   bytes.Repeat([]byte("stored-member-"), 300),
		"deflate.txt": bytes.Repeat([]byte("deflated-member-"), 500),
	}
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for name, data := range members {
		method := uint16(zip.Store)
		if strings.HasPrefix(name, "deflate") {
			method = zip.Deflate
		}
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: method})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes(), members
}

func TestCachedOpenUsesPersistedSourcesForPlainStoreAndInflatedMembers(t *testing.T) {
	archive, members := makeCachedOpenArchive(t)
	plain := bytes.Repeat([]byte("ordinary-file-content-"), 400)
	data := map[int64][]byte{1: plain, 2: archive}
	versions := map[int64]string{1: `"plain-v1"`, 2: `"archive-v1"`}
	files := []panapi.File{
		{ID: 1, Name: "plain.bin", Size: int64(len(plain)), Version: versions[1]},
		{ID: 2, Name: "bundle.zip", Size: int64(len(archive)), Version: versions[2]},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var id int64
		if _, err := fmt.Sscanf(req.URL.Path, "/%d", &id); err != nil {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		body := data[id]
		start, end := int64(0), int64(0)
		if _, err := fmt.Sscanf(req.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil || start < 0 || end < start || end >= int64(len(body)) {
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}
		etag := versions[id]
		if match := req.Header.Get("If-Match"); match != "" && match != etag {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body[start : end+1])
	}))
	defer server.Close()

	dir := t.TempDir()
	cache, err := storage.NewCache(dir, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	api := &cachedOpenAPI{url: server.URL, files: files}
	root := New(context.Background(), api, cache, 0, true)
	fs.NewNodeFS(root, &fs.Options{})
	plainNode := lookup(t, root, "plain.bin")
	if got := readNode(t, plainNode, 0, len(plain)); !bytes.Equal(got, plain) {
		t.Fatal("initial ordinary file read mismatch")
	}
	archiveNode := lookup(t, root, "bundle.zip")
	for name, want := range members {
		member := lookup(t, archiveNode, name)
		if got := readNode(t, member, 0, len(want)); !bytes.Equal(got, want) {
			t.Fatalf("initial ZIP member %q read mismatch", name)
		}
	}
	if got := api.resolves.Load(); got != 2 {
		t.Fatalf("initial URL resolver calls = %d, want one per source", got)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}

	cache, err = storage.NewCache(dir, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	offlineAPI := &cachedOpenAPI{files: files, offline: true}
	offlineRoot := New(context.Background(), offlineAPI, cache, 0, true)
	fs.NewNodeFS(offlineRoot, &fs.Options{})
	plainNode = lookup(t, offlineRoot, "plain.bin")
	if got := readNode(t, plainNode, 0, len(plain)); !bytes.Equal(got, plain) {
		t.Fatal("offline ordinary file read mismatch")
	}
	archiveNode = lookup(t, offlineRoot, "bundle.zip")
	for name, want := range members {
		member := lookup(t, archiveNode, name)
		if got := readNode(t, member, 0, len(want)); !bytes.Equal(got, want) {
			t.Fatalf("offline ZIP member %q read mismatch", name)
		}
	}
	if got := offlineAPI.resolves.Load(); got != 0 {
		t.Fatalf("offline source URL resolutions = %d, want 0", got)
	}
}

func TestCachedSourceInvalidationEvictsInMemoryRemote(t *testing.T) {
	content := []byte("source that changes after open")
	var etag atomic.Value
	etag.Store(`"v1"`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("If-Match") != "" && req.Header.Get("If-Match") != etag.Load().(string) {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		var start, end int64
		if _, err := fmt.Sscanf(req.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil || start < 0 || end < start || end >= int64(len(content)) {
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}
		w.Header().Set("ETag", etag.Load().(string))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(content)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(content[start : end+1])
	}))
	defer server.Close()
	api := &cachedOpenAPI{url: server.URL, files: []panapi.File{{ID: 9, Name: "changing.bin", Size: int64(len(content)), Version: `"version"`}}}
	cache, err := storage.NewCache(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	root := New(context.Background(), api, cache, 0, false)
	fs.NewNodeFS(root, &fs.Options{})
	node := lookup(t, root, "changing.bin")
	oldRemote, err := node.source(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	etag.Store(`"v2"`)
	buf := make([]byte, len(content))
	if _, err := oldRemote.ReadRangeAtContext(context.Background(), buf, 0); err == nil {
		t.Fatal("stale source unexpectedly read after entity changed")
	}
	newRemote, err := node.source(context.Background())
	if err != nil {
		t.Fatalf("source after invalidation: %v", err)
	}
	if newRemote == oldRemote || newRemote.Key() == oldRemote.Key() {
		t.Fatal("source lookup returned the invalidated in-memory identity")
	}
}

func TestCachedSourceRestoreDoesNotExtendTreeExpiry(t *testing.T) {
	content := []byte("expiry check")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests.Add(1)
		var start, end int64
		if _, err := fmt.Sscanf(req.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil || start < 0 || end < start || end >= int64(len(content)) {
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}
		w.Header().Set("ETag", `"expiry-v1"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(content)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(content[start : end+1])
	}))
	defer server.Close()
	dir := t.TempDir()
	files := []panapi.File{{ID: 12, Name: "expiring.bin", Size: int64(len(content)), Version: `"api-v1"`}}
	options := Options{SourceTTL: 500 * time.Millisecond}
	firstCache, err := storage.NewCache(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	firstAPI := &cachedOpenAPI{url: server.URL, files: files}
	firstRoot := NewWithOptions(context.Background(), firstAPI, firstCache, 0, false, options)
	fs.NewNodeFS(firstRoot, &fs.Options{})
	firstNode := lookup(t, firstRoot, "expiring.bin")
	if _, err := firstNode.source(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := firstCache.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(350 * time.Millisecond)

	secondCache, err := storage.NewCache(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer secondCache.Close()
	secondAPI := &cachedOpenAPI{url: server.URL, files: files}
	secondRoot := NewWithOptions(context.Background(), secondAPI, secondCache, 0, false, options)
	fs.NewNodeFS(secondRoot, &fs.Options{})
	secondNode := lookup(t, secondRoot, "expiring.bin")
	if _, err := secondNode.source(context.Background()); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 || secondAPI.resolves.Load() != 0 {
		t.Fatalf("valid descriptor was not restored: requests=%d resolves=%d", requests.Load(), secondAPI.resolves.Load())
	}
	time.Sleep(250 * time.Millisecond)
	if _, err := secondNode.source(context.Background()); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 || secondAPI.resolves.Load() != 1 {
		t.Fatalf("restored source lifetime was extended: requests=%d resolves=%d", requests.Load(), secondAPI.resolves.Load())
	}
}
