package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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

func TestRemoteReadAtContextCancelsHTTPAndBody(t *testing.T) {
	for _, phase := range []string{"request", "body"} {
		t.Run(phase, func(t *testing.T) {
			started := make(chan struct{})
			var calls atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if calls.Add(1) == 1 {
					w.Header().Set("ETag", `"v1"`)
					w.Header().Set("Content-Range", "bytes 0-0/1")
					w.WriteHeader(206)
					_, _ = w.Write([]byte("x"))
					return
				}
				close(started)
				if phase == "body" {
					w.Header().Set("ETag", `"v1"`)
					w.Header().Set("Content-Range", "bytes 0-0/1")
					w.WriteHeader(206)
					w.(http.Flusher).Flush()
				}
				<-req.Context().Done()
			}))
			defer s.Close()
			c, err := NewCache(t.TempDir(), 1024)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			r, err := NewRemote(context.Background(), c, "cancel-"+phase, 1, func(context.Context) (string, error) { return s.URL, nil })
			if err != nil {
				t.Fatal(err)
			}
			readCtx, cancel := context.WithCancel(context.Background())
			result := make(chan error, 1)
			go func() { _, readErr := r.ReadAtContext(readCtx, make([]byte, 1), 0); result <- readErr }()
			<-started
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("read error = %v, want context.Canceled", err)
				}
			case <-time.After(time.Second):
				t.Fatal("canceled read did not return promptly")
			}
		})
	}
}

func TestNewRemoteContextProbeCancellationAndOperationLifetime(t *testing.T) {
	for _, phase := range []string{"request", "body"} {
		t.Run("canceled-"+phase, func(t *testing.T) {
			started := make(chan struct{})
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				close(started)
				if phase == "body" {
					w.Header().Set("Content-Range", "bytes 0-0/1")
					w.WriteHeader(http.StatusPartialContent)
					w.(http.Flusher).Flush()
				}
				<-req.Context().Done()
			}))
			defer s.Close()
			c, err := NewCache(t.TempDir(), 1024)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			lifetime := context.Background()
			operation, cancel := context.WithCancel(context.Background())
			result := make(chan error, 1)
			go func() {
				_, e := NewRemoteContext(lifetime, operation, c, "probe-"+phase, 1, func(context.Context) (string, error) { return s.URL, nil })
				result <- e
			}()
			<-started
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("probe error = %v, want context.Canceled", err)
				}
			case <-time.After(time.Second):
				t.Fatal("canceled probe did not return promptly")
			}
		})
	}
	t.Run("operation-context-is-not-retained", func(t *testing.T) {
		var calls atomic.Int32
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			calls.Add(1)
			w.Header().Set("ETag", `"v1"`)
			w.Header().Set("Content-Range", "bytes 0-0/1")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte("x"))
		}))
		defer s.Close()
		c, err := NewCache(t.TempDir(), 1024)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		operation, cancel := context.WithCancel(context.Background())
		r, err := NewRemoteContext(context.Background(), operation, c, "detached", 1, func(context.Context) (string, error) { return s.URL, nil })
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		if _, err = r.ReadAt(make([]byte, 1), 0); err != nil {
			t.Fatalf("read retained constructor context: %v", err)
		}
		if calls.Load() != 2 {
			t.Fatalf("server calls = %d, want probe plus read", calls.Load())
		}
	})
}

func TestRemoteReadAtContextOperationsAreIndependent(t *testing.T) {
	started := make(chan struct{}, 2)
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		call := calls.Add(1)
		if call == 1 {
			w.Header().Set("ETag", `"v1"`)
			w.Header().Set("Content-Range", "bytes 0-0/1")
			w.WriteHeader(206)
			_, _ = w.Write([]byte("x"))
			return
		}
		started <- struct{}{}
		if call == 2 {
			<-req.Context().Done()
			return
		}
		w.Header().Set("ETag", `"v1"`)
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
	r, err := NewRemote(context.Background(), c, "independent", 1, func(context.Context) (string, error) { return s.URL, nil })
	if err != nil {
		t.Fatal(err)
	}
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	ctx2 := context.Background()
	first := make(chan error, 1)
	second := make(chan error, 1)
	go func() { _, e := r.ReadAtContext(ctx1, make([]byte, 1), 0); first <- e }()
	<-started
	go func() { _, e := r.ReadAtContext(ctx2, make([]byte, 1), 0); second <- e }()
	time.Sleep(20 * time.Millisecond) // let the second read join the first fill
	cancel1()
	if err = <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("first read error = %v", err)
	}
	select {
	case <-started: // the live waiter took over and retried the canceled fill
	case <-time.After(time.Second):
		t.Fatal("live waiter did not retry canceled fill")
	}
	if err = <-second; err != nil {
		t.Fatalf("independent read failed: %v", err)
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
	if err = <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("range read error = %v, want context.Canceled", err)
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
