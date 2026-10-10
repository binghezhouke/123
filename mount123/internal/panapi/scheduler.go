package panapi

import (
	"context"
	"sync"
	"time"
)

type apiCategory int

const (
	categoryDefault apiCategory = iota
	categoryV2List
	categoryV1List
	categoryUploadCreate
	categoryAccessToken
)

const foregroundBurstLimit = 3

// maxCategoryCooldown bounds a 429 cooldown so one anomalous Retry-After
// header cannot stall a whole API category for minutes or hours.
const maxCategoryCooldown = time.Minute

type apiWaiter struct {
	foreground bool
}

type apiScheduler struct {
	mu        sync.Mutex
	now       func() time.Time
	wake      func(time.Duration) <-chan time.Time
	next      map[apiCategory]time.Time
	cool      map[apiCategory]time.Time
	queues    map[apiCategory][]*apiWaiter
	changed   chan struct{}
	fgBurst   map[apiCategory]int
	intervals map[apiCategory]time.Duration
}

func newAPIScheduler() *apiScheduler {
	return &apiScheduler{
		now: time.Now,
		wake: func(d time.Duration) <-chan time.Time {
			t := time.NewTimer(d)
			return t.C
		},
		next:    map[apiCategory]time.Time{},
		cool:    map[apiCategory]time.Time{},
		queues:  map[apiCategory][]*apiWaiter{},
		changed: make(chan struct{}),
		fgBurst: map[apiCategory]int{},
		intervals: map[apiCategory]time.Duration{
			categoryV2List:       time.Second / 3,
			categoryV1List:       time.Second / 4,
			categoryUploadCreate: time.Second / 2,
			categoryAccessToken:  time.Second,
			// Endpoints without a published quota use the most conservative
			// documented interval rather than sharing the faster list quota.
			categoryDefault: time.Second,
		},
	}
}

func (s *apiScheduler) signalLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *apiScheduler) acquire(ctx context.Context, cat apiCategory, foreground bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	w := &apiWaiter{foreground: foreground}
	s.mu.Lock()
	s.queues[cat] = append(s.queues[cat], w)
	s.signalLocked()
	for {
		if err := ctx.Err(); err != nil {
			s.removeWaiterLocked(cat, w)
			s.signalLocked()
			s.mu.Unlock()
			return err
		}
		if s.selectedLocked(cat) == w {
			now := s.now()
			at := s.next[cat]
			if s.cool[cat].After(at) {
				at = s.cool[cat]
			}
			if !at.After(now) {
				s.removeWaiterLocked(cat, w)
				s.next[cat] = now.Add(s.intervals[cat])
				if foreground {
					s.fgBurst[cat]++
				} else {
					s.fgBurst[cat] = 0
				}
				s.signalLocked()
				s.mu.Unlock()
				return nil
			}
		}
		changed := s.changed
		wait := time.Duration(0)
		if s.selectedLocked(cat) == w {
			at := s.next[cat]
			if s.cool[cat].After(at) {
				at = s.cool[cat]
			}
			wait = at.Sub(s.now())
		}
		s.mu.Unlock()
		if wait > 0 {
			select {
			case <-ctx.Done():
			case <-changed:
			case <-s.wake(wait):
			}
		} else {
			select {
			case <-ctx.Done():
			case <-changed:
			}
		}
		s.mu.Lock()
	}
}

// A limited foreground burst preserves responsiveness while allowing queued
// background work to make progress during sustained foreground load.
func (s *apiScheduler) selectedLocked(cat apiCategory) *apiWaiter {
	queue := s.queues[cat]
	if len(queue) == 0 {
		return nil
	}
	var foreground, background *apiWaiter
	for _, w := range queue {
		if w.foreground && foreground == nil {
			foreground = w
		}
		if !w.foreground && background == nil {
			background = w
		}
	}
	if foreground != nil && (background == nil || s.fgBurst[cat] < foregroundBurstLimit) {
		return foreground
	}
	if background != nil {
		return background
	}
	return foreground
}

func (s *apiScheduler) removeWaiterLocked(cat apiCategory, target *apiWaiter) {
	queue := s.queues[cat]
	for i, w := range queue {
		if w == target {
			s.queues[cat] = append(queue[:i], queue[i+1:]...)
			return
		}
	}
}

func (s *apiScheduler) cooldown(cat apiCategory, d time.Duration) {
	if d <= 0 {
		return
	}
	if d > maxCategoryCooldown {
		d = maxCategoryCooldown
	}
	s.mu.Lock()
	until := s.now().Add(d)
	if until.After(s.cool[cat]) {
		s.cool[cat] = until
		s.signalLocked()
	}
	s.mu.Unlock()
}

func categoryForEndpoint(endpoint string) apiCategory {
	switch endpoint {
	case "/api/v2/file/list":
		return categoryV2List
	case "/api/v1/file/list":
		return categoryV1List
	case "/upload/v1/file/create", "/upload/v2/file/create", "/upload/v2/file/upload_complete":
		return categoryUploadCreate
	case "/api/v1/access_token":
		return categoryAccessToken
	default:
		return categoryDefault
	}
}
