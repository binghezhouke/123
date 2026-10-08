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
