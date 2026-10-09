package storage

import (
	"context"

	"io"
	"sync"
	"syscall"
)

// RangeWindow registers a bounded exact-range fill before any small demand
// reads. Bytes come from the existing disk staging path as they arrive. Wait
// checks the final HTTP result, including validation after the last byte read.
// Close cancels unused work and releases all pins. Callers must Wait before
// publishing content derived from this window. ReadAtContext, Wait and Close
// must not overlap; separate windows may be used concurrently.
type RangeWindow struct {
	remote     *Remote
	ctx        context.Context
	cancel     context.CancelFunc
	start, end int64
	flights    []*rangeFlight
	pins       []pinnedRangePart
	once       sync.Once
	mu         sync.Mutex
	tracked    map[*rangeFlight]bool
	closed     bool
}

type rangeWindowReadKey struct{}

// OpenRangeWindow eagerly registers missing ranges but does not wait for HTTP.
// The window is bounded by the remote and cache capacity; it uses the shared
// download/staging scheduler, cache identity and retry policy.
func (r *Remote) OpenRangeWindow(ctx context.Context, off, size int64) (*RangeWindow, error) {
	if off < 0 || size <= 0 || off > r.size || size > r.size-off {
		return nil, syscall.EINVAL
	}
	if size > r.cache.Capacity() {
		return nil, syscall.ENOSPC
	}
	ctx, cancel := combineContexts(r.lifetimeCtx, ctx)
	w := &RangeWindow{remote: r, ctx: ctx, cancel: cancel, start: off, end: off + size, tracked: make(map[*rangeFlight]bool)}
	if err := r.importLegacyPages(ctx, off, off+size); err != nil {
		w.Close()
		return nil, err
	}
	var err error
	w.pins, err = r.cache.pinAvailableRange(r.rangeID, off, off+size, false)
	if err != nil {
		w.Close()
		return nil, err
	}
	for _, gap := range uncoveredRangeGaps(w.pins, off, off+size) {
		for pos := gap.start; pos < gap.end; {
			flight, owner, err := r.cache.beginRangeFlight(ctx, r.rangeID, pos, gap.end)
			if err != nil {
				w.Close()
				return nil, err
			}
			w.flights = append(w.flights, flight)
			w.tracked[flight] = true
			if owner {
				go r.runRangeFlight(flight)
			}
			pos = min(gap.end, flight.end)
		}
	}
	return w, nil
}

// ReadAtContext uses offsets relative to this window. Foreground reads promote
// a speculative flight without creating another download or retaining a task
// beyond the explicitly owned window lifetime.
func (w *RangeWindow) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, syscall.EINVAL
	}
	if len(p) == 0 {
		return 0, nil
	}
	if off >= w.end-w.start {
		return 0, io.EOF
	}
	ctx, cancel := combineContexts(w.ctx, ctx)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	for _, f := range w.flightSnapshot() {
		select {
		case <-f.done:
			if f.err != nil {
				return 0, f.err
			}
		default:
		}
	}
	want := min(int64(len(p)), w.end-w.start-off)
	ctx = context.WithValue(ctx, rangeWindowReadKey{}, w)
	n, err := w.remote.ReadRangeAtContext(ctx, p[:want], w.start+off)
	if err == nil && n < len(p) {
		err = io.EOF
	}
	return n, err
}

func (w *RangeWindow) Wait(ctx context.Context) error {
	for _, f := range w.flightSnapshot() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-w.ctx.Done():
			return w.ctx.Err()
		case <-f.done:
			if f.err != nil {
				return f.err
			}
		}
	}
	return nil
}

// trackFlight retains even demand flights registered after an earlier fill
// published between the initial coverage snapshot and window registration.
// Otherwise that race could leave Wait blind to a late HTTP body error.
func (w *RangeWindow) trackFlight(f *rangeFlight) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.tracked[f] {
		return
	}
	w.remote.cache.mu.Lock()
	f.refs++ // the demand reader already owns a live reference
	w.remote.cache.mu.Unlock()
	w.tracked[f] = true
	w.flights = append(w.flights, f)
}
func (w *RangeWindow) flightSnapshot() []*rangeFlight {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]*rangeFlight(nil), w.flights...)
}
func (w *RangeWindow) Close() error {
	w.once.Do(func() {
		w.cancel()
		w.mu.Lock()
		w.closed = true
		flights := w.flights
		w.flights = nil
		w.mu.Unlock()
		for _, f := range flights {
			w.remote.cache.releaseRangeFlight(f)
		}
		closeRangeParts(w.pins)
	})
	return nil
}
func rangeWindowOwner(ctx context.Context) *RangeWindow {
	w, _ := ctx.Value(rangeWindowReadKey{}).(*RangeWindow)
	return w
}
