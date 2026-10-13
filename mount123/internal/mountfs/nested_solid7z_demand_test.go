package mountfs

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestSolid7zPendingOutputSharesResidentBudget(t *testing.T) {
	p := newNestedSolid7zPool(2, 100)
	defer p.Close()
	parent := &nestedArchive{}
	defer parent.Close()
	e, err := p.acquire(context.Background(), "group", 80, parent, nil)
	if err != nil || e == nil {
		t.Fatal(err)
	}
	e.state = &nestedSolid7zState{stream: &solidPoolTestStream{Reader: bytes.NewReader(nil)}}
	a, b, rejected := &nestedSolid7zDemand{start: 0, length: 6}, &nestedSolid7zDemand{start: 6, length: 6}, &nestedSolid7zDemand{start: 12, length: 1}
	p.mu.Lock()
	p.registerDemandLocked(e, a)
	p.registerDemandLocked(e, b)
	p.registerDemandLocked(e, rejected)
	p.mu.Unlock()
	if p.used != 92 || p.pendingBytes != 12 || rejected.buffer != nil {
		t.Fatal("waiting output escaped its resident budget")
	}
	p.capture(e, 0, []byte("first!other!"))
	p.release(e, true)
	if !a.ready || !b.ready || string(a.buffer) != "first!" || string(b.buffer) != "other!" {
		t.Fatal("queued range capture mismatch")
	}
	// Closing the idle decoder does not free still-live waiter buffers.
	p.Close()
	if p.used != 12 || p.pendingBytes != 12 {
		t.Fatal("shutdown undercounted waiting output")
	}
	p.releaseDemand(a)
	if p.used != 6 || p.pendingBytes != 6 {
		t.Fatal("first waiter released the wrong charge")
	}
	p.releaseDemand(b)
	p.releaseDemand(rejected)
	if p.used != 0 || p.pendingBytes != 0 {
		t.Fatal("waiting output charge leaked")
	}
}

func TestSolid7zPendingOutputDoesNotAcceptFailedOrIncompleteDecode(t *testing.T) {
	for _, mode := range []string{"source-or-crc-error", "late-range", "other-group"} {
		t.Run(mode, func(t *testing.T) {
			p := newNestedSolid7zPool(2, 100)
			defer p.Close()
			parent := &nestedArchive{}
			defer parent.Close()
			e, err := p.acquire(context.Background(), "group", 40, parent, nil)
			if err != nil || e == nil {
				t.Fatal(err)
			}
			d := &nestedSolid7zDemand{start: 0, length: 8}
			defer p.releaseDemand(d)
			p.mu.Lock()
			p.registerDemandLocked(e, d)
			p.mu.Unlock()
			switch mode {
			case "source-or-crc-error":
				p.capture(e, 0, []byte("complete"))
			case "late-range":
				p.capture(e, 4, []byte("late"))
			case "other-group":
				p.capture(&nestedSolid7zEntry{}, 0, []byte("complete"))
			}
			p.release(e, mode != "source-or-crc-error")
			if d.ready {
				t.Fatal("failed, incomplete, or unrelated output became reusable")
			}
			if p.used != 8 || p.pendingBytes != 8 {
				t.Fatal("retirement lost waiter ownership")
			}
		})
	}
}

func TestSolid7zPendingCancellationLeavesProducerAlive(t *testing.T) {
	p := newNestedSolid7zPool(2, 100)
	defer p.Close()
	parent := &nestedArchive{}
	defer parent.Close()
	e, err := p.acquire(context.Background(), "group", 80, parent, nil)
	if err != nil || e == nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	d := &nestedSolid7zDemand{start: 0, length: 8}
	done := make(chan error, 1)
	go func() { _, err := p.acquire(ctx, "group", 80, parent, d); p.releaseDemand(d); done <- err }()
	for {
		p.mu.Lock()
		pending := len(e.demands)
		p.mu.Unlock()
		if pending == 1 {
			break
		}
		select {
		case <-ctx.Done():
			p.release(e, false)
			t.Fatal("waiter did not register")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if p.used != 80 || p.pendingBytes != 0 || len(e.demands) != 0 || !e.busy {
		t.Fatal("waiter cancellation damaged producer or leaked memory")
	}
	p.capture(e, 0, []byte("complete"))
	p.release(e, false)
	if p.used != 0 {
		t.Fatal("producer retirement leaked")
	}
}
