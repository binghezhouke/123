package mountfs

import (
	"bytes"
	"context"
	"io"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/storage"
)

func deflateCheckpointFixture(t *testing.T) (*storage.Cache, *zipDeflateCheckpoints) {
	t.Helper()
	cache, err := storage.NewCache(t.TempDir(), 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cache.Close() })
	return cache, newZIPDeflateCheckpoints(cache, "synthetic-member", 40<<20, 20<<20)
}

func deflateCheckpointPoint(output int64) zipDeflateCheckpoint {
	return zipDeflateCheckpoint{OutputOffset: output, InputOffset: output / 2, BitOffset: 3, Window: bytes.Repeat([]byte("w"), 32768), CRC: 0x12345678}
}

func TestZIPDeflateCheckpointsPublishOnlyAfterFullCRC(t *testing.T) {
	_, store := deflateCheckpointFixture(t)
	ctx := context.Background()
	point := deflateCheckpointPoint((4 << 20) + 13)
	for _, finish := range []string{"close", "invalid"} {
		hooks := store.hooks(ctx)
		hooks.Save(point)
		if got := store.nearest(ctx, 8<<20, 0); got != nil {
			t.Fatal("unverified checkpoint became visible")
		}
		if finish == "close" {
			hooks.Close()
		} else {
			hooks.Invalid()
		}
		hooks.Valid()
		if got := store.nearest(ctx, 8<<20, 0); got != nil {
			t.Fatalf("checkpoint published after %s", finish)
		}
	}
	hooks := store.hooks(ctx)
	hooks.Save(point)
	hooks.Valid()
	hooks.Close()
	got := store.nearest(ctx, 8<<20, 0)
	if got == nil || got.OutputOffset != point.OutputOffset || got.InputOffset != point.InputOffset || got.BitOffset != point.BitOffset || got.CRC != point.CRC || !bytes.Equal(got.Window, point.Window) {
		t.Fatal("CRC-verified checkpoint did not survive stream close")
	}
}

func TestZIPDeflateCheckpointsNearestOwnsWindowAndKeepsFirstSlotPoint(t *testing.T) {
	_, store := deflateCheckpointFixture(t)
	ctx := context.Background()
	first := deflateCheckpointPoint((4 << 20) + 13)
	second := deflateCheckpointPoint((8 << 20) + 7)
	second.CRC = 0 // A declared zero CRC is a valid state, not a missing checksum.
	hooks := store.hooks(ctx)
	if !hooks.Want(first.OutputOffset) {
		t.Fatal("first point not wanted")
	}
	hooks.Save(first)
	if hooks.Want(first.OutputOffset + 100) {
		t.Fatal("second point in one slot exceeded pending budget")
	}
	hooks.Save(deflateCheckpointPoint(first.OutputOffset + 100))
	hooks.Save(second)
	first.Window[0] = 'z'
	hooks.Valid()
	got := store.nearest(ctx, 9<<20, 0)
	if got == nil || got.OutputOffset != second.OutputOffset || got.CRC != 0 {
		t.Fatal("nearest point or zero CRC lost")
	}
	got.Window[0] = 'z'
	if again := store.nearest(ctx, 9<<20, 0); again == nil || again.Window[0] != 'w' {
		t.Fatal("returned window aliases the stored record")
	}
	got = store.nearest(ctx, second.OutputOffset-1, 0)
	if got == nil || got.OutputOffset != first.OutputOffset || got.Window[0] != 'w' {
		t.Fatal("target before slot point did not fall back or Save retained caller's window")
	}
	if store.nearest(ctx, 9<<20, second.OutputOffset) != nil {
		t.Fatal("checkpoint did not advance past current decoder")
	}
	if got := store.nearest(ctx, 9<<20, first.OutputOffset); got == nil || got.OutputOffset != second.OutputOffset {
		t.Fatal("nearest point beyond current decoder not found")
	}
}

func TestZIPDeflateCheckpointsCancelAndFailedAttemptPreserveVerifiedRecords(t *testing.T) {
	_, store := deflateCheckpointFixture(t)
	ctx := context.Background()
	first := deflateCheckpointPoint((4 << 20) + 13)
	hooks := store.hooks(ctx)
	hooks.Save(first)
	hooks.Valid()
	for _, finish := range []string{"cancel", "invalid", "close"} {
		attempt, cancel := context.WithCancel(ctx)
		hooks = store.hooks(attempt)
		hooks.Save(deflateCheckpointPoint((8 << 20) + 7))
		switch finish {
		case "cancel":
			cancel()
		case "invalid":
			hooks.Invalid()
		case "close":
			hooks.Close()
		}
		hooks.Valid()
		cancel()
		got := store.nearest(ctx, 12<<20, 0)
		if got == nil || got.OutputOffset != first.OutputOffset {
			t.Fatalf("%s published pending points or removed verified records", finish)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if store.hooks(cancelled).Want(8<<20) || store.nearest(cancelled, 12<<20, 0) != nil {
		t.Fatal("cancelled request selected checkpoint work")
	}
}

func TestZIPDeflateCheckpointsRestartAndIdentityIsolation(t *testing.T) {
	dir := t.TempDir()
	cache, err := storage.NewCache(dir, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	store := newZIPDeflateCheckpoints(cache, "synthetic-member", 40<<20, 20<<20)
	hooks := store.hooks(context.Background())
	hooks.Save(deflateCheckpointPoint((4 << 20) + 13))
	hooks.Valid()
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	cache, err = storage.NewCache(dir, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	store = newZIPDeflateCheckpoints(cache, "synthetic-member", 40<<20, 20<<20)
	if store.nearest(context.Background(), 8<<20, 0) == nil {
		t.Fatal("verified record lost across cache restart")
	}
	for _, changed := range []struct {
		key              string
		size, compressed int64
	}{{"synthetic-other-member", 40 << 20, 20 << 20}, {"synthetic-member", 41 << 20, 20 << 20}, {"synthetic-member", 40 << 20, 21 << 20}} {
		other := newZIPDeflateCheckpoints(cache, changed.key, changed.size, changed.compressed)
		if other.nearest(context.Background(), 8<<20, 0) != nil {
			t.Fatal("checkpoint identity crossed member or layout")
		}
	}
}

func TestZIPDeflateCheckpointsCorruptionFallsBackToEarlierPoint(t *testing.T) {
	cache, store := deflateCheckpointFixture(t)
	ctx := context.Background()
	hooks := store.hooks(ctx)
	hooks.Save(deflateCheckpointPoint((4 << 20) + 13))
	hooks.Save(deflateCheckpointPoint((8 << 20) + 7))
	hooks.Valid()
	key := store.recordKey(2)
	handle, err := cache.OpenCached(key, zipDeflateCheckpointRecordBytes)
	if err != nil || handle == nil {
		t.Fatal("second record missing")
	}
	encoded := make([]byte, zipDeflateCheckpointRecordBytes)
	if n, err := handle.ReadAt(encoded, 0); n != len(encoded) || err != nil {
		t.Fatal("cannot read record")
	}
	handle.Close()
	encoded[len(encoded)/2] ^= 1
	if err := cache.Remove(key); err != nil {
		t.Fatal(err)
	}
	if err := cache.Store(ctx, key, encoded); err != nil {
		t.Fatal(err)
	}
	got := store.nearest(ctx, 12<<20, 0)
	if got == nil || got.OutputOffset != (4<<20)+13 {
		t.Fatal("corrupt nearest point did not fall back")
	}
	if bad, err := cache.OpenCached(key, zipDeflateCheckpointRecordBytes); err != nil || bad != nil {
		if bad != nil {
			bad.Close()
		}
		t.Fatal("un-pinned corrupt record was not removed")
	}
}

func TestZIPDeflateCheckpointsBoundsAndCapacityPolicy(t *testing.T) {
	cache, store := deflateCheckpointFixture(t)
	if store.slots != 16 || store.stride < 4<<20 || store.slots*zipDeflateCheckpointWindowBytes != 512<<10 {
		t.Fatal("pending window budget changed")
	}
	for _, size := range []int64{-1, 0, (8 << 20) - 1} {
		if newZIPDeflateCheckpoints(cache, "small", size, 10) != nil {
			t.Fatal("small or invalid member enabled checkpoints")
		}
	}
	if newZIPDeflateCheckpoints(nil, "missing-cache", 8<<20, 10) != nil || newZIPDeflateCheckpoints(cache, "", 8<<20, 10) != nil || newZIPDeflateCheckpoints(cache, "empty", 8<<20, 0) != nil {
		t.Fatal("incomplete identity enabled checkpoints")
	}
	large := newZIPDeflateCheckpoints(cache, "large", math.MaxInt64, math.MaxInt64)
	if large == nil || large.stride <= 0 {
		t.Fatal("stride rounding overflowed")
	}
	ctx := context.Background()
	for _, mutate := range []func(*zipDeflateCheckpoint){
		func(p *zipDeflateCheckpoint) { p.OutputOffset = -1 },
		func(p *zipDeflateCheckpoint) { p.OutputOffset = store.size + 1 },
		func(p *zipDeflateCheckpoint) { p.OutputOffset = store.size; p.InputOffset = store.compressed - 1 },
		func(p *zipDeflateCheckpoint) { p.InputOffset = -1 },
		func(p *zipDeflateCheckpoint) { p.InputOffset = store.compressed },
		func(p *zipDeflateCheckpoint) { p.BitOffset = 8 },
		func(p *zipDeflateCheckpoint) { p.Window = p.Window[:32767] },
	} {
		point := deflateCheckpointPoint((4 << 20) + 13)
		mutate(&point)
		hooks := store.hooks(ctx)
		hooks.Save(point)
		hooks.Valid()
		if store.nearest(ctx, math.MaxInt64, 0) != nil {
			t.Fatal("invalid decoder state published")
		}
	}
	for _, capacity := range []int64{512 << 10, 1 << 20} {
		small, err := storage.NewCache(t.TempDir(), capacity)
		if err != nil {
			t.Fatal(err)
		}
		s := newZIPDeflateCheckpoints(small, "small-budget", 32<<20, 16<<20)
		if capacity == 512<<10 && s != nil {
			t.Fatal("one-slot budget enabled checkpoints")
		}
		if capacity == 1<<20 && (s == nil || s.slots != 3 || s.stride != 11<<20) {
			t.Fatal("capacity reduction or MiB stride rounding failed")
		}
		small.Close()
	}
}

func TestZIPDeflateCheckpointsOrdinaryCacheEviction(t *testing.T) {
	cache, err := storage.NewCache(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	store := newZIPDeflateCheckpoints(cache, "eviction", 32<<20, 16<<20)
	ctx := context.Background()
	hooks := store.hooks(ctx)
	hooks.Save(deflateCheckpointPoint((11 << 20) + 13))
	hooks.Valid()
	if store.nearest(ctx, 12<<20, 0) == nil {
		t.Fatal("record not committed")
	}
	if err := cache.Store(ctx, "synthetic-full-cache", make([]byte, 1<<20)); err != nil {
		t.Fatal(err)
	}
	if store.nearest(ctx, 12<<20, 0) != nil {
		t.Fatal("checkpoint bypassed ordinary disk capacity eviction")
	}
}

func TestZIPDeflateCheckpointsLookupDoesNotWaitForFill(t *testing.T) {
	cache, store := deflateCheckpointFixture(t)
	started, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(resume) }) }
	defer unblock()
	finished := make(chan error, 1)
	go func() {
		h, err := cache.Acquire(context.Background(), store.recordKey(1), zipDeflateCheckpointRecordBytes, func(ctx context.Context, w io.Writer) error {
			close(started)
			<-resume
			_, err := w.Write(make([]byte, zipDeflateCheckpointRecordBytes))
			return err
		})
		if h != nil {
			h.Close()
		}
		finished <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("record fill did not start")
	}
	result := make(chan *zipDeflateCheckpoint, 1)
	go func() { result <- store.nearest(context.Background(), 8<<20, 0) }()
	select {
	case point := <-result:
		if point != nil {
			t.Fatal("unpublished fill became visible")
		}
	case <-time.After(time.Second):
		t.Fatal("checkpoint miss waited on a fill")
	}
	unblock()
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}
