package mountfs

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/binghezhouke/123/mount123/internal/iostats"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/binghezhouke/123/mount123/internal/workqueue"
)

const (
	initialReadAheadBytes  int64 = 1 << 20
	maximumReadAheadBytes  int64 = 16 << 20
	minimumReadAheadChunk  int64 = 256 << 10
	initialReadAheadChunk  int64 = 512 << 10
	maximumReadAheadChunk  int64 = 4 << 20
	maxReadAheadFlights          = 2
	readAheadContinuityGap       = 1 << 20
)

type readAheadRange struct {
	start, end    int64 // half-open file offsets
	complete      bool
	consumedUntil int64
	consumedSet   bool
	generation    uint64
}

// readAheadSnapshot is a bounded per-handle accounting view. ConsumedBytes
// credits unique forward overlap with a scheduled range; this is a usefulness
// proxy, not proof that the background request itself supplied those bytes.
type readAheadSnapshot struct {
	ScheduledBytes  uint64
	CompletedBytes  uint64
	ConsumedBytes   uint64
	WastedBytes     uint64
	ForegroundWait  time.Duration
	ForegroundReads uint64
}

type readAheadSource interface {
	PrefetchRangeAtContext(context.Context, int64, int64) error
	DownloadStats() storage.DownloadStats
}

type readAhead struct {
	mu        sync.Mutex
	parent    context.Context
	remote    readAheadSource
	base      int64
	size      uint64
	maxWindow int64

	ctx    context.Context
	cancel context.CancelFunc

	frontier           int64
	window             int64
	chunk              int64
	continuous         int
	lastProgress       time.Time
	rateBytesPerSecond float64
	lowHitReads        int
	generation         uint64
	ranges             []*readAheadRange
	jobs               int
	inFlightBytes      int64
	closed             bool
	statsTracker       *iostats.Tracker
	wasteRecorded      bool
	stats              readAheadSnapshot
	wg                 sync.WaitGroup
}

func newReadAhead(parent context.Context, remote readAheadSource, base int64, size uint64, maxWindow int64) *readAhead {
	if maxWindow <= 0 {
		maxWindow = maximumReadAheadBytes
	}
	maxWindow = min(maxWindow, maximumReadAheadBytes)
	if maxWindow < minimumReadAheadChunk {
		maxWindow = minimumReadAheadChunk
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(workqueue.Background(parent))
	return &readAhead{
		parent: parent, remote: remote, base: base, size: size, maxWindow: maxWindow,
		ctx: ctx, cancel: cancel, window: min(initialReadAheadBytes, maxWindow), chunk: initialReadAheadChunk,
	}
}

// observe records a successful foreground read. The high-water mark advances
// for contiguous, overlapping, or mildly reordered reads. A distant offset is
// treated as a seek; a small reread does not discard useful in-flight work.
func (r *readAhead) observe(off, n int64, elapsed time.Duration) {
	if n <= 0 || off < 0 || off > int64(^uint64(0)>>1)-n {
		return
	}
	end := off + n
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || uint64(end) > r.size {
		return
	}
	r.stats.ForegroundReads++
	r.stats.ForegroundWait += max(elapsed, 0)
	if r.statsTracker != nil {
		r.statsTracker.ObserveReadAheadWait(elapsed)
	}
	if !r.lastProgress.IsZero() && time.Since(r.lastProgress) > 750*time.Millisecond {
		r.window = min(r.maxWindow, max(minimumReadAheadChunk, r.window/2))
		r.chunk = max(minimumReadAheadChunk, r.chunk/2)
		r.continuous = 0
		r.rateBytesPerSecond = 0
	}
	hitBytes := r.scheduledOverlapLocked(off, end)
	if r.frontier == 0 {
		r.frontier = end
		r.lastProgress = time.Now()
	} else {
		gap := off - r.frontier
		if gap > readAheadContinuityGap || end < r.frontier-readAheadContinuityGap {
			r.cancel()
			r.ctx, r.cancel = context.WithCancel(workqueue.Background(r.parent))
			r.generation++
			r.ranges = nil
			r.window = min(initialReadAheadBytes, r.maxWindow)
			r.chunk = initialReadAheadChunk
			r.continuous = 0
			r.lowHitReads = 0
			r.rateBytesPerSecond = 0
			r.frontier = end
			r.lastProgress = time.Now()
		} else if end > r.frontier {
			oldFrontier := r.frontier
			if off <= r.frontier+readAheadContinuityGap {
				r.continuous++
				// Application pauses are part of consumption time. A fast cache
				// hit alone does not mean the application is consuming quickly.
				elapsedForRate := time.Since(r.lastProgress)
				if r.lastProgress.IsZero() {
					elapsedForRate = elapsed
				}
				if elapsedForRate > 0 {
					rate := float64(min(n, end-oldFrontier)) / elapsedForRate.Seconds()
					if r.rateBytesPerSecond == 0 {
						r.rateBytesPerSecond = rate
					} else {
						r.rateBytesPerSecond = 0.75*r.rateBytesPerSecond + 0.25*rate
					}
				}
				if r.continuous >= 2 {
					r.window = min(r.maxWindow, max(r.window, initialReadAheadBytes*4))
				}
				if r.continuous >= 4 && (elapsed > 20*time.Millisecond || r.rateBytesPerSecond > float64(r.window)/(250*time.Millisecond).Seconds()) {
					r.window = min(r.maxWindow, r.window*2)
					r.chunk = min(maximumReadAheadChunk, max(r.chunk, r.chunk*2))
				}
				if hitBytes*4 < uint64(n) {
					r.lowHitReads++
					if r.lowHitReads >= 4 {
						r.window = min(r.maxWindow, max(minimumReadAheadChunk, r.window/2))
						r.chunk = max(minimumReadAheadChunk, r.chunk/2)
						r.lowHitReads = 0
					}
				} else if hitBytes > 0 {
					r.lowHitReads = 0
					r.chunk = min(maximumReadAheadChunk, r.chunk*2)
				}
			}
			r.frontier = end
			r.lastProgress = time.Now()
		}
	}
	r.trimConsumedLocked()
	r.planLocked()
}

func (r *readAhead) scheduledOverlapLocked(start, end int64) uint64 {
	var n uint64
	for _, rg := range r.ranges {
		if rg.end <= start || rg.start >= end {
			continue
		}
		if !rg.consumedSet {
			rg.consumedUntil = max(start, rg.start)
			rg.consumedSet = true
		}
		if start <= rg.consumedUntil && end > rg.consumedUntil {
			b := min(end, rg.end)
			if b > rg.consumedUntil {
				n += uint64(b - rg.consumedUntil)
				rg.consumedUntil = b
			}
		}
	}
	r.stats.ConsumedBytes += n
	if n > 0 && r.statsTracker != nil {
		r.statsTracker.AddReadAheadConsumed(n)
	}
	return n
}

func (r *readAhead) trimConsumedLocked() {
	kept := r.ranges[:0]
	for _, rg := range r.ranges {
		if rg.complete && rg.end <= r.frontier {
			continue
		}
		kept = append(kept, rg)
	}
	r.ranges = kept
}

func (r *readAhead) planLocked() {
	if r.closed || r.remote == nil || r.frontier >= int64(r.size) || r.jobs >= maxReadAheadFlights {
		return
	}
	limit := min(int64(r.size), r.frontier+r.window)
	if limit <= r.frontier {
		return
	}
	// Fill the first uncovered gap. Completed and in-flight ranges both count
	// against the same consumer-relative lead budget.
	sorted := append([]*readAheadRange(nil), r.ranges...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].start < sorted[j].start })
	start := r.frontier
	for _, rg := range sorted {
		if rg.end <= start {
			continue
		}
		if rg.start > start {
			limit = min(limit, rg.start)
			break
		}
		start = max(start, rg.end)
	}
	if start >= limit {
		return
	}
	length := min(r.chunk, limit-start)
	length = min(length, r.window-r.inFlightBytes)
	if length <= 0 {
		return
	}
	// Leave room for foreground work when the global download scheduler is
	// busy. If no background budget is available, the next foreground read
	// observation retries planning without growing the consumer-relative lead.
	budget := r.remote.DownloadStats().AvailableBackgroundBytes
	if budget <= 0 {
		return
	}
	length = min(length, budget)
	if length <= 0 {
		return
	}
	if length < minimumReadAheadChunk && length < limit-start {
		return
	}
	ctx := r.ctx
	rg := &readAheadRange{start: start, end: start + length, generation: r.generation}
	r.ranges = append(r.ranges, rg)
	r.jobs++
	r.inFlightBytes += length
	r.stats.ScheduledBytes += uint64(length)
	if r.statsTracker != nil {
		r.statsTracker.AddReadAheadScheduled(uint64(length))
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		err := r.remote.PrefetchRangeAtContext(ctx, r.base+rg.start, rg.end-rg.start)
		r.mu.Lock()
		r.jobs--
		r.inFlightBytes -= rg.end - rg.start
		if err == nil {
			r.stats.CompletedBytes += uint64(rg.end - rg.start)
			if r.statsTracker != nil {
				r.statsTracker.AddReadAheadCompleted(uint64(rg.end - rg.start))
			}
			if !r.closed && rg.generation == r.generation && r.containsRangeLocked(rg) {
				rg.complete = true
				r.planLocked()
			} else if !r.closed {
				r.planLocked()
			}
		} else {
			for i, candidate := range r.ranges {
				if candidate == rg {
					r.ranges = append(r.ranges[:i], r.ranges[i+1:]...)
					break
				}
			}
			if !r.closed && rg.generation != r.generation {
				r.planLocked()
			}
		}
		r.mu.Unlock()
	}()
}

func (r *readAhead) containsRangeLocked(want *readAheadRange) bool {
	for _, rg := range r.ranges {
		if rg == want {
			return true
		}
	}
	return false
}

func (r *readAhead) snapshot() readAheadSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.stats
	if s.CompletedBytes > s.ConsumedBytes {
		s.WastedBytes = s.CompletedBytes - min(s.CompletedBytes, s.ConsumedBytes)
	}
	return s
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
	r.mu.Lock()
	if !r.wasteRecorded {
		if r.stats.CompletedBytes > r.stats.ConsumedBytes && r.statsTracker != nil {
			wasted := r.stats.CompletedBytes - min(r.stats.CompletedBytes, r.stats.ConsumedBytes)
			r.statsTracker.AddReadAheadWasted(wasted)
		}
		r.wasteRecorded = true
	}
	r.mu.Unlock()
}
