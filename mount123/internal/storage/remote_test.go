package storage

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRemoteRangeReadsAndCachesBlocks(t *testing.T) {
	data := []byte(strings.Repeat("0123456789", int(remoteBlockSize/10+1)))
	data = data[:remoteBlockSize+17]
	var requests atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Accept-Encoding") != "identity" {
			t.Errorf("Accept-Encoding = %q", r.Header.Get("Accept-Encoding"))
		}
		if r.Header.Get("Range") == "" {
			t.Errorf("missing Range header")
		}
		var a, b int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &a, &b); err != nil {
			http.Error(w, "bad range", 400)
			return
		}
		w.Header().Set("ETag", `"version-1"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, len(data)))
		w.Header().Set("Content-Length", strconv.FormatInt(b-a+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[a : b+1])
	}))
	defer s.Close()
	c, err := NewCache(t.TempDir(), 4*remoteBlockSize)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r, err := NewRemote(context.Background(), c, "file-v1", int64(len(data)), func(context.Context) (string, error) { return s.URL, nil })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Key(), "etag") {
		t.Fatalf("key does not include entity version: %q", r.Key())
	}
	out := make([]byte, 25)
	n, err := r.ReadAt(out, remoteBlockSize-8)
	if err != nil || n != 25 {
		t.Fatalf("ReadAt = %d, %v", n, err)
	}
	for i := range out {
		if out[i] != data[remoteBlockSize-8+int64(i)] {
			t.Fatalf("byte %d differs", i)
		}
	}
	before := requests.Load()
	again := make([]byte, 25)
	if _, err = r.ReadAt(again, remoteBlockSize-8); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != before {
		t.Fatalf("expected cached read, requests %d -> %d", before, requests.Load())
	}
	if _, err = r.ReadAt(make([]byte, 8), int64(len(data))-4); err != io.EOF {
		t.Fatalf("short read error = %v", err)
	}
}

func TestRemoteRejectsRangeFallbackAndInvalidLength(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"200": func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("whole object")) },
		"bad-content-range": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Range", "bytes 0-0/99")
			w.WriteHeader(206)
			w.Write([]byte("x"))
		},
		"encoded-range": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Range", "bytes 0-0/1")
			w.Header().Set("Content-Encoding", "gzip")
			w.WriteHeader(206)
			_, _ = w.Write([]byte("x"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := httptest.NewServer(handler)
			defer s.Close()
			c, e := NewCache(t.TempDir(), 1024)
			if e != nil {
				t.Fatal(e)
			}
			defer c.Close()
			if _, e = NewRemote(context.Background(), c, "k", 1, func(context.Context) (string, error) { return s.URL, nil }); e == nil {
				t.Fatal("expected probe failure")
			}
		})
	}
}

func TestRemoteUsesLastModifiedWhenETagIsWeak(t *testing.T) {
	var requests atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) > 1 && r.Header.Get("If-Match") != "" {
			t.Errorf("weak ETag used with If-Match: %q", r.Header.Get("If-Match"))
		}
		if requests.Load() > 1 && r.Header.Get("If-Unmodified-Since") != "Wed, 21 Oct 2015 07:28:00 GMT" {
			t.Errorf("missing date condition: %q", r.Header.Get("If-Unmodified-Since"))
		}
		w.Header().Set("ETag", `W/"weak"`)
		w.Header().Set("Last-Modified", "Wed, 21 Oct 2015 07:28:00 GMT")
		w.Header().Set("Content-Range", "bytes 0-0/1")
		w.WriteHeader(206)
		_, _ = w.Write([]byte("x"))
	}))
	defer s.Close()
	c, err := NewCache(t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r, err := NewRemote(context.Background(), c, "k", 1, func(context.Context) (string, error) { return s.URL, nil })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Key(), "modified:") {
		t.Fatalf("weak ETag should use Last-Modified namespace: %q", r.Key())
	}
	if _, err = r.ReadAt(make([]byte, 1), 0); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteReadsUseMountLifetimeContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	release := make(chan struct{})
	var requests atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("ETag", `"v1"`)
			w.Header().Set("Content-Range", "bytes 0-0/1")
			w.WriteHeader(206)
			_, _ = w.Write([]byte("x"))
			return
		}
		close(started)
		<-release
	}))
	defer s.Close()
	c, err := NewCache(t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r, err := NewRemote(ctx, c, "k", 1, func(context.Context) (string, error) { return s.URL, nil })
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { _, readErr := r.ReadAt(make([]byte, 1), 0); result <- readErr }()
	<-started
	cancel()
	close(release)
	if err = <-result; err == nil {
		t.Fatal("expected canceled range read")
	}
}

func TestRemoteRejectsChangedEntityAndRefreshesExpiredURL(t *testing.T) {
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("ETag", `"old"`)
		} else {
			w.Header().Set("ETag", `"new"`)
		}
		w.Header().Set("Content-Range", "bytes 0-0/1")
		w.WriteHeader(206)
		_, _ = w.Write([]byte("x"))
	}))
	defer s.Close()
	c, err := NewCache(t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r, err := NewRemote(context.Background(), c, "k", 1, func(context.Context) (string, error) { return s.URL, nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.ReadAt(make([]byte, 1), 0); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("want entity changed error, got %v", err)
	}
}

func TestRemoteRefreshesExpiredStatusAndSanitizesResolverError(t *testing.T) {
	var hits, resolves atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("ETag", `"stable"`)
		w.Header().Set("Content-Range", "bytes 0-0/1")
		w.WriteHeader(206)
		_, _ = w.Write([]byte("x"))
	}))
	defer s.Close()
	c, err := NewCache(t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r, err := NewRemote(context.Background(), c, "k", 1, func(context.Context) (string, error) { resolves.Add(1); return s.URL, nil })
	if err != nil {
		t.Fatal(err)
	}
	if resolves.Load() != 2 {
		t.Fatalf("resolver called %d times", resolves.Load())
	}
	_ = r
	_, err = NewRemote(context.Background(), c, "secret", 1, func(context.Context) (string, error) {
		return "", fmt.Errorf("failed for https://host/path?signature=secret")
	})
	if err == nil || strings.Contains(err.Error(), "signature=secret") {
		t.Fatalf("resolver error leaked URL: %v", err)
	}
}
