// Package workqueue bounds expensive work and reserves capacity for readers.
package workqueue

import (
	"context"
	"sync"
)

type backgroundKey struct{}

// Background marks speculative work and refreshes. It is inherited by child
// contexts, so downloads caused by a background decoder keep its priority.
func Background(ctx context.Context) context.Context {
	return context.WithValue(ctx, backgroundKey{}, true)
}

// IsBackground reports the scheduling priority inherited by an operation.
func IsBackground(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	bg, _ := ctx.Value(backgroundKey{}).(bool)
	return bg
}

// Gate reserves one slot for foreground work when capacity is at least two.
// Queued foreground work also takes precedence when a slot becomes free.
// Running jobs are not forcibly interrupted.
type Gate struct {
	mu                                           sync.Mutex
	limit, active, background, waiting, requests int
	backgroundLimit                              int
	changed                                      chan struct{}
}

// Stats is a bounded point-in-time view of gate pressure.
type Stats struct{ Limit, Active, Background, Waiting, Requests int }

func (g *Gate) Stats() Stats {
	if g == nil {
		return Stats{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return Stats{Limit: g.limit, Active: g.active, Background: g.background, Waiting: g.waiting, Requests: g.requests}
}

func New(limit int) *Gate {
	return &Gate{limit: max(1, limit), changed: make(chan struct{})}
}

// SetBackgroundLimit caps how much of the gate background work may hold. Zero
// restores the default reservation of one slot for foreground work. Lowering
// the cap does not interrupt work that is already running.
func (g *Gate) SetBackgroundLimit(limit int) {
	if g == nil {
		return
	}
	g.mu.Lock()
	if limit < 0 {
		limit = 0
	}
	if g.backgroundLimit != limit {
		g.backgroundLimit = limit
		g.notify()
	}
	g.mu.Unlock()
}

// backgroundCap is the number of slots background work may hold. Callers hold
// the gate lock.
func (g *Gate) backgroundCap() int {
	if g.backgroundLimit > 0 {
		return min(g.backgroundLimit, g.limit)
	}
	return max(1, g.limit-1)
}

func (g *Gate) notify() {
	close(g.changed)
	g.changed = make(chan struct{})
}

// Acquire returns an idempotent release function. The wait observes ctx.
func (g *Gate) Acquire(ctx context.Context) (func(), error) {
	bg := IsBackground(ctx)
	g.mu.Lock()
	if !bg {
		g.waiting++
	}
	for {
		if err := ctx.Err(); err != nil {
			if !bg {
				g.waiting--
				g.notify()
			}
			g.mu.Unlock()
			return nil, err
		}
		if g.active < g.limit && (!bg || (g.background < g.backgroundCap() && g.waiting == 0)) {
			g.active++
			g.requests++
			if bg {
				g.background++
			} else {
				g.waiting--
			}
			g.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					g.mu.Lock()
					g.active--
					if bg {
						g.background--
					}
					g.notify()
					g.mu.Unlock()
				})
			}, nil
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
		}
		g.mu.Lock()
	}
}
