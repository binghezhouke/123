package storage

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/workqueue"
)

// A selected image can join a retained speculative decoder after the browser
// cancels its low-priority request. Its existing producer must gain access to
// reserved foreground capacity without losing the prefix already decoded.
func TestForegroundGrowingReaderPromotesQueuedProducer(t *testing.T) {
	for _, stage := range []string{"decode", "download", "staging"} {
		t.Run(stage, func(t *testing.T) {
			c, err := NewCache(t.TempDir(), 1024)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			bg := workqueue.Background(ctx)
			var acquire func(context.Context) (func(), error)
			var scheduler *downloadScheduler
			if stage == "decode" {
				gate := workqueue.New(2)
				release, err := gate.Acquire(bg)
				if err != nil {
					t.Fatal(err)
				}
				defer release()
				acquire = gate.Acquire
			} else {
				cfg := DownloadConfig{MaxRequests: 2, MaxInFlightBytes: 2, ForegroundReservedBytes: 1, RequestChunkBytes: 1}
				c.downloads, c.staging = newDownloadScheduler(cfg), newDownloadScheduler(cfg)
				scheduler = c.downloads
				if stage == "staging" {
					scheduler = c.staging
				}
				release, err := scheduler.Acquire(bg, 1, "unrelated", &downloadPriority{})
				if err != nil {
					t.Fatal(err)
				}
				defer release()
				acquire = func(fillCtx context.Context) (func(), error) {
					flight, _, err := c.beginRangeFlight(fillCtx, "synthetic-source", 0, 1)
					if err != nil {
						return nil, err
					}
					defer c.releaseRangeFlight(flight)
					release, err := scheduler.Acquire(flight.ctx, 1, "selected", flight.priority)
					c.finishRangeFlight("synthetic-source", flight, err)
					return release, err
				}
			}
			started := make(chan struct{})
			var fills atomic.Int32
			fill := func(fillCtx context.Context, w io.Writer) error {
				fills.Add(1)
				if _, err := io.WriteString(w, "a"); err != nil {
					return err
				}
				close(started)
				release, err := acquire(fillCtx)
				if err != nil {
					return err
				}
				defer release()
				_, err = io.WriteString(w, "b")
				return err
			}
			h, err := c.AcquireGrowingRetained(bg, bg, "selected-image", 2, fill)
			if err != nil {
				t.Fatal(err)
			}
			<-started
			if scheduler != nil {
				for scheduler.snapshot().WaitingBackground == 0 {
					if ctx.Err() != nil {
						t.Fatal("producer did not queue")
					}
					time.Sleep(time.Millisecond)
				}
			}
			if err := h.Close(); err != nil {
				t.Fatal(err)
			}
			selected, err := c.AcquireGrowingRetained(ctx, ctx, "selected-image", 2, fill)
			if err != nil {
				t.Fatal(err)
			}
			defer selected.Close()
			data := make([]byte, 2)
			if n, err := selected.ReadAt(ctx, data, 0); err != nil || n != 2 || string(data) != "ab" {
				t.Fatalf("selected image blocked behind unrelated background work: n=%d data=%q err=%v", n, data, err)
			}
			if fills.Load() != 1 {
				t.Fatalf("restarted producer: %d fills", fills.Load())
			}
		})
	}
}
