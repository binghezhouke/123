package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/workqueue"
)

func sourceContext(letter string) context.Context {
	return WithCacheOwner(context.Background(), strings.Repeat(letter, 32))
}

func TestSourceQuotaEvictsItsOwnDataAndPreservesOtherSources(t *testing.T) {
	c, err := NewCache(t.TempDir(), 500)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.ConfigureSourceLimit(20); err != nil {
		t.Fatal(err)
	}
	if err := c.Store(sourceContext("b"), "other", bytes.Repeat([]byte{2}, 80)); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"first", "second", "third"} {
		if err := c.Store(sourceContext("a"), key, bytes.Repeat([]byte{1}, 60)); err != nil {
			t.Fatal(err)
		}
	}
	if h, err := c.OpenCached("other", 80); err != nil || h == nil {
		t.Fatalf("other source evicted: %v", err)
	} else {
		h.Close()
	}
	if h, err := c.OpenCached("first", 60); err != nil || h != nil {
		t.Fatalf("old source block retained: %v", err)
	}
	s := c.Stats()
	if s.UsedBytes != 140 || s.SourceEvictions != 2 || s.LargestSourceBytes != 80 || s.SourceLimitBytes != 100 {
		t.Fatalf("bad source accounting: %+v", s)
	}
}

func TestSourceQuotaPinsAndReservationsConvergeOnRelease(t *testing.T) {
	c, err := NewCache(t.TempDir(), 500)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.ConfigureSourceLimit(20); err != nil {
		t.Fatal(err)
	}
	ctx := sourceContext("a")
	fill := func(_ context.Context, w io.Writer) error { _, err := w.Write(make([]byte, 60)); return err }
	first, err := c.Acquire(ctx, "first", 60, fill)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.Acquire(ctx, "second", 60, fill)
	if err != nil {
		t.Fatal(err)
	}
	if c.Stats().SourceOverLimitCount != 1 {
		t.Fatal("active overflow must be visible")
	}
	if _, err := first.ReadAt(make([]byte, 60), 0); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if s := c.Stats(); s.SourceOverLimitCount != 0 || s.UsedBytes != 60 {
		t.Fatalf("release did not trim: %+v", s)
	}
	second.Close()
	started, finish := make(chan struct{}), make(chan struct{})
	result := make(chan error, 1)
	go func() {
		_, err := c.Acquire(ctx, "cancelled", 70, func(context.Context, io.Writer) error { close(started); <-finish; return context.Canceled })
		result <- err
	}()
	<-started
	if c.Stats().LargestSourceReservedBytes != 70 {
		t.Fatal("missing source reservation")
	}
	close(finish)
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if s := c.Stats(); s.ReservedBytes != 0 || s.SourceCount != 0 {
		t.Fatalf("cancelled reservation leaked: %+v", s)
	}
}

func TestSourceQuotaSurvivesRestartAndAdoptsLegacyRanges(t *testing.T) {
	dir := t.TempDir()
	c, err := NewCache(dir, 500)
	if err != nil {
		t.Fatal(err)
	}
	identity := strings.Repeat("c", 64)
	for i := int64(0); i < 3; i++ {
		h, err := c.AcquireRange(context.Background(), identity, i*60, (i+1)*60, func(_ context.Context, w io.Writer) error { _, err := w.Write(make([]byte, 60)); return err })
		if err != nil {
			t.Fatal(err)
		}
		h.Close()
	}
	r := &Remote{cache: c, rangeID: identity}
	if err := r.SetCacheOwner(strings.Repeat("a", 32)); err != nil {
		t.Fatal(err)
	}
	if err := c.Store(sourceContext("a"), "inner:block:0", make([]byte, 60)); err != nil {
		t.Fatal(err)
	}
	if err := c.StoreArchiveIndex(sourceContext("a"), "archive-index:synthetic", make([]byte, 40)); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err = NewCache(dir, 500)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if s := c.Stats(); s.SourceCount != 1 || s.LargestSourceBytes != 240 {
		t.Fatalf("lost owner across reopen: %+v", s)
	}
	h, err := c.AcquireRange(context.Background(), identity, 180, 190, func(_ context.Context, w io.Writer) error {
		_, err := w.Write(make([]byte, 10))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	h.Close()
	if s := c.Stats(); s.SourceCount != 1 || s.LargestSourceBytes != 250 {
		t.Fatalf("new range lost restored ownership: %+v", s)
	}
	if err := c.ConfigureSourceLimit(20); err != nil {
		t.Fatal(err)
	}
	if s := c.Stats(); s.LargestSourceBytes != 70 || s.IndexUsedBytes != 40 {
		t.Fatalf("restart quota did not preserve index: %+v", s)
	}
	h, err = c.OpenCachedContext(sourceContext("a"), "inner:block:0", 60)
	if err != nil || h == nil {
		t.Fatalf("newest decoded block lost: %v", err)
	}
	h.Close()
}

func TestSourceQuotaGrowingCompletionAndFailureReleaseReservations(t *testing.T) {
	c, err := NewCache(t.TempDir(), 500)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.ConfigureSourceLimit(20); err != nil {
		t.Fatal(err)
	}
	ctx := sourceContext("a")
	started, finish := make(chan struct{}), make(chan struct{})
	h, err := c.AcquireGrowingRetained(ctx, context.Background(), "growing", 120, func(ctx context.Context, w io.Writer) error {
		if cacheOwner(ctx) != strings.Repeat("a", 32) {
			return errors.New("producer lost ownership")
		}
		close(started)
		<-finish
		_, err := w.Write(make([]byte, 120))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if c.Stats().LargestSourceReservedBytes != 120 {
		t.Fatal("missing growing reservation")
	}
	h.Close()
	close(finish)
	<-h.Done()
	if !h.Materialized() {
		t.Fatal("retained fill failed")
	}
	if s := c.Stats(); s.UsedBytes != 0 || s.ReservedBytes != 0 || s.SourceCount != 0 {
		t.Fatalf("unowned retained completion leaked: %+v", s)
	}
	h, err = c.AcquireGrowing(ctx, context.Background(), "failed", 120, func(context.Context, io.Writer) error {
		return io.ErrUnexpectedEOF
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Wait(context.Background()); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("failed fill: %v", err)
	}
	h.Close()
	if s := c.Stats(); s.ReservedBytes != 0 || s.SourceCount != 0 {
		t.Fatalf("failed growing reservation leaked: %+v", s)
	}
}

func TestSourceQuotaPrefetchStopsWhenPinnedDataPreventsRetention(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("ETag", `"fixture"`)
		http.ServeContent(w, r, "data.bin", time.Unix(100, 0), bytes.NewReader(make([]byte, 200)))
	}))
	defer server.Close()
	c, err := NewCache(t.TempDir(), 500)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.ConfigureSourceLimit(20); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(sourceContext("a"), 3*time.Second)
	defer cancel()
	r, err := NewRemote(ctx, c, "synthetic-remote", 200, func(context.Context) (string, error) { return server.URL, nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetCacheOwner(cacheOwner(ctx)); err != nil {
		t.Fatal(err)
	}
	h, err := c.Acquire(ctx, "pinned", 80, func(_ context.Context, w io.Writer) error { _, err := w.Write(make([]byte, 80)); return err })
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if err := r.PrefetchRangeAtContext(workqueue.Background(ctx), 0, 60); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("prefetch should stop on quota pressure: %v", err)
	}
	if n := requests.Load(); n > 2 {
		t.Fatalf("repeated evicted download: %d requests", n)
	}
	if s := c.Stats(); s.SourceOverLimitCount != 0 || s.ReservedBytes != 0 {
		t.Fatalf("prefetch leaked: %+v", s)
	}
}

func TestSourceQuotaDisabledAndValidation(t *testing.T) {
	c, err := NewCache(t.TempDir(), 500)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, v := range []int{-1, 101} {
		if c.ConfigureSourceLimit(v) == nil {
			t.Fatal("invalid percent accepted")
		}
	}
	if err := c.ConfigureSourceLimit(0); err != nil {
		t.Fatal(err)
	}
	if err := c.Store(sourceContext("a"), "large", make([]byte, 300)); err != nil {
		t.Fatal(err)
	}
	if c.Stats().UsedBytes != 300 || c.SourceCapacity() != 500 {
		t.Fatal("disabled limit still enforced")
	}
}

func TestSourceQuotaGrowingPromotionPersistsOwner(t *testing.T) {
	dir := t.TempDir()
	c, err := NewCache(dir, 500)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	finish := make(chan struct{})
	h, err := c.AcquireGrowing(sourceContext("a"), context.Background(), "promoted", 80, func(_ context.Context, w io.Writer) error {
		_, err := w.Write(make([]byte, 80))
		<-finish
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	for i := 0; i < 2; i++ {
		if _, err := h.ReadAt(context.Background(), make([]byte, 1), 0); err != nil {
			close(finish)
			t.Fatal(err)
		}
	}
	close(finish)
	if err := h.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.Close()
	c.Close()
	c, err = NewCache(dir, 500)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if s := c.Stats(); s.SourceCount != 1 || s.LargestSourceBytes != 80 || s.UnattributedDataBytes != 0 {
		t.Fatalf("class promotion lost durable ownership: %+v", s)
	}
}

func TestSourceQuotaEmptyMemberSurvivesCancelledSibling(t *testing.T) {
	c, err := NewCache(t.TempDir(), 500)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := sourceContext("a")
	if err := c.Store(ctx, "empty", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Acquire(ctx, "cancelled", 20, func(context.Context, io.Writer) error { return context.Canceled }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	h, err := c.OpenCachedContext(ctx, "empty", 0)
	if err != nil || h == nil {
		t.Fatalf("empty member: %v", err)
	}
	h.Close()
	if s := c.Stats(); s.SourceCount != 1 || s.ReservedBytes != 0 {
		t.Fatalf("empty entry accounting: %+v", s)
	}
}
