package panapi

import (
	"context"
	"testing"
	"time"
)

func newTestScheduler(clock *testClock) *apiScheduler {
	s := newAPIScheduler()
	s.now = clock.now
	s.wake = clock.wake
	return s
}

type testClock struct{ current time.Time }

func (c *testClock) now() time.Time { return c.current }

func (c *testClock) wake(d time.Duration) <-chan time.Time {
	c.current = c.current.Add(d)
	ready := make(chan time.Time, 1)
	ready <- c.current
	return ready
}

func TestAPISchedulerUsesIndependentOfficialIntervals(t *testing.T) {
	clock := &testClock{current: time.Unix(0, 0)}
	s := newTestScheduler(clock)
	ctx := context.Background()

	if err := s.acquire(ctx, categoryV2List, true); err != nil {
		t.Fatal(err)
	}
	firstV2 := clock.current
	if err := s.acquire(ctx, categoryV1List, true); err != nil {
		t.Fatal(err)
	}
	if got := clock.current.Sub(firstV2); got != 0 {
		t.Fatalf("v1 list blocked by v2 list for %s", got)
	}
	if err := s.acquire(ctx, categoryV2List, true); err != nil {
		t.Fatal(err)
	}
	if got, want := clock.current.Sub(firstV2), time.Second/3; got != want {
		t.Fatalf("v2 list interval = %s, want %s", got, want)
	}
}

func TestAPIScheduler429CooldownIsScopedToCategory(t *testing.T) {
	clock := &testClock{current: time.Unix(0, 0)}
	s := newTestScheduler(clock)
	ctx := context.Background()

	s.cooldown(categoryV2List, 5*time.Second)
	if err := s.acquire(ctx, categoryV1List, true); err != nil {
		t.Fatal(err)
	}
	if clock.current != time.Unix(0, 0) {
		t.Fatalf("unrelated category advanced clock to %s", clock.current)
	}
	if err := s.acquire(ctx, categoryV2List, true); err != nil {
		t.Fatal(err)
	}
	if got, want := clock.current, time.Unix(5, 0); !got.Equal(want) {
		t.Fatalf("cooled category acquired at %s, want %s", got, want)
	}
}

func TestAPISchedulerHonorsCancellation(t *testing.T) {
	s := newAPIScheduler()
	ctx, cancel := context.WithCancel(context.Background())
	if err := s.acquire(ctx, categoryV2List, true); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := s.acquire(ctx, categoryV2List, true); err != context.Canceled {
		t.Fatalf("acquire error = %v, want context.Canceled", err)
	}
}

func TestCategoryForEndpoint(t *testing.T) {
	tests := map[string]apiCategory{
		"/api/v2/file/list":               categoryV2List,
		"/api/v1/file/list":               categoryV1List,
		"/upload/v1/file/create":          categoryUploadCreate,
		"/upload/v2/file/create":          categoryUploadCreate,
		"/upload/v2/file/upload_complete": categoryUploadCreate,
		"/api/v1/access_token":            categoryAccessToken,
		"/api/v1/file/download_info":      categoryDefault,
	}
	for endpoint, want := range tests {
		if got := categoryForEndpoint(endpoint); got != want {
			t.Errorf("categoryForEndpoint(%q) = %d, want %d", endpoint, got, want)
		}
	}
}
