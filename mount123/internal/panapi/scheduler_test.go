package panapi

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/workqueue"
)

func newTestScheduler(clock *testClock) *apiScheduler {
	s := newAPIScheduler()
	s.now = clock.now
	s.wake = clock.wake
	return s
}

// testClock is safe for the scheduler tests that drive waiters from several
// goroutines. Tests that never start a waiter read current directly.
type testClock struct {
	mu      sync.Mutex
	current time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current
}

func (c *testClock) advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.current = c.current.Add(d)
	return c.current
}

func (c *testClock) wake(d time.Duration) <-chan time.Time {
	at := c.advance(d)
	ready := make(chan time.Time, 1)
	ready <- at
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

func TestAPISchedulerForegroundPrecedesQueuedBackground(t *testing.T) {
	clock := &testClock{current: time.Unix(0, 0)}
	s := newTestScheduler(clock)
	s.wake = func(time.Duration) <-chan time.Time { return make(chan time.Time) }
	if err := s.acquire(context.Background(), categoryV2List, true); err != nil {
		t.Fatal(err)
	}
	backgroundDone := make(chan error, 1)
	go func() { backgroundDone <- s.acquire(workqueue.Background(context.Background()), categoryV2List, false) }()
	deadline := time.Now().Add(time.Second)
	for {
		s.mu.Lock()
		queued := len(s.queues[categoryV2List])
		s.mu.Unlock()
		if queued == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background request did not queue")
		}
		time.Sleep(time.Millisecond)
	}
	foregroundDone := make(chan error, 1)
	go func() { foregroundDone <- s.acquire(context.Background(), categoryV2List, true) }()
	waitForSchedulerQueue(t, s, categoryV2List, 2)
	clock.advance(time.Second)
	s.mu.Lock()
	s.signalLocked()
	s.mu.Unlock()
	select {
	case err := <-foregroundDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("foreground request was not granted")
	}
	select {
	case <-backgroundDone:
		t.Fatal("background request bypassed foreground")
	default:
	}
}

// The burst policy is a pure function of queue state, so assert it directly
// rather than racing a shared test clock against waiter wakeups.
func TestAPISchedulerForegroundBurstYieldsToQueuedBackground(t *testing.T) {
	s := newAPIScheduler()
	foreground := &apiWaiter{foreground: true}
	background := &apiWaiter{}
	s.queues[categoryV2List] = []*apiWaiter{background, foreground}

	for burst := 0; burst < foregroundBurstLimit; burst++ {
		s.fgBurst[categoryV2List] = burst
		if got := s.selectedLocked(categoryV2List); got != foreground {
			t.Fatalf("burst %d selected the background waiter while burst allowance remained", burst)
		}
	}
	s.fgBurst[categoryV2List] = foregroundBurstLimit
	if got := s.selectedLocked(categoryV2List); got != background {
		t.Fatal("queued background work never runs once the foreground burst is spent")
	}

	s.queues[categoryV2List] = []*apiWaiter{background}
	if got := s.selectedLocked(categoryV2List); got != background {
		t.Fatal("background-only queue was not selected")
	}
	s.queues[categoryV2List] = []*apiWaiter{foreground}
	if got := s.selectedLocked(categoryV2List); got != foreground {
		t.Fatal("foreground-only queue was not selected")
	}
	s.queues[categoryV2List] = nil
	if got := s.selectedLocked(categoryV2List); got != nil {
		t.Fatal("empty queue selected a waiter")
	}
}

func TestAPISchedulerTracksForegroundBurstAcrossGrants(t *testing.T) {
	clock := &testClock{current: time.Unix(0, 0)}
	s := newTestScheduler(clock)
	for want := 1; want <= foregroundBurstLimit; want++ {
		clock.advance(time.Second)
		if err := s.acquire(context.Background(), categoryV2List, true); err != nil {
			t.Fatal(err)
		}
		s.mu.Lock()
		got := s.fgBurst[categoryV2List]
		s.mu.Unlock()
		if got != want {
			t.Fatalf("foreground burst = %d, want %d", got, want)
		}
	}
	clock.advance(time.Second)
	if err := s.acquire(workqueue.Background(context.Background()), categoryV2List, false); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	got := s.fgBurst[categoryV2List]
	s.mu.Unlock()
	if got != 0 {
		t.Fatalf("background grant left burst = %d, want 0", got)
	}
}

func waitForSchedulerQueue(t *testing.T, s *apiScheduler, cat apiCategory, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		s.mu.Lock()
		got := len(s.queues[cat])
		s.mu.Unlock()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue length = %d, want %d", got, want)
		}
		time.Sleep(time.Millisecond)
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
