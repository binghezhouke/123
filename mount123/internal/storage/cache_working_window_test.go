package storage

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/workqueue"
)

// Model a nearly full durable cache from earlier browsing, then prefetch a
// small new image working set twice. It fits easily if old hot data can age.
func TestCacheHotHistoryDoesNotEvictEveryNewPrefetch(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprint(restart), func(t *testing.T) { testCacheWorkingWindow(t, restart) })
	}
}

func TestGrowingHotAccountingSurvivesPublicationAndRemoval(t *testing.T) {
	c, err := NewCache(t.TempDir(), 100)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	finish := make(chan struct{})
	h, err := c.AcquireGrowing(ctx, ctx, "stream-image", 8, func(ctx context.Context, w io.Writer) error {
		if _, err := io.WriteString(w, "abcd"); err != nil {
			return err
		}
		select {
		case <-finish:
		case <-ctx.Done():
			return ctx.Err()
		}
		_, err := io.WriteString(w, "efgh")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	for i := 0; i < 2; i++ {
		if _, err := h.ReadAt(ctx, make([]byte, 4), 0); err != nil {
			t.Fatal(err)
		}
	}
	close(finish)
	for c.Stats().ReservedBytes != 0 {
		if ctx.Err() != nil {
			t.Fatal("stream was not published")
		}
		time.Sleep(time.Millisecond)
	}
	if stats := c.Stats(); stats.HotUsedBytes != 8 || stats.Classes[cacheHot].Bytes != 8 {
		t.Fatalf("stream hot accounting: %+v", stats)
	}
	h.Close()
	if err := c.Remove("stream-image"); err != nil {
		t.Fatal(err)
	}
	if c.Stats().HotUsedBytes != 0 {
		t.Fatal("removal leaked hot occupancy")
	}
}

func testCacheWorkingWindow(t *testing.T, restart bool) {
	dir := t.TempDir()
	c, err := NewCache(dir, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { c.Close() }()
	ctx := context.Background()
	fill := func(_ context.Context, w io.Writer) error { _, err := io.WriteString(w, "abcd"); return err }
	for i := 0; i < 24; i++ {
		h, err := c.Acquire(ctx, fmt.Sprintf("old-hot-%d", i), 4, fill)
		if err != nil {
			t.Fatal(err)
		}
		for j := 0; j < 2; j++ {
			if _, err := h.ReadAt(make([]byte, 4), 0); err != nil {
				t.Fatal(err)
			}
		}
		if err := h.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.StoreArchiveIndex(ctx, "protected-index", []byte("meta")); err != nil {
		t.Fatal(err)
	}
	if restart {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		c, err = NewCache(dir, 100)
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := c.Stats(); got.HotUsedBytes != 96 || got.HotBudgetBytes != 76 {
		t.Fatalf("hot history accounting before pressure: %+v", got)
	}
	// Keep one useful entry open throughout admission. Pressure must never
	// remove an active handle, even when its class is above the budget.
	pinned, err := c.Open("old-hot-0")
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.Close()
	bg := workqueue.Background(ctx)
	fills := 0
	for pass := 0; pass < 2; pass++ {
		for i := 0; i < 4; i++ {
			h, err := c.Acquire(bg, fmt.Sprintf("new-image-%d", i), 4, func(ctx context.Context, w io.Writer) error {
				fills++
				return fill(ctx, w)
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := h.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if fills != 4 {
		t.Fatalf("new image working set refetched: %d fills, want 4", fills)
	}
	index, err := c.OpenArchiveIndex("protected-index")
	if err != nil {
		t.Fatal("new working set evicted index:", err)
	}
	index.Close()
	data := make([]byte, 4)
	if _, err := pinned.ReadAt(data, 0); err != nil || string(data) != "abcd" {
		t.Fatalf("pinned data lost: %q %v", data, err)
	}
	stats := c.Stats()
	if stats.HotUsedBytes != stats.Classes[cacheHot].Bytes || stats.UsedBytes+stats.ReservedBytes > stats.CapacityBytes {
		t.Fatalf("inconsistent pressure accounting: %+v", stats)
	}
	// Recent hot data within the budget still outlives a larger one-pass scan.
	for i := 0; i < 40; i++ {
		h, err := c.Acquire(bg, fmt.Sprintf("scan-%d", i), 4, fill)
		if err != nil {
			t.Fatal(err)
		}
		h.Close()
	}
	hot, err := c.Open("old-hot-23")
	if err != nil {
		t.Fatal("within-budget hot data lost to scan:", err)
	}
	hot.Close()
}
