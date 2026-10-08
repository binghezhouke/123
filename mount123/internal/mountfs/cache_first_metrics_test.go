package mountfs

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/hanwen/go-fuse/v2/fs"
)

type cacheFirstMetricsAPI struct {
	url      string
	size     int64
	offline  atomic.Bool
	resolves atomic.Uint64
	lists    atomic.Uint64
}

func (*cacheFirstMetricsAPI) CacheIdentity() string { return "cache-first-metrics-account" }
func (a *cacheFirstMetricsAPI) List(context.Context, int64) ([]panapi.File, error) {
	a.lists.Add(1)
	if a.offline.Load() {
		return nil, errors.New("offline fixture")
	}
	return []panapi.File{{ID: 17, Name: "sample.bin", Version: "content-v1", Size: a.size}}, nil
}
func (a *cacheFirstMetricsAPI) DownloadURL(context.Context, int64) (string, error) {
	a.resolves.Add(1)
	if a.offline.Load() {
		return "", errors.New("offline fixture")
	}
	return a.url, nil
}

// Exercise the public filesystem and metrics together: a warm restart must
// retain local source preparation while omitting both network source stages.
func TestCacheFirstRestartSeparatesLocalAndNetworkStages(t *testing.T) {
	payload := bytes.Repeat([]byte("cached-content"), 512)
	var requests atomic.Uint64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("ETag", `"content-v1"`)
		http.ServeContent(w, r, "sample.bin", time.Unix(100, 0), bytes.NewReader(payload))
	}))
	defer server.Close()
	api := &cacheFirstMetricsAPI{url: server.URL, size: int64(len(payload))}
	cacheDir := t.TempDir()
	open := func() (*storage.Cache, *Node, context.CancelFunc) {
		cache, err := storage.NewCache(cacheDir, 4<<20)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		root := NewWithOptions(ctx, api, cache, 0, true, Options{DisableReadAhead: true})
		fs.NewNodeFS(root, &fs.Options{})
		t.Cleanup(func() { cancel(); _ = cache.Close() })
		if err := root.Prepare(ctx); err != nil {
			t.Fatal(err)
		}
		return cache, root, cancel
	}
	cache, root, cancel := open()
	if got := readNode(t, lookup(t, root, "sample.bin"), 0, len(payload)); !bytes.Equal(got, payload) {
		t.Fatal("cold read content differs")
	}
	deadline := time.Now().Add(2 * time.Second)
	for cache.Stats().ReservedBytes != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if cache.Stats().ReservedBytes != 0 {
		t.Fatal("range publication did not finish")
	}
	cold := root.IOStats()
	if cold.Stages.SourcePrepare.Status != "measured" || cold.Stages.URLResolve.Status != "measured" || cold.Stages.SourceProbe.Status != "measured" {
		t.Fatalf("cold source stages missing: %+v", cold.Stages)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	cancel()
	beforeRequests, beforeURLs, beforeLists := requests.Load(), api.resolves.Load(), api.lists.Load()
	api.offline.Store(true)
	server.Close()
	_, root, _ = open()
	if got := readNode(t, lookup(t, root, "sample.bin"), 0, len(payload)); !bytes.Equal(got, payload) {
		t.Fatal("offline read content differs")
	}
	warm := root.IOStats()
	if warm.Stages.SourcePrepare.Status != "measured" || warm.Stages.FileOpen.Status != "measured" || warm.Stages.URLResolve.Status != "unknown" || warm.Stages.SourceProbe.Status != "unknown" {
		t.Fatalf("restart did not separate local preparation from network: %+v", warm.Stages)
	}
	if api.resolves.Load() != beforeURLs || api.lists.Load() != beforeLists || requests.Load() != beforeRequests || warm.RemoteRecovery.Attempts != 0 {
		t.Fatalf("cached restart used network: URL=%d List=%d HTTP=%d attempts=%d", api.resolves.Load()-beforeURLs, api.lists.Load()-beforeLists, requests.Load()-beforeRequests, warm.RemoteRecovery.Attempts)
	}
}
