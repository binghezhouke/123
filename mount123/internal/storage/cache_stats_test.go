package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/workqueue"
)

func TestCacheStatsEvictionRefaultAndPinnedBytes(t *testing.T) {
	c, err := NewCache(t.TempDir(), 8)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.Store(context.Background(), "one", []byte("12345678")); err != nil {
		t.Fatal(err)
	}
	h, err := c.Open("one")
	if err != nil {
		t.Fatal(err)
	}
	var buf [1]byte
	for _, off := range []int64{0, 1, 0} {
		if _, err := h.ReadAt(buf[:], off); err != nil {
			t.Fatal(err)
		}
	}
	if got := c.Stats(); got.PinnedBytes != 8 || got.Classes[2].Bytes != 8 {
		t.Fatalf("pinned inventory = %+v", got)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Store(context.Background(), "two", []byte("abcdefgh")); err != nil {
		t.Fatal(err)
	}
	if err := c.Store(context.Background(), "one", []byte("12345678")); err != nil {
		t.Fatal(err)
	}
	s := c.Stats()
	if s.UsedBytes != 8 || s.Entries != 1 || s.CapacityEvictions != 2 || s.CapacityEvictionBytes != 16 {
		t.Fatalf("inventory/evictions = %+v", s)
	}
	if s.Classes[2].CapacityEvictions != 1 || s.Classes[2].CapacityEvictionBytes != 8 || s.Classes[1].CapacityEvictions != 1 {
		t.Fatalf("class evictions = %+v", s.Classes)
	}
	if s.Refaults.Count != 1 || s.Refaults.Bytes != 8 {
		t.Fatalf("refaults = %+v", s.Refaults)
	}
	if s.FillSuccesses != 3 || s.ENOSPC != 0 {
		t.Fatalf("fill/enospc counters = %+v", s)
	}
}

func TestCacheStatsENOSPCAndCoalescedFill(t *testing.T) {
	c, err := NewCache(t.TempDir(), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Store(context.Background(), "pinned", []byte("four")); err != nil {
		t.Fatal(err)
	}
	pinned, err := c.Open("pinned")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Acquire(context.Background(), "blocked", 4, fillBytes([]byte("data"))); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("Acquire error = %v, want ENOSPC", err)
	}
	_ = pinned.Close()

	started, release := make(chan struct{}), make(chan struct{})
	fill := func(ctx context.Context, w io.Writer) error {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		_, err := io.WriteString(w, "data")
		return err
	}
	var wg sync.WaitGroup
	results := make(chan *Handle, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, err := c.Acquire(context.Background(), "same", 4, fill)
			if err != nil {
				errs <- err
				return
			}
			results <- h
		}()
		if i == 0 {
			<-started
		}
	}
	deadline := time.Now().Add(time.Second)
	for c.Stats().ExistingFillWaits == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if c.Stats().ExistingFillWaits == 0 {
		t.Fatal("second acquisition did not join the fill")
	}
	close(release)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	for h := range results {
		_ = h.Close()
	}
	s := c.Stats()
	if s.ENOSPC != 1 || s.ExistingFillWaits != 1 || s.FillSuccesses != 2 {
		t.Fatalf("counters = %+v", s)
	}
}

func TestCacheStatsFillFailureAndRestart(t *testing.T) {
	dir := t.TempDir()
	c, err := NewCache(dir, 32)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Store(context.Background(), "resident", []byte("saved")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Acquire(context.Background(), "broken", 1, func(context.Context, io.Writer) error {
		return errors.New("fill failed")
	}); err == nil {
		t.Fatal("expected fill failure")
	}
	if got := c.Stats().FillFailures; got != 1 {
		t.Fatalf("FillFailures = %d", got)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err = NewCache(dir, 32)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s := c.Stats()
	if s.Status != "measured" || s.Entries != 1 || s.UsedBytes != 5 || s.FillSuccesses != 0 || s.FillFailures != 0 || s.CapacityEvictions != 0 {
		t.Fatalf("reopened stats = %+v", s)
	}
	h, err := c.Open("resident")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	got := make([]byte, 5)
	if _, err := h.ReadAt(got, 0); err != nil || !bytes.Equal(got, []byte("saved")) {
		t.Fatalf("restored value %q, %v", got, err)
	}
}

func TestRemoteCacheStatsSeparateForegroundBackgroundAndMetadata(t *testing.T) {
	data := []byte("abcdefghijklmnopqrstuvwxyz")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var start, end int64
		if _, err := fmt.Sscanf(req.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}
		w.Header().Set("ETag", `"stats-v1"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
	}))
	defer server.Close()
	c, err := NewCache(t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r, err := NewRemote(context.Background(), c, "stats", int64(len(data)), func(context.Context) (string, error) { return server.URL, nil })
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 10)
	if _, err := r.ReadAtContext(context.Background(), buf, 2); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, time.Second, func() bool {
		return c.Stats().Entries == 1 && c.DownloadStats().ActiveRequests == 0
	})
	if _, err := r.ReadAtContext(context.Background(), buf, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadMetadataAtContext(context.Background(), make([]byte, 3), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadAtContext(workqueue.Background(context.Background()), make([]byte, 4), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadRangeAtContext(context.Background(), make([]byte, 2), 0); err != nil {
		t.Fatal(err)
	}
	s := c.Stats()
	if s.Foreground.ReadRequests != 2 || s.Foreground.FullHits != 1 || s.Foreground.RequestedBytes != 20 || s.Foreground.HitBytes != 10 || s.Foreground.MissBytes != 10 {
		t.Fatalf("foreground range stats = %+v", s.Foreground)
	}
	if s.Background.ReadRequests != 1 || s.Background.FullHits != 1 || s.Background.RequestedBytes != 4 || s.Background.HitBytes != 4 || s.Background.MissBytes != 0 {
		t.Fatalf("background range stats = %+v", s.Background)
	}
	if s.FillSuccesses != 1 {
		t.Fatalf("cached repeat reads unexpectedly filled again: %+v", s)
	}
}

func TestRemoteCacheStatsCountPartialCoverageAcrossGapsAndOverlap(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 100)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var start, end int64
		if _, err := fmt.Sscanf(req.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}
		w.Header().Set("ETag", `"partial-stats-v1"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
	}))
	defer server.Close()
	c, err := NewCache(t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r, err := NewRemote(context.Background(), c, "partial-stats", int64(len(data)), func(context.Context) (string, error) { return server.URL, nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, extent := range [][2]int64{{20, 50}, {40, 70}, {80, 90}} {
		a, b := extent[0], extent[1]
		h, err := c.AcquireRange(context.Background(), r.rangeID, a, b, func(_ context.Context, w io.Writer) error {
			_, err := w.Write(data[a:b])
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := h.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.ReadAtContext(context.Background(), make([]byte, 100), 0); err != nil {
		t.Fatal(err)
	}
	s := c.Stats().Foreground
	if s.ReadRequests != 1 || s.FullHits != 0 || s.RequestedBytes != 100 || s.HitBytes != 60 || s.MissBytes != 40 {
		t.Fatalf("partial coverage stats = %+v; want 60 unique covered bytes", s)
	}
}

func TestCacheStatsGhostListIsBounded(t *testing.T) {
	c, err := NewCache(t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i := 0; i < maxCacheGhostIDs+2; i++ {
		if err := c.Store(context.Background(), fmt.Sprintf("key-%d", i), []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.ghost) != maxCacheGhostIDs || c.ghostLRU.Len() != maxCacheGhostIDs {
		t.Fatalf("ghost bounds = map:%d list:%d", len(c.ghost), c.ghostLRU.Len())
	}
}
