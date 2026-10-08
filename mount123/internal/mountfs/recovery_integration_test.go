package mountfs

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/hanwen/go-fuse/v2/fs"
)

type recoveryIntegrationAPI struct {
	url   string
	size  int64
	lists atomic.Uint64
}

func (a *recoveryIntegrationAPI) CacheIdentity() string { return "recovery-integration-account" }
func (a *recoveryIntegrationAPI) List(context.Context, int64) ([]panapi.File, error) {
	a.lists.Add(1)
	return []panapi.File{{ID: 123, Name: "test.json", Size: a.size, Version: "v1"}}, nil
}
func (a *recoveryIntegrationAPI) DownloadURL(context.Context, int64) (string, error) {
	return a.url, nil
}

func TestPublicMountReadsRecoverAndDirectoryRestoresIntoIOStats(t *testing.T) {
	payload := []byte(`{"test":"recovery and durable directory"}`)
	var rangeCalls atomic.Uint64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "bytes=0-0" && rangeCalls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("ETag", `"stable-v1"`)
		http.ServeContent(w, r, "test.json", time.Unix(100, 0), bytes.NewReader(payload))
	}))
	defer server.Close()
	api := &recoveryIntegrationAPI{url: server.URL, size: int64(len(payload))}
	cacheDir := filepath.Join(t.TempDir(), "cache")
	cache, err := storage.NewCache(cacheDir, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	root := New(context.Background(), api, cache, 0, true)
	fs.NewNodeFS(root, &fs.Options{})
	if err := root.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := readNode(t, lookup(t, root, "test.json"), 0, len(payload)); !bytes.Equal(got, payload) {
		t.Fatalf("recovered data = %q", got)
	}
	deadline := time.Now().Add(2 * time.Second)
	for root.IOStats().RemoteRecovery.Recovered == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	snapshot := root.IOStats()
	if snapshot.DirectoryCache.Status != "measured" || snapshot.DirectoryCache.ListCalls != 1 || snapshot.RemoteRecovery.Status != "measured" || snapshot.RemoteRecovery.Retries != 1 || snapshot.RemoteRecovery.Recovered != 1 {
		t.Fatalf("unexpected recovery metrics: directory=%+v remote=%+v", snapshot.DirectoryCache, snapshot.RemoteRecovery)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := storage.NewCache(cacheDir, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	root = New(context.Background(), api, reopened, 0, true)
	if err := root.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot = root.IOStats()
	if api.lists.Load() != 1 || snapshot.DirectoryCache.ListCalls != 0 || snapshot.DirectoryCache.DiskRestores != 1 || snapshot.RemoteRecovery.Attempts != 0 {
		t.Fatalf("restart did not restore directory without network: directory=%+v remote=%+v totalLists=%d", snapshot.DirectoryCache, snapshot.RemoteRecovery, api.lists.Load())
	}
}
