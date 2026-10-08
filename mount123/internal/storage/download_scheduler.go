package storage

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

const (
	defaultDownloadRequests = 32
	defaultDownloadBytes    = int64(128 << 20)
	defaultForegroundBytes  = int64(16 << 20)
)

// DownloadConfig bounds HTTP response bytes that are still in flight. It does
// not describe Go heap use, cache disk use, or bandwidth.
type DownloadConfig struct {
	MaxRequests             int
	MaxInFlightBytes        int64
	ForegroundReservedBytes int64
	RequestChunkBytes       int64
}

func DefaultDownloadConfig() DownloadConfig {
	return DownloadConfig{MaxRequests: defaultDownloadRequests, MaxInFlightBytes: defaultDownloadBytes, ForegroundReservedBytes: defaultForegroundBytes, RequestChunkBytes: defaultDownloadBytes}
}

func (c DownloadConfig) normalized() (DownloadConfig, error) {
	d := DefaultDownloadConfig()
	if c == (DownloadConfig{}) {
		return d, nil
	}
	if c.MaxRequests == 0 {
		c.MaxRequests = d.MaxRequests
	}
	if c.MaxInFlightBytes == 0 {
		c.MaxInFlightBytes = d.MaxInFlightBytes
	}
	if c.RequestChunkBytes == 0 {
		c.RequestChunkBytes = d.RequestChunkBytes
	}
	if c.MaxRequests < 1 || c.MaxInFlightBytes < 1 || c.ForegroundReservedBytes < 0 || c.ForegroundReservedBytes >= c.MaxInFlightBytes || c.RequestChunkBytes < 1 {
		return c, errors.New("invalid download scheduler configuration")
	}
	if c.RequestChunkBytes > c.MaxInFlightBytes {
		c.RequestChunkBytes = c.MaxInFlightBytes
	}
	return c, nil
}

type downloadPriority struct{ promoted atomic.Bool }

type downloadWaiter struct {
	ctx        context.Context
	bytes      int64
	file       string
	priority   *downloadPriority
	done       chan struct{}
	granted    bool
	background bool
}

// downloadScheduler grants request slots and expected response-byte capacity
// atomically. A round-robin file queue prevents one archive scan from owning
// all of the waiting positions.
type downloadScheduler struct {
	mu                 sync.Mutex
	cfg                DownloadConfig
	activeRequests     int
	activeBytes        int64
	backgroundRequests int
	backgroundBytes    int64
	queues             [2]map[string][]*downloadWaiter
	order              [2][]string
	next               [2]int
	stats              DownloadStats
	foregroundBurst    int
	backgroundTurn     bool
	idle               chan struct{}
}

// DownloadStats reports scheduler state. Bytes count expected HTTP response
// bytes held until the response body closes.
type DownloadStats struct {
	MaxRequests              int
	MaxInFlightBytes         int64
	ForegroundReservedBytes  int64
	AvailableBackgroundBytes int64
	ActiveRequests           int
	ActiveBytes              int64
	PeakActiveBytes          int64
	WaitingForeground        int
	WaitingBackground        int
	CompletedRequests        uint64
	Promotions               uint64
	StagingActiveBytes       int64
	StagingPeakBytes         int64
	StagingWaitingForeground int
	StagingWaitingBackground int
}

func newDownloadScheduler(cfg DownloadConfig) *downloadScheduler {
	idle := make(chan struct{})
	close(idle)
	return &downloadScheduler{cfg: cfg, queues: [2]map[string][]*downloadWaiter{make(map[string][]*downloadWaiter), make(map[string][]*downloadWaiter)}, idle: idle}
}

func (s *downloadScheduler) promote(p *downloadPriority) {
	if p == nil || p.promoted.Swap(true) {
		return
	}
	s.promoteQueued(p)
}

// promoteQueued reclassifies a shared task's waiters after another scheduler
// has already set its shared priority flag.
func (s *downloadScheduler) promoteQueued(p *downloadPriority) {
	if p == nil {
		return
	}
	s.mu.Lock()
	s.stats.Promotions++
	for file, q := range s.queues[1] {
		for i := 0; i < len(q); {
			w := q[i]
			if w.priority != p {
				i++
				continue
			}
			q = append(q[:i], q[i+1:]...)
			s.stats.WaitingBackground--
			s.stats.WaitingForeground++
			if len(s.queues[0][file]) == 0 {
				s.order[0] = append(s.order[0], file)
			}
			s.queues[0][file] = append(s.queues[0][file], w)
		}
		if len(q) == 0 {
			delete(s.queues[1], file)
		} else {
			s.queues[1][file] = q
		}
	}
	s.dispatchLocked()
	s.mu.Unlock()
}

func (s *downloadScheduler) Acquire(ctx context.Context, bytes int64, file string, priority *downloadPriority) (func(), error) {
	if bytes < 1 {
		return nil, errors.New("download request must reserve positive bytes")
	}
	if bytes > s.cfg.MaxInFlightBytes {
		return nil, errors.New("download request exceeds in-flight byte budget")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	w := &downloadWaiter{ctx: ctx, bytes: bytes, file: file, priority: priority, done: make(chan struct{})}
	s.mu.Lock()
	s.enqueueLocked(w)
	s.dispatchLocked()
	s.mu.Unlock()
	select {
	case <-w.done:
	case <-ctx.Done():
	}
	s.mu.Lock()
	if ctx.Err() != nil {
		if w.granted {
			s.releaseLocked(w)
		} else {
			s.removeLocked(w)
		}
		s.dispatchLocked()
		s.mu.Unlock()
		return nil, ctx.Err()
	}
	if !w.granted {
		s.mu.Unlock()
		return nil, errors.New("download scheduler woke without granting request")
	}
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() { s.mu.Lock(); s.releaseLocked(w); s.dispatchLocked(); s.mu.Unlock() })
	}, nil
}

func (s *downloadScheduler) isBackground(w *downloadWaiter) bool {
	return w.priority == nil || !w.priority.promoted.Load()
}

func (s *downloadScheduler) enqueueLocked(w *downloadWaiter) {
	class := 1
	if !s.isBackground(w) {
		class = 0
	}
	if len(s.queues[class][w.file]) == 0 {
		s.order[class] = append(s.order[class], w.file)
	}
	s.queues[class][w.file] = append(s.queues[class][w.file], w)
	if class == 0 {
		s.stats.WaitingForeground++
	} else {
		s.stats.WaitingBackground++
	}
}

func (s *downloadScheduler) removeLocked(w *downloadWaiter) {
	for class := range s.queues {
		for file, q := range s.queues[class] {
			for i, candidate := range q {
				if candidate == w {
					q = append(q[:i], q[i+1:]...)
					s.queues[class][file] = q
					if class == 0 {
						s.stats.WaitingForeground--
					} else {
						s.stats.WaitingBackground--
					}
					if len(q) == 0 {
						delete(s.queues[class], file)
					}
					return
				}
			}
		}
	}
}

func (s *downloadScheduler) releaseLocked(w *downloadWaiter) {
	s.activeRequests--
	s.activeBytes -= w.bytes
	if w.background {
		s.backgroundRequests--
		s.backgroundBytes -= w.bytes
	}
	s.stats.ActiveRequests = s.activeRequests
	s.stats.ActiveBytes = s.activeBytes
	s.stats.CompletedRequests++
	if s.activeRequests == 0 {
		close(s.idle)
	}
}

func (s *downloadScheduler) canGrant(w *downloadWaiter) bool {
	if s.activeRequests >= s.cfg.MaxRequests || s.activeBytes+w.bytes > s.cfg.MaxInFlightBytes {
		return false
	}
	backgroundRequestLimit := max(1, s.cfg.MaxRequests-1)
	if s.isBackground(w) && (s.backgroundRequests >= backgroundRequestLimit || s.backgroundBytes+w.bytes > s.cfg.MaxInFlightBytes-s.cfg.ForegroundReservedBytes) {
		return false
	}
	return true
}

func (s *downloadScheduler) nextGrantableLocked(class int) *downloadWaiter {
	files := s.order[class]
	for n := 0; n < len(files); {
		if len(files) == 0 {
			break
		}
		idx := s.next[class] % len(files)
		file := files[idx]
		q := s.queues[class][file]
		for len(q) > 0 && q[0].ctx.Err() != nil {
			w := q[0]
			q = q[1:]
			if class == 0 {
				s.stats.WaitingForeground--
			} else {
				s.stats.WaitingBackground--
			}
			close(w.done)
		}
		if len(q) == 0 {
			delete(s.queues[class], file)
			files = append(files[:idx], files[idx+1:]...)
			s.order[class] = files
			if len(files) > 0 {
				s.next[class] = idx % len(files)
			} else {
				s.next[class] = 0
			}
			continue
		}
		s.queues[class][file] = q
		w := q[0]
		if s.canGrant(w) {
			q = q[1:]
			if len(q) == 0 {
				delete(s.queues[class], file)
			} else {
				s.queues[class][file] = q
			}
			if class == 0 {
				s.stats.WaitingForeground--
			} else {
				s.stats.WaitingBackground--
			}
			if len(q) == 0 {
				files = append(files[:idx], files[idx+1:]...)
				s.order[class] = files
				if len(files) > 0 {
					s.next[class] = idx % len(files)
				} else {
					s.next[class] = 0
				}
			} else {
				s.order[class] = files
				s.next[class] = (idx + 1) % len(files)
			}
			return w
		}
		s.next[class] = (idx + 1) % len(files)
		n++
	}
	return nil
}

func (s *downloadScheduler) dispatchLocked() {
	for {
		var w *downloadWaiter
		if s.stats.WaitingBackground > 0 && s.stats.WaitingForeground > 0 && s.foregroundBurst >= 3 {
			s.backgroundTurn = true
		}
		if s.backgroundTurn {
			w = s.nextGrantableLocked(1)
			if w == nil {
				if s.stats.WaitingBackground > 0 {
					return
				}
				s.backgroundTurn = false
			}
		}
		if w == nil {
			w = s.nextGrantableLocked(0)
		}
		if w == nil {
			w = s.nextGrantableLocked(1)
		}
		if w == nil {
			return
		}
		w.granted = true
		w.background = s.isBackground(w)
		if s.activeRequests == 0 {
			s.idle = make(chan struct{})
		}
		s.activeRequests++
		s.activeBytes += w.bytes
		if w.background {
			s.backgroundRequests++
			s.backgroundBytes += w.bytes
			s.foregroundBurst = 0
			s.backgroundTurn = false
		} else {
			s.foregroundBurst++
		}
		s.stats.ActiveRequests = s.activeRequests
		s.stats.ActiveBytes = s.activeBytes
		if s.activeBytes > s.stats.PeakActiveBytes {
			s.stats.PeakActiveBytes = s.activeBytes
		}
		close(w.done)
	}
}

func (s *downloadScheduler) waitIdle() {
	s.mu.Lock()
	for s.activeRequests != 0 {
		idle := s.idle
		s.mu.Unlock()
		<-idle
		s.mu.Lock()
	}
	s.mu.Unlock()
}

func (s *downloadScheduler) snapshot() DownloadStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	stats := s.stats
	stats.MaxRequests = s.cfg.MaxRequests
	stats.MaxInFlightBytes = s.cfg.MaxInFlightBytes
	stats.ForegroundReservedBytes = s.cfg.ForegroundReservedBytes
	backgroundLimit := s.cfg.MaxInFlightBytes - s.cfg.ForegroundReservedBytes
	stats.AvailableBackgroundBytes = max(int64(0), min(backgroundLimit-s.backgroundBytes, s.cfg.MaxInFlightBytes-s.activeBytes))
	return stats
}
