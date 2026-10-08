package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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
	continueResponse := make(chan struct{})
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
			select {
			case <-continueResponse:
			case <-req.Context().Done():
				return
			}
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
	if calls.Load() != 2 {
		t.Fatalf("shared fill restarted %d HTTP requests", calls.Load()-1)
	}
	close(continueResponse)
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

func TestRemoteRangeWindowsSharePagesWithMetadataAndData(t *testing.T) {
	data := []byte(strings.Repeat("range-window-data-", int((8<<20)/18+1)))[:8<<20]
	var mu sync.Mutex
	var ranges []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		rangeHeader := req.Header.Get("Range")
		mu.Lock()
		ranges = append(ranges, rangeHeader)
		mu.Unlock()
		var a, b int64
		if _, err := fmt.Sscanf(rangeHeader, "bytes=%d-%d", &a, &b); err != nil {
			http.Error(w, "bad range", 400)
			return
		}
		w.Header().Set("ETag", `"window-v1"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, len(data)))
		w.Header().Set("Content-Length", strconv.FormatInt(b-a+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[a : b+1])
	}))
	defer s.Close()
	c, err := NewCache(t.TempDir(), 10<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r, err := NewRemote(context.Background(), c, "window-sharing", int64(len(data)), func(context.Context) (string, error) { return s.URL, nil })
	if err != nil {
		t.Fatal(err)
	}

	window := make([]byte, 3*int(remoteCachePageSize))
	windowOff := 2*remoteCachePageSize + 123
	if n, err := r.ReadRangeAtContext(context.Background(), window, windowOff); err != nil || n != len(window) {
		t.Fatalf("window read = %d, %v", n, err)
	}
	if !bytes.Equal(window, data[windowOff:windowOff+int64(len(window))]) {
		t.Fatal("window bytes differ")
	}
	before := countRemoteRanges(&mu, &ranges)
	ordinary := make([]byte, 200)
	if n, err := r.ReadAt(ordinary, windowOff+remoteCachePageSize+99); err != nil || n != len(ordinary) {
		t.Fatalf("ordinary read = %d, %v", n, err)
	}
	if !bytes.Equal(ordinary, data[windowOff+remoteCachePageSize+99:windowOff+remoteCachePageSize+99+int64(len(ordinary))]) {
		t.Fatal("ordinary bytes differ")
	}
	if got := countRemoteRanges(&mu, &ranges); got != before {
		t.Fatalf("ordinary read refetched window data: ranges %d -> %d", before, got)
	}

	// A sparse metadata read populates a page that a later window read reuses.
	metadataOff := 6*remoteCachePageSize + 17
	metadata := make([]byte, 100)
	if n, err := r.ReadMetadataAtContext(context.Background(), metadata, metadataOff); err != nil || n != len(metadata) {
		t.Fatalf("metadata read = %d, %v", n, err)
	}
	waitUntil(t, time.Second, func() bool { st := c.DownloadStats(); return st.ActiveRequests == 0 && st.StagingActiveBytes == 0 })
	before = countRemoteRanges(&mu, &ranges)
	window2 := make([]byte, int(remoteCachePageSize))
	if n, err := r.ReadRangeAtContext(context.Background(), window2, 6*remoteCachePageSize); err != nil || n != len(window2) {
		t.Fatalf("overlapping window read = %d, %v", n, err)
	}
	if got := countRemoteRanges(&mu, &ranges); got != before {
		t.Fatalf("window failed to reuse metadata page: ranges %d -> %d", before, got)
	}

	// A normal cold read keeps its 1 MiB request granularity; sparse metadata
	// and a later exact window over those bytes should both reuse its pages.
	cold := make([]byte, 32)
	if n, err := r.ReadAt(cold, 0); err != nil || n != len(cold) {
		t.Fatalf("cold ordinary read = %d, %v", n, err)
	}
	before = countRemoteRanges(&mu, &ranges)
	if n, err := r.ReadMetadataAtContext(context.Background(), metadata, remoteCachePageSize+7); err != nil || n != len(metadata) {
		t.Fatalf("metadata reuse read = %d, %v", n, err)
	}
	if n, err := r.ReadRangeAtContext(context.Background(), window2, remoteCachePageSize); err != nil || n != len(window2) {
		t.Fatalf("ordinary overlap window = %d, %v", n, err)
	}
	if got := countRemoteRanges(&mu, &ranges); got != before {
		t.Fatalf("metadata/window failed to reuse ordinary pages: ranges %d -> %d", before, got)
	}
}

func TestRemoteRangeWindowFetchesOnlyMissingOverlap(t *testing.T) {
	data := []byte(strings.Repeat("0123456789abcdef", (2<<20)/16))
	var mu sync.Mutex
	var ranges []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		rangeHeader := req.Header.Get("Range")
		mu.Lock()
		ranges = append(ranges, rangeHeader)
		mu.Unlock()
		var a, b int64
		if _, err := fmt.Sscanf(rangeHeader, "bytes=%d-%d", &a, &b); err != nil {
			http.Error(w, "bad range", 400)
			return
		}
		w.Header().Set("ETag", `"overlap-v1"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, len(data)))
		w.Header().Set("Content-Length", strconv.FormatInt(b-a+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[a : b+1])
	}))
	defer s.Close()
	c, err := NewCache(t.TempDir(), 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r, err := NewRemote(context.Background(), c, "overlap-sharing", int64(len(data)), func(context.Context) (string, error) { return s.URL, nil })
	if err != nil {
		t.Fatal(err)
	}
	page := remoteCachePageSize
	first := make([]byte, 4*int(page))
	if n, err := r.ReadRangeAtContext(context.Background(), first, 8*page); err != nil || n != len(first) {
		t.Fatalf("first range read = %d, %v", n, err)
	}
	before := countRemoteRanges(&mu, &ranges)
	second := make([]byte, 4*int(page))
	if n, err := r.ReadRangeAtContext(context.Background(), second, 10*page); err != nil || n != len(second) {
		t.Fatalf("overlap read = %d, %v", n, err)
	}
	if !bytes.Equal(second, data[10*page:14*page]) {
		t.Fatal("overlap bytes differ")
	}
	if got := countRemoteRanges(&mu, &ranges); got != before+1 {
		t.Fatalf("partial overlap made %d requests, want one missing run", got-before)
	}
	mu.Lock()
	last := ranges[len(ranges)-1]
	mu.Unlock()
	if want := fmt.Sprintf("bytes=%d-%d", 12*page, 14*page-1); last != want {
		t.Fatalf("missing range = %q, want %q", last, want)
	}
}

func countRemoteRanges(mu *sync.Mutex, ranges *[]string) int {
	mu.Lock()
	defer mu.Unlock()
	// Include the byte-zero construction probe in the same count.
	return len(*ranges)
}

func TestRemoteMigratesLegacyDataBlockIntoSharedPages(t *testing.T) {
	data := []byte(strings.Repeat("legacy-cache", int(remoteBlockSize/12)))[:remoteBlockSize]
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		var a, b int64
		if _, err := fmt.Sscanf(req.Header.Get("Range"), "bytes=%d-%d", &a, &b); err != nil {
			http.Error(w, "bad range", 400)
			return
		}
		w.Header().Set("ETag", `"legacy-v1"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, len(data)))
		w.Header().Set("Content-Length", strconv.FormatInt(b-a+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[a : b+1])
	}))
	defer s.Close()
	c, err := NewCache(t.TempDir(), 3*remoteBlockSize)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r, err := NewRemote(context.Background(), c, "legacy-migration", int64(len(data)), func(context.Context) (string, error) { return s.URL, nil })
	if err != nil {
		t.Fatal(err)
	}
	oldKey := fmt.Sprintf("remote:%s:0:%d", r.key, len(data))
	old, err := c.Acquire(context.Background(), oldKey, int64(len(data)), func(_ context.Context, w io.Writer) error { _, e := w.Write(data); return e })
	if err != nil {
		t.Fatal(err)
	}
	_ = old.Close()
	before := calls.Load()
	out := make([]byte, 128)
	if n, err := r.ReadAt(out, 123); err != nil || n != len(out) {
		t.Fatalf("legacy read = %d, %v", n, err)
	}
	if !bytes.Equal(out, data[123:123+len(out)]) {
		t.Fatal("legacy cache bytes differ")
	}
	if calls.Load() != before {
		t.Fatalf("legacy cache migration made HTTP request: %d -> %d", before, calls.Load())
	}
}

func TestRemoteOverlappingRangeFlightsDeduplicateAndKeepMissingTail(t *testing.T) {
	data := []byte(strings.Repeat("overlap-flight", int((1<<20)/14+1)))[:1<<20]
	var mu sync.Mutex
	var ranges []string
	started := make(chan struct{})
	release := make(chan struct{})
	var dataRequests atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		rangeHeader := req.Header.Get("Range")
		var a, b int64
		if _, err := fmt.Sscanf(rangeHeader, "bytes=%d-%d", &a, &b); err != nil {
			http.Error(w, "bad range", 400)
			return
		}
		mu.Lock()
		ranges = append(ranges, rangeHeader)
		mu.Unlock()
		if !(a == 0 && b == 0) && dataRequests.Add(1) == 1 {
			close(started)
			<-release
		}
		w.Header().Set("ETag", `"overlap-flight-v1"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, len(data)))
		w.Header().Set("Content-Length", strconv.FormatInt(b-a+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[a : b+1])
	}))
	defer s.Close()
	c, err := NewCache(t.TempDir(), 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	resolve := func(context.Context) (string, error) { return s.URL, nil }
	r, err := NewRemote(context.Background(), c, "overlap-flight", int64(len(data)), resolve)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := NewRemote(context.Background(), c, "overlap-flight", int64(len(data)), resolve)
	if err != nil {
		t.Fatal(err)
	}
	page := remoteCachePageSize
	first := make([]byte, 2*int(page))
	second := make([]byte, 2*int(page))
	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)
	go func() { _, e := r.ReadRangeAtContext(context.Background(), first, 0); firstDone <- e }()
	<-started
	go func() { _, e := r2.ReadRangeAtContext(context.Background(), second, page); secondDone <- e }()
	time.Sleep(25 * time.Millisecond)
	close(release)
	if err = <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err = <-secondDone; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, data[:len(first)]) || !bytes.Equal(second, data[page:3*page]) {
		t.Fatal("overlap flight returned wrong data")
	}
	mu.Lock()
	got := append([]string(nil), ranges...)
	mu.Unlock()
	if len(got) != 4 {
		t.Fatalf("HTTP ranges = %v, want two probes plus two fills", got)
	}
	if got[2] != fmt.Sprintf("bytes=0-%d", 2*page-1) || got[3] != fmt.Sprintf("bytes=%d-%d", 2*page, 3*page-1) {
		t.Fatalf("ranges = %v, want shared overlap merged into missing tail", got)
	}
}

func TestRemoteInterleavedReaderFillsPrefixBeforeJoiningLaterFlight(t *testing.T) {
	data := bytes.Repeat([]byte("interleaved-range-"), (2<<20)/18+1)[:2<<20]
	startedLater, releaseLater := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var ranges []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var a, b int64
		if _, err := fmt.Sscanf(req.Header.Get("Range"), "bytes=%d-%d", &a, &b); err != nil {
			http.Error(w, "bad range", 400)
			return
		}
		mu.Lock()
		ranges = append(ranges, req.Header.Get("Range"))
		mu.Unlock()
		if a == 1<<20 {
			close(startedLater)
			<-releaseLater
		}
		w.Header().Set("ETag", `"interleave-v1"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, len(data)))
		w.Header().Set("Content-Length", strconv.FormatInt(b-a+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[a : b+1])
	}))
	defer s.Close()
	c, err := NewCache(t.TempDir(), 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r, err := NewRemote(context.Background(), c, "interleaved", int64(len(data)), func(context.Context) (string, error) { return s.URL, nil })
	if err != nil {
		t.Fatal(err)
	}
	prefetchDone := make(chan error, 1)
	go func() { prefetchDone <- r.PrefetchRangeAtContext(context.Background(), 1<<20, 1<<20) }()
	select {
	case <-startedLater:
	case <-time.After(time.Second):
		t.Fatal("later range did not start")
	}
	read := make([]byte, 2<<20)
	readDone := make(chan error, 1)
	go func() {
		n, e := r.ReadRangeAtContext(context.Background(), read, 0)
		if e == nil && n != len(read) {
			e = io.ErrUnexpectedEOF
		}
		readDone <- e
	}()
	deadline := time.Now().Add(time.Second)
	for {
		mu.Lock()
		sawPrefix := false
		for _, rg := range ranges {
			if rg == "bytes=0-1048575" {
				sawPrefix = true
				break
			}
		}
		mu.Unlock()
		if sawPrefix {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("reader did not fill the uncovered prefix before joining later flight")
		}
		time.Sleep(time.Millisecond)
	}
	close(releaseLater)
	if err := <-prefetchDone; err != nil {
		t.Fatal(err)
	}
	if err := <-readDone; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(read, data) {
		t.Fatal("interleaved read returned incorrect bytes")
	}
}

func TestRemoteDisjointRangeFlightsRunInParallel(t *testing.T) {
	data := []byte(strings.Repeat("parallel-range", int((1<<20)/14+1)))[:1<<20]
	started := make(chan string, 2)
	release := make(chan struct{})
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var a, b int64
		if _, e := fmt.Sscanf(req.Header.Get("Range"), "bytes=%d-%d", &a, &b); e != nil {
			http.Error(w, "bad range", 400)
			return
		}
		if calls.Add(1) > 1 {
			started <- req.Header.Get("Range")
			<-release
		}
		w.Header().Set("ETag", `"parallel-range-v1"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, len(data)))
		w.Header().Set("Content-Length", strconv.FormatInt(b-a+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[a : b+1])
	}))
	defer s.Close()
	c, e := NewCache(t.TempDir(), 4<<20)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	r, e := NewRemote(context.Background(), c, "parallel-range", int64(len(data)), func(context.Context) (string, error) { return s.URL, nil })
	if e != nil {
		t.Fatal(e)
	}
	page := remoteCachePageSize
	done := make(chan error, 2)
	for _, off := range []int64{0, 5 * page} {
		off := off
		go func() { _, err := r.ReadRangeAtContext(context.Background(), make([]byte, page), off); done <- err }()
	}
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case rg := <-started:
			seen[rg] = true
		case <-time.After(time.Second):
			close(release)
			t.Fatal("disjoint range requests were serialized")
		}
	}
	close(release)
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 2 {
		t.Fatalf("started ranges = %v", seen)
	}
}

func TestCachePinsAllRangeExtentsAgainstSmallCacheEviction(t *testing.T) {
	c, err := NewCache(t.TempDir(), 2*int64(remoteCachePageSize))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	identity := cacheID("small-cache-object")
	one := bytes.Repeat([]byte("a"), int(remoteCachePageSize))
	two := bytes.Repeat([]byte("b"), int(remoteCachePageSize))
	for i, data := range [][]byte{one, two} {
		start := int64(i) * remoteCachePageSize
		h, e := c.AcquireRange(context.Background(), identity, start, start+remoteCachePageSize, func(_ context.Context, w io.Writer) error { _, x := w.Write(data); return x })
		if e != nil {
			t.Fatal(e)
		}
		_ = h.Close()
	}
	parts, ok, err := c.pinRange(identity, 0, 2*remoteCachePageSize)
	if err != nil || !ok {
		t.Fatalf("pin range = %v, %v", ok, err)
	}
	thirdID := cacheID("other-object")
	if _, err = c.AcquireRange(context.Background(), thirdID, 0, remoteCachePageSize, fillBytes(one)); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("fill while both extents pinned = %v, want ENOSPC", err)
	}
	out := make([]byte, 2*int(remoteCachePageSize))
	if err = readPinnedRange(parts, out, 0); err != nil {
		t.Fatal(err)
	}
	closeRangeParts(parts)
	if !bytes.Equal(out[:len(one)], one) || !bytes.Equal(out[len(one):], two) {
		t.Fatal("pinned extents changed during eviction")
	}
}

func TestCacheRangeExtentsSurviveReopen(t *testing.T) {
	dir := t.TempDir()
	identity := cacheID("persistent-extent")
	c, err := NewCache(dir, 1024)
	if err != nil {
		t.Fatal(err)
	}
	h, err := c.AcquireRange(context.Background(), identity, 10, 20, fillBytes([]byte("0123456789")))
	if err != nil {
		t.Fatal(err)
	}
	_ = h.Close()
	_ = c.Close()
	c, err = NewCache(dir, 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if missing := c.missingRanges(identity, 10, 20); len(missing) != 0 {
		t.Fatalf("reopened range missing: %v", missing)
	}
	parts, ok, err := c.pinRange(identity, 10, 20)
	if err != nil || !ok {
		t.Fatalf("pin persisted extent = %v, %v", ok, err)
	}
	out := make([]byte, 10)
	err = readPinnedRange(parts, out, 10)
	closeRangeParts(parts)
	if err != nil || string(out) != "0123456789" {
		t.Fatalf("persisted bytes = %q, %v", out, err)
	}
}

func TestRemoveRangeCleansLRUAndCoverage(t *testing.T) {
	c, err := NewCache(t.TempDir(), remoteCachePageSize)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	identity := cacheID("remove-range")
	key := rangeKey(identity, 0, remoteCachePageSize)
	h, err := c.AcquireRange(context.Background(), identity, 0, remoteCachePageSize, fillBytes(bytes.Repeat([]byte("x"), int(remoteCachePageSize))))
	if err != nil {
		t.Fatal(err)
	}
	_ = h.Close()
	if err := c.Remove(key); err != nil {
		t.Fatal(err)
	}
	if missing := c.missingRanges(identity, 0, remoteCachePageSize); len(missing) != 1 || missing[0] != (byteRange{0, remoteCachePageSize}) {
		t.Fatalf("coverage after remove = %v", missing)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.used != 0 || c.lruLen() != 0 || len(c.ranges[identity]) != 0 {
		t.Fatalf("remove left stale state: used=%d lru=%d ranges=%v", c.used, c.lruLen(), c.ranges[identity])
	}
}

func TestRemoteRejectsOverlongChunkedRangeBody(t *testing.T) {
	data := []byte("0123456789")
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		call := calls.Add(1)
		var a, b int64
		if _, e := fmt.Sscanf(req.Header.Get("Range"), "bytes=%d-%d", &a, &b); e != nil {
			http.Error(w, "bad range", 400)
			return
		}
		w.Header().Set("ETag", `"long-v1"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		if call > 1 {
			w.(http.Flusher).Flush()
			_, _ = w.Write(data[a : b+1])
			_, _ = w.Write([]byte("x"))
			return
		}
		_, _ = w.Write(data[a : b+1])
	}))
	defer s.Close()
	c, err := NewCache(t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r, err := NewRemote(context.Background(), c, "long-body", int64(len(data)), func(context.Context) (string, error) { return s.URL, nil })
	if err != nil {
		t.Fatal(err)
	}
	if n, readErr := r.ReadAt(make([]byte, 2), 1); readErr != nil || n != 2 {
		t.Fatalf("progressive prefix read = %d, %v", n, readErr)
	}
	if err = r.ensureCachedRange(context.Background(), 0, int64(len(data))); err == nil || !strings.Contains(err.Error(), "body length mismatch") {
		t.Fatalf("complete overlong response error = %v", err)
	}
	if len(c.missingRanges(r.rangeID, 0, remoteCachePageSize)) == 0 {
		t.Fatal("overlong response was cached")
	}
}

func TestRemoteReadReturnsPrefixBeforeRangeTailAndPublishesAfterward(t *testing.T) {
	data := bytes.Repeat([]byte("progressive-range-"), (1<<20)/18+1)[:1<<20]
	started := make(chan struct{})
	releaseTail := make(chan struct{})
	var requests atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var a, b int64
		if _, err := fmt.Sscanf(req.Header.Get("Range"), "bytes=%d-%d", &a, &b); err != nil {
			http.Error(w, "bad range", 400)
			return
		}
		if requests.Add(1) == 1 {
			w.Header().Set("ETag", `"progress-v1"`)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, len(data)))
			w.Header().Set("Content-Length", strconv.FormatInt(b-a+1, 10))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(data[a : b+1])
			return
		}
		w.Header().Set("ETag", `"progress-v1"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, len(data)))
		w.Header().Set("Content-Length", strconv.FormatInt(b-a+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		prefixEnd := min(a+4095, b)
		_, _ = w.Write(data[a : prefixEnd+1])
		w.(http.Flusher).Flush()
		close(started)
		<-releaseTail
		_, _ = w.Write(data[prefixEnd+1 : b+1])
	}))
	defer s.Close()
	c, err := NewCache(t.TempDir(), 2<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r, err := NewRemote(context.Background(), c, "progressive-prefix", int64(len(data)), func(context.Context) (string, error) { return s.URL, nil })
	if err != nil {
		t.Fatal(err)
	}
	out := make([]byte, 8)
	readDone := make(chan struct {
		n   int
		err error
	}, 1)
	startedAt := time.Now()
	go func() {
		n, e := r.ReadAt(out, 0)
		readDone <- struct {
			n   int
			err error
		}{n, e}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("range response did not start")
	}
	select {
	case got := <-readDone:
		if got.err != nil || got.n != len(out) || !bytes.Equal(out, data[:len(out)]) {
			t.Fatalf("prefix read = %d, %v, bytes match=%v", got.n, got.err, bytes.Equal(out, data[:len(out)]))
		}
		t.Logf("progressive first-read latency: %s; baseline waits for the blocked tail", time.Since(startedAt))
	case <-time.After(time.Second):
		t.Fatal("small read waited for blocked Range tail")
	}
	if len(c.missingRanges(r.rangeID, 0, int64(len(data)))) == 0 {
		t.Fatal("incomplete response became cache-visible")
	}
	close(releaseTail)
	deadline := time.Now().Add(time.Second)
	for len(c.missingRanges(r.rangeID, 0, int64(len(data)))) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(c.missingRanges(r.rangeID, 0, int64(len(data)))) != 0 {
		t.Fatal("complete response was not published")
	}
	if got := c.IOStats().Snapshot().DownloadedBytes.Bytes; got == nil || *got != uint64(len(data)+1) {
		t.Fatalf("download bytes = %v, want probe plus complete range (%d)", got, len(data)+1)
	}
}

func TestRemoteReadDoesNotWaitForSlowCachePublicationAndSurvivesRestart(t *testing.T) {
	data := bytes.Repeat([]byte("slow-publication-"), 4096)[:64<<10]
	var dataRequests atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var a, b int64
		if _, err := fmt.Sscanf(req.Header.Get("Range"), "bytes=%d-%d", &a, &b); err != nil {
			http.Error(w, "bad range", 400)
			return
		}
		if a != 0 || b != 0 {
			dataRequests.Add(1)
		}
		w.Header().Set("ETag", `"slow-publish-v1"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, len(data)))
		w.Header().Set("Content-Length", strconv.FormatInt(b-a+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[a : b+1])
	}))
	defer s.Close()
	cacheDir := t.TempDir()
	c, err := NewCache(cacheDir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	syncStarted, releaseSync := make(chan struct{}), make(chan struct{})
	c.syncFile = func(f *os.File) error { close(syncStarted); <-releaseSync; return f.Sync() }
	r, err := NewRemote(context.Background(), c, "slow-publication", int64(len(data)), func(context.Context) (string, error) { return s.URL, nil })
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	readDone := make(chan error, 1)
	go func() {
		n, e := r.ReadAt(buf, 100)
		if e == nil && n != len(buf) {
			e = io.ErrUnexpectedEOF
		}
		readDone <- e
	}()
	select {
	case <-syncStarted:
	case <-time.After(time.Second):
		t.Fatal("cache publication did not reach Sync")
	}
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("reader waited for cache Sync")
	}
	if !bytes.Equal(buf, data[100:116]) {
		t.Fatal("reader returned wrong staged prefix")
	}
	if len(c.missingRanges(r.rangeID, 0, int64(len(data)))) == 0 {
		t.Fatal("cache entry visible before Sync completed")
	}
	close(releaseSync)
	deadline := time.Now().Add(time.Second)
	for len(c.missingRanges(r.rangeID, 0, int64(len(data)))) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if len(c.missingRanges(r.rangeID, 0, int64(len(data)))) != 0 {
		t.Fatal("completed cache entry missing")
	}
	before := dataRequests.Load()
	c2, err := NewCache(cacheDir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	r2, err := NewRemote(context.Background(), c2, "slow-publication", int64(len(data)), func(context.Context) (string, error) { return s.URL, nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r2.ReadAt(buf, 100); err != nil {
		t.Fatal(err)
	}
	if dataRequests.Load() != before {
		t.Fatalf("restart refetched data Range: %d -> %d", before, dataRequests.Load())
	}
}

func TestCacheCloseWaitsForProgressPublicationAndReleasesResources(t *testing.T) {
	data := bytes.Repeat([]byte("close-progress-"), (64<<10)/15+1)[:64<<10]
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var a, b int64
		if _, err := fmt.Sscanf(req.Header.Get("Range"), "bytes=%d-%d", &a, &b); err != nil {
			http.Error(w, "bad range", 400)
			return
		}
		w.Header().Set("ETag", `"close-progress-v1"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, len(data)))
		w.Header().Set("Content-Length", strconv.FormatInt(b-a+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[a : b+1])
	}))
	defer s.Close()
	dir := t.TempDir()
	c, err := NewCache(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	syncStarted, releaseSync := make(chan struct{}), make(chan struct{})
	c.syncFile = func(f *os.File) error { close(syncStarted); <-releaseSync; return f.Sync() }
	r, err := NewRemote(context.Background(), c, "close-progress", int64(len(data)), func(context.Context) (string, error) { return s.URL, nil })
	if err != nil {
		t.Fatal(err)
	}
	if n, err := r.ReadAt(make([]byte, 8), 0); err != nil || n != 8 {
		t.Fatalf("progress read = %d, %v", n, err)
	}
	select {
	case <-syncStarted:
	case <-time.After(time.Second):
		t.Fatal("flight did not reach publication")
	}
	closed := make(chan error, 1)
	closeStarted := make(chan struct{})
	go func() { close(closeStarted); closed <- c.Close() }()
	<-closeStarted
	select {
	case err := <-closed:
		t.Fatalf("Close returned before Sync gate released: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	close(releaseSync)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cache Close deadlocked with progress cleanup")
	}
	items, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if strings.HasPrefix(item.Name(), ".fill-") {
			t.Fatalf("staging file survived Close: %q", item.Name())
		}
	}
}

func TestProgressiveFirstReadLatencyAndDownloadedByteBaseline(t *testing.T) {
	data := bytes.Repeat([]byte("baseline-range-"), (1<<20)/15+1)[:1<<20]
	var serverBytes [2]atomic.Uint64
	var done [2]chan struct{}
	done[0], done[1] = make(chan struct{}), make(chan struct{})
	var completed [2]sync.Once
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		phase := 0
		if req.URL.Path == "/progressive" {
			phase = 1
		}
		var a, b int64
		if _, err := fmt.Sscanf(req.Header.Get("Range"), "bytes=%d-%d", &a, &b); err != nil {
			http.Error(w, "bad range", 400)
			return
		}
		w.Header().Set("ETag", `"baseline-v1"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, len(data)))
		w.Header().Set("Content-Length", strconv.FormatInt(b-a+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		if a == 0 && b == 0 {
			_, _ = w.Write(data[:1])
			return
		}
		prefixEnd := min(a+4095, b)
		n, _ := w.Write(data[a : prefixEnd+1])
		serverBytes[phase].Add(uint64(n))
		w.(http.Flusher).Flush()
		time.Sleep(40 * time.Millisecond)
		if prefixEnd < b {
			n, _ = w.Write(data[prefixEnd+1 : b+1])
			serverBytes[phase].Add(uint64(n))
		}
		completed[phase].Do(func() { close(done[phase]) })
	}))
	defer s.Close()
	newRemote := func(path string) (*Cache, *Remote) {
		t.Helper()
		c, err := NewCache(t.TempDir(), 2<<20)
		if err != nil {
			t.Fatal(err)
		}
		r, err := NewRemote(context.Background(), c, "latency-"+path, int64(len(data)), func(context.Context) (string, error) { return s.URL + path, nil })
		if err != nil {
			_ = c.Close()
			t.Fatal(err)
		}
		return c, r
	}

	baselineCache, baselineRemote := newRemote("/baseline")
	started := time.Now()
	if err := baselineRemote.PrefetchRangeAtContext(context.Background(), 0, remoteBlockSize); err != nil {
		t.Fatal(err)
	}
	baselineLatency := time.Since(started)
	if err := baselineCache.Close(); err != nil {
		t.Fatal(err)
	}

	progressCache, progressRemote := newRemote("/progressive")
	started = time.Now()
	if n, err := progressRemote.ReadAt(make([]byte, 8), 0); err != nil || n != 8 {
		t.Fatalf("progressive read = %d, %v", n, err)
	}
	progressiveLatency := time.Since(started)
	select {
	case <-done[1]:
	case <-time.After(time.Second):
		t.Fatal("progressive response did not finish")
	}
	baseBytes := serverBytes[0].Load() + 1 // include the one-byte entity probe
	progressBytes := serverBytes[1].Load() + 1
	if baseBytes != uint64(remoteBlockSize+1) || progressBytes != baseBytes {
		t.Fatalf("download byte baseline=%d progressive=%d, want both %d", baseBytes, progressBytes, remoteBlockSize+1)
	}
	t.Logf("local baseline full-fill latency=%s, progressive 8-byte latency=%s; downloaded bytes baseline=%d progressive=%d", baselineLatency, progressiveLatency, baseBytes, progressBytes)
	if err := progressCache.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteLargeWindowUsesOneCacheExtent(t *testing.T) {
	data := bytes.Repeat([]byte("large-window"), (16<<20)/12+1)[:16<<20]
	var dataRanges atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var a, b int64
		if _, err := fmt.Sscanf(req.Header.Get("Range"), "bytes=%d-%d", &a, &b); err != nil {
			http.Error(w, "bad range", 400)
			return
		}
		w.Header().Set("ETag", `"large-window-v1"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, len(data)))
		w.Header().Set("Content-Length", strconv.FormatInt(b-a+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[a : b+1])
		if !(a == 0 && b == 0) {
			dataRanges.Add(1)
		}
	}))
	defer s.Close()
	c, err := NewCache(t.TempDir(), 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r, err := NewRemote(context.Background(), c, "large-window", int64(len(data)), func(context.Context) (string, error) { return s.URL, nil })
	if err != nil {
		t.Fatal(err)
	}
	window := make([]byte, 16<<20)
	if n, err := r.ReadRangeAtContext(context.Background(), window, 0); err != nil || n != len(window) {
		t.Fatalf("large window = %d, %v", n, err)
	}
	if dataRanges.Load() != 1 {
		t.Fatalf("large window made %d HTTP range requests, want one", dataRanges.Load())
	}
	waitUntil(t, time.Second, func() bool { st := c.DownloadStats(); return st.ActiveRequests == 0 && st.StagingActiveBytes == 0 })
	c.mu.Lock()
	entries := len(c.entries)
	c.mu.Unlock()
	if entries != 1 {
		t.Fatalf("large window created %d cache files, want one extent", entries)
	}
}

func TestRemoteRangeLargerThanCacheStreamsWithoutRefetchLoop(t *testing.T) {
	const pages = 4
	data := bytes.Repeat([]byte("range-cache-pressure"), int(remoteCachePageSize*pages)/len("range-cache-pressure")+1)[:remoteCachePageSize*pages]
	var dataRequests atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var start, end int64
		if _, err := fmt.Sscanf(req.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}
		if start != 0 || end != 0 {
			dataRequests.Add(1)
		}
		w.Header().Set("ETag", `"capacity-pressure-v1"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
	}))
	defer s.Close()
	c, err := NewCache(t.TempDir(), 2*remoteCachePageSize)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r, err := NewRemote(context.Background(), c, "capacity-pressure", int64(len(data)), func(context.Context) (string, error) { return s.URL, nil })
	if err != nil {
		t.Fatal(err)
	}
	// Seed two disjoint extents so satisfying the intervening gaps under full
	// capacity can evict earlier bytes from the same requested range.
	for _, page := range []int64{0, 2} {
		seed := make([]byte, remoteCachePageSize)
		if n, err := r.ReadRangeAtContext(context.Background(), seed, page*remoteCachePageSize); err != nil || n != len(seed) {
			t.Fatalf("seed page %d = %d, %v", page, n, err)
		}
	}
	dataRequests.Store(0)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out := make([]byte, len(data))
	n, err := r.ReadRangeAtContext(ctx, out, 0)
	if err != nil || n != len(out) {
		t.Fatalf("range read = %d, %v", n, err)
	}
	if !bytes.Equal(out, data) {
		t.Fatal("range read returned incorrect bytes")
	}
	if got := dataRequests.Load(); got != 2 {
		t.Fatalf("data range requests = %d, want 2 cache-sized fetches", got)
	}
	waitUntil(t, time.Second, func() bool { st := c.DownloadStats(); return st.ActiveRequests == 0 && st.StagingActiveBytes == 0 })
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.used > c.max {
		t.Fatalf("cache disk use %d exceeds limit %d", c.used, c.max)
	}
	for id, entry := range c.entries {
		if entry.pins != 0 {
			t.Fatalf("published extent %s retained %d pins after readers finished", id, entry.pins)
		}
	}
}
