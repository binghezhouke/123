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

// Gate reserves one slot for foreground work when capacity is at least two.
// Queued foreground work also takes precedence when a slot becomes free.
// Running jobs are not forcibly interrupted.
type Gate struct {
	mu                                 sync.Mutex
	limit, active, background, waiting int
	changed                            chan struct{}
}

func New(limit int) *Gate {
	return &Gate{limit: max(1, limit), changed: make(chan struct{})}
}

func (g *Gate) notify() {
	close(g.changed)
	g.changed = make(chan struct{})
}

// Acquire returns an idempotent release function. The wait observes ctx.
func (g *Gate) Acquire(ctx context.Context) (func(), error) {
	bg, _ := ctx.Value(backgroundKey{}).(bool)
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
		if g.active < g.limit && (!bg || (g.background < max(1, g.limit-1) && g.waiting == 0)) {
			g.active++
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
