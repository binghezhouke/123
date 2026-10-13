package mountfs

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

type nestedDecodeKey struct{}

// One synchronous decode may read through several compressed parents. Take
// admission before creating cache flights or locking a reader, then lend it to
// its ancestors. Acquiring a second permit inside a fill deadlocks at limit 1.
type nestedDecodeLease struct {
	tree   *Tree
	active atomic.Bool
}

func (t *Tree) acquireNestedDecode(ctx context.Context) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if lease, ok := ctx.Value(nestedDecodeKey{}).(*nestedDecodeLease); ok && lease.tree == t && lease.active.Load() {
		return ctx, func() {}, nil
	}
	release, err := t.acquireBuild(ctx)
	if err != nil {
		return nil, nil, err
	}
	lease := &nestedDecodeLease{tree: t}
	lease.active.Store(true)
	return context.WithValue(ctx, nestedDecodeKey{}, lease), func() {
		if lease.active.Swap(false) {
			release()
		}
	}, nil
}

// A retained decoder must use the current ReadAt request, not the request that
// first opened it. Bind while holding the reader's lock and unbind on return;
// this context is only for synchronous decoder I/O, never background producers.
type nestedReadContext struct {
	mu     sync.RWMutex
	active context.Context
}

var inactiveNestedContext = func() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}()

func (c *nestedReadContext) bind(ctx context.Context) {
	c.mu.Lock()
	c.active = ctx
	c.mu.Unlock()
}

func (c *nestedReadContext) current() context.Context {
	c.mu.RLock()
	ctx := c.active
	c.mu.RUnlock()
	if ctx == nil {
		return inactiveNestedContext
	}
	return ctx
}

func (c *nestedReadContext) Deadline() (time.Time, bool) { return c.current().Deadline() }
func (c *nestedReadContext) Done() <-chan struct{}       { return c.current().Done() }
func (c *nestedReadContext) Err() error                  { return c.current().Err() }
func (c *nestedReadContext) Value(key any) any           { return c.current().Value(key) }
