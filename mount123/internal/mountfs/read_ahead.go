package mountfs

import (
	"context"
	"sync"

	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/binghezhouke/123/mount123/internal/workqueue"
)

const initialReadAheadBytes int64 = 1 << 20
const maximumReadAheadBytes int64 = 16 << 20
const maxReadAheadFlights = 2

type readAhead struct {
	mu               sync.Mutex
	parent           context.Context
	remote           *storage.Remote
	base             int64
	size             uint64
	maxWindow        int64
	maxInFlightBytes int64
	ctx              context.Context
	cancel           context.CancelFunc
	previousEnd      int64
	plannedUntil     int64
	window           int64
	streak           int
	jobs             int
	inFlightBytes    int64
	haveRead         bool
	closed           bool
	wg               sync.WaitGroup
}

func newReadAhead(parent context.Context, remote *storage.Remote, base int64, size uint64, maxWindow int64) *readAhead {
	if maxWindow <= 0 {
		maxWindow = maximumReadAheadBytes
	}
	if maxWindow > maximumReadAheadBytes {
		maxWindow = maximumReadAheadBytes
	}
	if maxWindow < initialReadAheadBytes {
		maxWindow = initialReadAheadBytes
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(workqueue.Background(parent))
	return &readAhead{parent: parent, remote: remote, base: base, size: size, maxWindow: maxWindow, maxInFlightBytes: 2 * maxWindow, ctx: ctx, cancel: cancel, window: initialReadAheadBytes}
}

func (r *readAhead) observe(off, n int64) {
	if n <= 0 || off < 0 {
		return
	}
	end := off + n
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || uint64(end) > r.size {
		return
	}
	if r.haveRead {
		if off != r.previousEnd {
			r.cancel()
			r.ctx, r.cancel = context.WithCancel(workqueue.Background(r.parent))
			r.streak = 0
			r.window = initialReadAheadBytes
			r.plannedUntil = end
		} else {
			r.streak++
			if r.streak == 1 {
				r.window = min(4<<20, r.maxWindow)
			}
			if r.streak >= 2 {
				r.window = r.maxWindow
			}
		}
	} else {
		r.plannedUntil = end
	}
	r.previousEnd = end
	r.haveRead = true
	start := max(end, r.plannedUntil)
	if start >= int64(r.size) || r.jobs >= maxReadAheadFlights {
		return
	}
	length := min(r.window, int64(r.size)-start)
	if length <= 0 || r.inFlightBytes+length > r.maxInFlightBytes {
		return
	}
	ctx := r.ctx
	r.jobs++
	r.inFlightBytes += length
	r.plannedUntil = start + length
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer func() { r.mu.Lock(); r.jobs--; r.inFlightBytes -= length; r.mu.Unlock() }()
		_ = r.remote.PrefetchRangeAtContext(ctx, r.base+start, length)
	}()
}

func (r *readAhead) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	if r.cancel != nil {
		r.cancel()
	}
	r.mu.Unlock()
	r.wg.Wait()
}
