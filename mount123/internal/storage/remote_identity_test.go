package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func identityFixture(t *testing.T, dir string, capacity int64, handler http.Handler) (*Cache, *httptest.Server) {
	t.Helper()
	cache, err := NewCache(dir, capacity)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	return cache, server
}

func identityRangeHandler(data []byte, etag string, calls *atomic.Int32, status int) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		if match := req.Header.Get("If-Match"); match != "" && match != etag {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		start, end := int64(0), int64(0)
		if _, err := fmt.Sscanf(req.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}
		if end >= int64(len(data)) {
			end = int64(len(data)) - 1
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
	}
}

func identityOpts(ttl time.Duration) RemoteIdentity {
	return RemoteIdentity{Account: "stable-account-digest", FileID: 81, Version: `"version-1":100`, TTL: ttl}
}

func newIdentityRemote(t *testing.T, cache *Cache, identity RemoteIdentity, size int64, resolve ResolveURL) *Remote {
	t.Helper()
	r, err := NewCachedRemoteContext(context.Background(), context.Background(), cache, "identity-fixture", size, resolve, identity)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func waitIdentityRangeCached(t *testing.T, cache *Cache, identity string, start, end int64) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for len(cache.missingRanges(identity, start, end)) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if missing := cache.missingRanges(identity, start, end); len(missing) != 0 {
		t.Fatalf("range did not finish publishing to cache: %+v", missing)
	}
}

func TestCachedRemoteRestoresCoveredContentWithoutResolvingURL(t *testing.T) {
	data := bytes.Repeat([]byte("cached-source-"), 9000)
	var requests atomic.Int32
	dir := t.TempDir()
	cache, server := identityFixture(t, dir, 8<<20, identityRangeHandler(data, `"v1"`, &requests, 0))
	resolveCalls := atomic.Int32{}
	resolve := func(context.Context) (string, error) { resolveCalls.Add(1); return server.URL, nil }
	first := newIdentityRemote(t, cache, identityOpts(0), int64(len(data)), resolve)
	got := make([]byte, len(data))
	if n, err := first.ReadRangeAtContext(context.Background(), got, 0); err != nil || n != len(got) || !bytes.Equal(got, data) {
		t.Fatalf("initial read = %d, %v", n, err)
	}
	waitIdentityRangeCached(t, cache, first.rangeID, 0, int64(len(data)))
	key := first.Key()
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	server.Close()

	cache, err := NewCache(dir, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	remote := newIdentityRemote(t, cache, identityOpts(0), int64(len(data)), func(context.Context) (string, error) {
		resolveCalls.Add(1)
		return "", fmt.Errorf("offline resolver unexpectedly called")
	})
	if remote.Key() != key {
		t.Fatalf("restored key = %q, want %q", remote.Key(), key)
	}
	clear(got)
	if n, err := remote.ReadRangeAtContext(context.Background(), got, 0); err != nil || n != len(got) || !bytes.Equal(got, data) {
		t.Fatalf("offline read = %d, %v", n, err)
	}
	if got := resolveCalls.Load(); got != 1 {
		t.Fatalf("resolver calls = %d, want only initial resolve", got)
	}
}

func TestCachedRemoteGapUsesConditionalRangeWithoutProbe(t *testing.T) {
	data := []byte("missing cached range")
	var requests atomic.Int32
	var conditional atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("If-Match") == `"v1"` {
			conditional.Add(1)
		}
		identityRangeHandler(data, `"v1"`, &requests, 0).ServeHTTP(w, req)
	})
	cache, server := identityFixture(t, t.TempDir(), 1<<20, handler)
	defer cache.Close()
	var resolveCalls atomic.Int32
	resolve := func(context.Context) (string, error) { resolveCalls.Add(1); return server.URL, nil }
	_ = newIdentityRemote(t, cache, identityOpts(0), int64(len(data)), resolve)
	if requests.Load() != 1 { // initial 0-0 identity probe
		t.Fatalf("initial HTTP requests = %d, want 1", requests.Load())
	}
	requests.Store(0)
	remote := newIdentityRemote(t, cache, identityOpts(0), int64(len(data)), resolve)
	got := make([]byte, len(data))
	if n, err := remote.ReadRangeAtContext(context.Background(), got, 0); err != nil || n != len(got) || !bytes.Equal(got, data) {
		t.Fatalf("gap read = %d, %v", n, err)
	}
	if requests.Load() != 1 || conditional.Load() != 1 || resolveCalls.Load() != 1 {
		t.Fatalf("requests=%d conditional=%d resolves=%d, want one conditional range using cached URL", requests.Load(), conditional.Load(), resolveCalls.Load())
	}
}

func TestCachedRemoteRejectsChangedEntityAndInvalidatesDescriptor(t *testing.T) {
	data := []byte("identity changed")
	var requests atomic.Int32
	var etag atomic.Value
	etag.Store(`"v1"`)
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests.Add(1)
		current := etag.Load().(string)
		if match := req.Header.Get("If-Match"); match != "" && match != current {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		start, end := int64(0), int64(0)
		if _, err := fmt.Sscanf(req.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}
		w.Header().Set("ETag", current)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
	})
	cache, server := identityFixture(t, t.TempDir(), 1<<20, handler)
	defer cache.Close()
	identity := identityOpts(0)
	resolve := func(context.Context) (string, error) { return server.URL, nil }
	_ = newIdentityRemote(t, cache, identity, int64(len(data)), resolve)
	etag.Store(`"v2"`)
	invalidated := atomic.Int32{}
	identity.OnInvalidated = func() { invalidated.Add(1) }
	remote := newIdentityRemote(t, cache, identity, int64(len(data)), resolve)
	buf := make([]byte, len(data))
	if _, err := remote.ReadRangeAtContext(context.Background(), buf, 0); err == nil {
		t.Fatal("expected changed entity error")
	}
	if invalidated.Load() != 1 {
		t.Fatalf("invalidation callback count = %d, want 1", invalidated.Load())
	}
	_, digest := remoteIdentityKeys(cache, identity, int64(len(data)))
	if _, err := cache.OpenArchiveIndex("remote-identity-v1:" + digest); err == nil {
		t.Fatal("stale identity descriptor remains after validator mismatch")
	}
}

func TestCachedRemoteDescriptorTTLIsNotExtended(t *testing.T) {
	data := []byte("ttl")
	var requests atomic.Int32
	cache, server := identityFixture(t, t.TempDir(), 1<<20, identityRangeHandler(data, `"v1"`, &requests, 0))
	defer cache.Close()
	var resolves atomic.Int32
	resolve := func(context.Context) (string, error) { resolves.Add(1); return server.URL, nil }
	identity := identityOpts(15 * time.Millisecond)
	_ = newIdentityRemote(t, cache, identity, int64(len(data)), resolve)
	time.Sleep(30 * time.Millisecond)
	requests.Store(0)
	_ = newIdentityRemote(t, cache, identity, int64(len(data)), resolve)
	if requests.Load() != 1 {
		t.Fatalf("expired descriptor restored without a fresh probe: requests=%d", requests.Load())
	}
}

func TestCachedRemoteRejectsSyntheticVersionAndInvalidDescriptor(t *testing.T) {
	data := []byte("version")
	var requests atomic.Int32
	cache, server := identityFixture(t, t.TempDir(), 1<<20, identityRangeHandler(data, `"v1"`, &requests, 0))
	defer cache.Close()
	resolve := func(context.Context) (string, error) { return server.URL, nil }
	identity := identityOpts(0)
	identity.Version = ":" + fmt.Sprint(len(data))
	_ = newIdentityRemote(t, cache, identity, int64(len(data)), resolve)
	identity.Version = `"real"`
	_ = newIdentityRemote(t, cache, identity, int64(len(data)), resolve)
	_, digest := remoteIdentityKeys(cache, identity, int64(len(data)))
	key := "remote-identity-v1:" + digest
	if err := cache.ReplaceArchiveIndex(context.Background(), key, []byte("corrupt")); err != nil {
		t.Fatal(err)
	}
	before := requests.Load()
	_ = newIdentityRemote(t, cache, identity, int64(len(data)), resolve)
	if requests.Load() != before+1 {
		t.Fatalf("corrupt descriptor did not force probe: requests before=%d after=%d", before, requests.Load())
	}
}

func TestCachedRemoteDescriptorAdmissionIsBestEffort(t *testing.T) {
	data := []byte("small cache")
	var requests atomic.Int32
	cache, server := identityFixture(t, t.TempDir(), 64, identityRangeHandler(data, `"v1"`, &requests, 0))
	defer cache.Close()
	remote := newIdentityRemote(t, cache, identityOpts(0), int64(len(data)), func(context.Context) (string, error) { return server.URL, nil })
	if remote.Key() == "" || requests.Load() != 1 {
		t.Fatalf("eager source unavailable: key=%q requests=%d", remote.Key(), requests.Load())
	}
	_, digest := remoteIdentityKeys(cache, identityOpts(0), int64(len(data)))
	h, err := cache.OpenArchiveIndex("remote-identity-v1:" + digest)
	if err == nil {
		_ = h.Close()
		t.Fatal("descriptor unexpectedly admitted to undersized cache")
	}
}

func TestCachedRemoteIdentityChangesForceFreshProbe(t *testing.T) {
	data := []byte("identity scope")
	var requests atomic.Int32
	cache, server := identityFixture(t, t.TempDir(), 1<<20, identityRangeHandler(data, `"v1"`, &requests, 0))
	defer cache.Close()
	resolve := func(context.Context) (string, error) { return server.URL, nil }
	base := identityOpts(0)
	_ = newIdentityRemote(t, cache, base, int64(len(data)), resolve)
	for name, changed := range map[string]RemoteIdentity{
		"account": func() RemoteIdentity { v := base; v.Account = "another-account"; return v }(),
		"version": func() RemoteIdentity { v := base; v.Version = `"version-2"`; return v }(),
	} {
		t.Run(name, func(t *testing.T) {
			before := requests.Load()
			_ = newIdentityRemote(t, cache, changed, int64(len(data)), resolve)
			if requests.Load() != before+1 {
				t.Fatalf("identity change reused descriptor: requests before=%d after=%d", before, requests.Load())
			}
		})
	}
	before := requests.Load()
	if _, err := NewCachedRemoteContext(context.Background(), context.Background(), cache, "identity-fixture", int64(len(data)+1), resolve, base); err == nil {
		t.Fatal("size change unexpectedly passed source probe")
	}
	if requests.Load() != before+1 {
		t.Fatalf("size change skipped fresh probe: requests before=%d after=%d", before, requests.Load())
	}
}

func modifiedRangeHandler(data []byte, modified string, requests *atomic.Int32, conditional *atomic.Value) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		requests.Add(1)
		if value := req.Header.Get("If-Unmodified-Since"); value != "" {
			conditional.Store(value)
			if value != modified {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
		}
		start, end := int64(0), int64(0)
		if _, err := fmt.Sscanf(req.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}
		if end >= int64(len(data)) {
			end = int64(len(data)) - 1
		}
		w.Header().Set("Last-Modified", modified)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
	}
}

func TestCachedRemoteRestoresLastModifiedIdentityWithoutURLOrProbe(t *testing.T) {
	data := bytes.Repeat([]byte("last-modified-cache"), 32)
	const modified = "Wed, 07 Jan 2026 01:28:19 GMT"
	var requests atomic.Int32
	var conditional atomic.Value
	dir := t.TempDir()
	cache, server := identityFixture(t, dir, 2<<20, modifiedRangeHandler(data, modified, &requests, &conditional))
	resolveCalls := atomic.Int32{}
	resolve := func(context.Context) (string, error) { resolveCalls.Add(1); return server.URL, nil }
	remote := newIdentityRemote(t, cache, identityOpts(0), int64(len(data)), resolve)
	wantKey := "identity-fixture\x00modified:" + modified
	if remote.Key() != wantKey {
		t.Fatalf("initial modified key = %q, want %q", remote.Key(), wantKey)
	}
	got := make([]byte, len(data))
	if n, err := remote.ReadRangeAtContext(context.Background(), got, 0); err != nil || n != len(got) || !bytes.Equal(got, data) {
		t.Fatalf("initial modified read = %d, %v", n, err)
	}
	waitIdentityRangeCached(t, cache, remote.rangeID, 0, int64(len(data)))
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	server.Close()

	cache, err := NewCache(dir, 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	restored := newIdentityRemote(t, cache, identityOpts(0), int64(len(data)), func(context.Context) (string, error) {
		resolveCalls.Add(1)
		return "", errors.New("offline resolver unexpectedly called")
	})
	if restored.Key() != wantKey || restored.modified != modified {
		t.Fatalf("restored modified identity = key %q modified %q", restored.Key(), restored.modified)
	}
	if restored.rangeID != remote.rangeID {
		t.Fatalf("restored range id = %q, initial = %q", restored.rangeID, remote.rangeID)
	}
	clear(got)
	if n, err := restored.ReadRangeAtContext(context.Background(), got, 0); err != nil || n != len(got) || !bytes.Equal(got, data) {
		t.Fatalf("offline modified read = %d, %v", n, err)
	}
	if resolveCalls.Load() != 1 {
		t.Fatalf("resolver calls = %d, want initial call only", resolveCalls.Load())
	}
}

func TestCachedRemoteLastModifiedGapIsConditional(t *testing.T) {
	data := []byte("last modified remote gap")
	const modified = "Wed, 07 Jan 2026 01:28:19 GMT"
	var requests atomic.Int32
	var conditional atomic.Value
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests.Add(1)
		if value := req.Header.Get("If-Unmodified-Since"); value != "" {
			conditional.Store(value)
			if value != modified {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
		}
		start, end := int64(0), int64(0)
		if _, err := fmt.Sscanf(req.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}
		if end >= int64(len(data)) {
			end = int64(len(data)) - 1
		}
		w.Header().Set("Last-Modified", modified)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
	})
	cache, server := identityFixture(t, t.TempDir(), 1<<20, handler)
	defer cache.Close()
	identity := identityOpts(0)
	resolve := func(context.Context) (string, error) { return server.URL, nil }
	_ = newIdentityRemote(t, cache, identity, int64(len(data)), resolve) // initial probe
	requests.Store(0)
	remote := newIdentityRemote(t, cache, identity, int64(len(data)), resolve)
	got := make([]byte, len(data))
	if n, err := remote.ReadRangeAtContext(context.Background(), got, 0); err != nil || n != len(got) || !bytes.Equal(got, data) {
		t.Fatalf("conditional modified gap read = %d, %v", n, err)
	}
	if requests.Load() != 1 || conditional.Load() != modified {
		t.Fatalf("modified gap requests=%d If-Unmodified-Since=%v", requests.Load(), conditional.Load())
	}
}

func TestCachedRemoteLastModifiedChangeInvalidatesDescriptor(t *testing.T) {
	data := []byte("last modified changed")
	const modified = "Wed, 07 Jan 2026 01:28:19 GMT"
	const changed = "Wed, 07 Jan 2026 01:29:19 GMT"
	var requests atomic.Int32
	var current atomic.Value
	current.Store(modified)
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		now := current.Load().(string)
		requests.Add(1)
		if value := req.Header.Get("If-Unmodified-Since"); value != "" && value != now {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		start, end := int64(0), int64(0)
		if _, err := fmt.Sscanf(req.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}
		if end >= int64(len(data)) {
			end = int64(len(data)) - 1
		}
		w.Header().Set("Last-Modified", now)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
	})
	cache, server := identityFixture(t, t.TempDir(), 1<<20, handler)
	defer cache.Close()
	identity := identityOpts(0)
	resolve := func(context.Context) (string, error) { return server.URL, nil }
	_ = newIdentityRemote(t, cache, identity, int64(len(data)), resolve)
	invalidated := atomic.Int32{}
	identity.OnInvalidated = func() { invalidated.Add(1) }
	remote := newIdentityRemote(t, cache, identity, int64(len(data)), resolve)
	current.Store(changed)
	buf := make([]byte, len(data))
	if _, err := remote.ReadRangeAtContext(context.Background(), buf, 0); err == nil {
		t.Fatal("changed Last-Modified source unexpectedly read")
	}
	if invalidated.Load() != 1 {
		t.Fatalf("invalidation callback count = %d, want 1", invalidated.Load())
	}
	_, digest := remoteIdentityKeys(cache, identity, int64(len(data)))
	if _, err := cache.OpenArchiveIndex("remote-identity-v1:" + digest); err == nil {
		t.Fatal("stale Last-Modified descriptor remains")
	}
}

func TestCachedRemoteRejectsInvalidLastModified(t *testing.T) {
	data := []byte("invalid validator")
	var requests atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests.Add(1)
		start, end := int64(0), int64(0)
		if _, err := fmt.Sscanf(req.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}
		if end >= int64(len(data)) {
			end = int64(len(data)) - 1
		}
		w.Header().Set("Last-Modified", "not-a-date")
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
	})
	cache, server := identityFixture(t, t.TempDir(), 1<<20, handler)
	defer cache.Close()
	resolve := func(context.Context) (string, error) { return server.URL, nil }
	_ = newIdentityRemote(t, cache, identityOpts(0), int64(len(data)), resolve)
	before := requests.Load()
	_ = newIdentityRemote(t, cache, identityOpts(0), int64(len(data)), resolve)
	if requests.Load() != before+1 {
		t.Fatalf("invalid Last-Modified was cached: requests before=%d after=%d", before, requests.Load())
	}
}

func TestRemoteWithoutValidatorNeverReusesPersistentRanges(t *testing.T) {
	var current atomic.Value
	current.Store([]byte("first content"))
	var probes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		probes.Add(1)
		data := current.Load().([]byte)
		var start, end int64
		if _, err := fmt.Sscanf(req.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}
		if end >= int64(len(data)) {
			end = int64(len(data)) - 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
	}))
	defer server.Close()
	resolve := func(context.Context) (string, error) { return server.URL, nil }
	dir := t.TempDir()
	firstCache, err := NewCache(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	first, err := NewRemoteContext(context.Background(), context.Background(), firstCache, "unvalidated", int64(len("first content")), resolve)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len("first content"))
	if _, err := first.ReadRangeAtContext(context.Background(), buf, 0); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "first content" {
		t.Fatalf("first read = %q", buf)
	}
	firstKey := first.Key()
	if err := firstCache.Close(); err != nil {
		t.Fatal(err)
	}

	current.Store([]byte("newer content"))
	secondCache, err := NewCache(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer secondCache.Close()
	second, err := NewRemoteContext(context.Background(), context.Background(), secondCache, "unvalidated", int64(len("newer content")), resolve)
	if err != nil {
		t.Fatal(err)
	}
	if second.Key() == firstKey {
		t.Fatal("unvalidated remote reused a persistent cache namespace")
	}
	buf = make([]byte, len("newer content"))
	if _, err := second.ReadRangeAtContext(context.Background(), buf, 0); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "newer content" {
		t.Fatalf("second read reused stale bytes: %q", buf)
	}
}
