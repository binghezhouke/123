package storage

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/workqueue"
)

func windowTestRemote(t *testing.T, size int64, handler http.HandlerFunc) (*Cache, *Remote) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	cache, err := NewCache(t.TempDir(), 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cache.Close() })
	remote, err := NewRemote(context.Background(), cache, "window-fixture", size, func(context.Context) (string, error) { return server.URL, nil })
	if err != nil {
		t.Fatal(err)
	}
	return cache, remote
}
func rangeWindowHeaders(t *testing.T, w http.ResponseWriter, r *http.Request, size int64) (int64, int64) {
	t.Helper()
	var a, b int64
	if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &a, &b); err != nil {
		t.Error(err)
	}
	w.Header().Set("ETag", `"window"`)
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, size))
	return a, b
}
func TestRangeWindowProgressBeforeTailAndCloseReleasesBudgets(t *testing.T) {
	const size = 8 << 20
	started := make(chan struct{}, 1)
	var mu sync.Mutex
	var ranges [][2]int64
	cache, remote := windowTestRemote(t, size, func(w http.ResponseWriter, r *http.Request) {
		a, b := rangeWindowHeaders(t, w, r, size)
		w.Header().Set("Content-Length", strconv.FormatInt(b-a+1, 10))
		w.WriteHeader(206)
		if a == b {
			_, _ = w.Write([]byte{'x'})
			return
		}
		mu.Lock()
		ranges = append(ranges, [2]int64{a, b})
		mu.Unlock()
		_, _ = w.Write(bytes.Repeat([]byte{'x'}, 32))
		w.(http.Flusher).Flush()
		started <- struct{}{}
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	window, err := remote.OpenRangeWindow(workqueue.Background(ctx), 0, size)
	if err != nil {
		t.Fatal(err)
	}
	defer window.Close()
	<-started
	got := make([]byte, 32)
	if n, err := window.ReadAtContext(ctx, got, 0); err != nil || n != 32 || !bytes.Equal(got, bytes.Repeat([]byte{'x'}, 32)) {
		t.Fatalf("prefix=%d/%v", n, err)
	}
	if cache.DownloadStats().Promotions == 0 {
		t.Fatal("foreground demand did not promote speculative window")
	}
	window.Close()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		s := cache.DownloadStats()
		if s.ActiveRequests == 0 && s.StagingActiveBytes == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	s := cache.DownloadStats()
	if s.ActiveRequests != 0 || s.StagingActiveBytes != 0 {
		t.Fatalf("window close leaked budgets: %+v", s)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ranges) != 1 || ranges[0] != [2]int64{0, size - 1} {
		t.Fatalf("small demand fragmented registered window: %v", ranges)
	}
}

func TestRangeWindowWaitRejectsErrorAfterAllDemandBytes(t *testing.T) {
	data := bytes.Repeat([]byte{'a'}, 64<<10)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	cache, remote := windowTestRemote(t, int64(len(data)), func(w http.ResponseWriter, r *http.Request) {
		a, b := rangeWindowHeaders(t, w, r, int64(len(data)))
		w.WriteHeader(206)
		if a == b {
			_, _ = w.Write(data[a : b+1])
			return
		}
		w.(http.Flusher).Flush()
		_, _ = w.Write(data[a : b+1])
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			return
		case <-release:
		}
		_, _ = w.Write([]byte{'!'})
		w.(http.Flusher).Flush()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	window, err := remote.OpenRangeWindow(ctx, 0, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	defer window.Close()
	got := make([]byte, len(data))
	if n, err := window.ReadAtContext(ctx, got, 0); err != nil || n != len(got) {
		t.Fatalf("demand=%d/%v", n, err)
	}
	unblock()
	if err := window.Wait(ctx); err == nil || !strings.Contains(err.Error(), "body length mismatch") {
		t.Fatalf("late HTTP validation result: %v", err)
	}
	if len(cache.missingRanges(remote.rangeID, 0, int64(len(data)))) == 0 {
		t.Fatal("invalid response published")
	}
}

func TestRangeWindowTracksDemandAfterNoopFlightLosesCoverage(t *testing.T) {
	data := bytes.Repeat([]byte{'b'}, 64<<10)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	cache, remote := windowTestRemote(t, int64(len(data)), func(w http.ResponseWriter, r *http.Request) {
		a, b := rangeWindowHeaders(t, w, r, int64(len(data)))
		w.WriteHeader(206)
		if a == b {
			_, _ = w.Write(data[a : b+1])
			return
		}
		w.(http.Flusher).Flush()
		_, _ = w.Write(data[a : b+1])
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			return
		case <-release:
		}
		_, _ = w.Write([]byte{'!'})
		w.(http.Flusher).Flush()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// Reproduce publication between the coverage snapshot and flight execution:
	// a no-op flight sees a completed extent, without ever pinning it itself.
	h, err := cache.AcquireRange(ctx, remote.rangeID, 0, int64(len(data)), fillBytes(data))
	if err != nil {
		t.Fatal(err)
	}
	h.Close()
	f, owner, err := cache.beginRangeFlight(ctx, remote.rangeID, 0, int64(len(data)))
	if err != nil || !owner {
		t.Fatalf("begin=%v/%v", owner, err)
	}
	go remote.runRangeFlight(f)
	<-f.done
	if f.err != nil {
		t.Fatal(f.err)
	}
	wctx, wcancel := context.WithCancel(ctx)
	window := &RangeWindow{remote: remote, ctx: wctx, cancel: wcancel, start: 0, end: int64(len(data)), flights: []*rangeFlight{f}, tracked: map[*rangeFlight]bool{f: true}}
	defer window.Close()
	// The unpinned extent can be evicted before the first demand arrives.
	if err := cache.Remove(rangeKey(remote.rangeID, 0, int64(len(data)))); err != nil {
		t.Fatal(err)
	}
	if n, err := window.ReadAtContext(ctx, make([]byte, len(data)), 0); err != nil || n != len(data) {
		t.Fatalf("demand=%d/%v", n, err)
	}
	unblock()
	if err := window.Wait(ctx); err == nil || !strings.Contains(err.Error(), "body length mismatch") {
		t.Fatalf("new demand flight escaped Wait: %v", err)
	}
}
