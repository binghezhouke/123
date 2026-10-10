package workqueue

import (
	"context"
	"testing"
	"time"
)

func TestReservationCancellationAndRelease(t *testing.T) {
	g := New(2)
	release, err := g.Acquire(Background(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := g.Acquire(Background(ctx)); err == nil {
		t.Fatal("background consumed reserved slot")
	}
	foreground, err := g.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	blocked, stop := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer stop()
	if _, err := g.Acquire(blocked); err == nil {
		t.Fatal("capacity exceeded")
	}
	foreground()
	foreground()
	release()
	release()
	next, err := g.Acquire(Background(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	next()
}

// Sustained foreground pressure can lower the background share without
// touching the foreground limit, and restoring the default brings capacity
// back without restarting the gate.
func TestBackgroundLimitReservesCapacityForForeground(t *testing.T) {
	g := New(4)
	g.SetBackgroundLimit(1)
	first, err := g.Acquire(Background(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	waiting := make(chan func(), 1)
	go func() {
		release, err := g.Acquire(Background(context.Background()))
		if err != nil {
			return
		}
		waiting <- release
	}()
	select {
	case release := <-waiting:
		release()
		t.Fatal("background work exceeded the lowered share")
	case <-time.After(20 * time.Millisecond):
	}
	foreground, err := g.Acquire(context.Background())
	if err != nil {
		t.Fatalf("foreground work was denied capacity: %v", err)
	}
	if got := g.Stats(); got.Background != 1 || got.Active != 2 {
		t.Fatalf("gate stats = %+v, want one background and one foreground", got)
	}
	first()
	select {
	case release := <-waiting:
		release()
	case <-time.After(time.Second):
		t.Fatal("background work did not resume after capacity was released")
	}
	foreground()
	g.SetBackgroundLimit(0)
	releases := make([]func(), 0, 3)
	for i := 0; i < 3; i++ {
		release, err := g.Acquire(Background(context.Background()))
		if err != nil {
			t.Fatalf("restored background share denied capacity: %v", err)
		}
		releases = append(releases, release)
	}
	for _, release := range releases {
		release()
	}
}
