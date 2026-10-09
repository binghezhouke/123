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

type apiScheduler struct {
	mu        sync.Mutex
	now       func() time.Time
	wake      func(time.Duration) <-chan time.Time
	next      map[apiCategory]time.Time
	cool      map[apiCategory]time.Time
	intervals map[apiCategory]time.Duration
}

func newAPIScheduler() *apiScheduler {
	return &apiScheduler{
		now: time.Now,
		wake: func(d time.Duration) <-chan time.Time {
			t := time.NewTimer(d)
			return t.C
		},
		next: map[apiCategory]time.Time{},
		cool: map[apiCategory]time.Time{},
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

func (s *apiScheduler) acquire(ctx context.Context, cat apiCategory, foreground bool) error {
	_ = foreground // The mount's build gate already gives foreground work admission priority.
	for {
		s.mu.Lock()
		now := s.now()
		at := s.next[cat]
		if s.cool[cat].After(at) {
			at = s.cool[cat]
		}
		wait := at.Sub(now)
		if wait <= 0 {
			s.next[cat] = now.Add(s.intervals[cat])
			s.mu.Unlock()
			return nil
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.wake(wait):
		}
	}
}

func (s *apiScheduler) cooldown(cat apiCategory, d time.Duration) {
	if d <= 0 {
		return
	}
	s.mu.Lock()
	until := s.now().Add(d)
	if until.After(s.cool[cat]) {
		s.cool[cat] = until
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
