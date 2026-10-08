package storage

import (
	"context"

	"github.com/binghezhouke/123/mount123/internal/workqueue"
)

// The transfer gate is independent of decoder and API limits. A long archive
// scan cannot consume every transfer slot needed by an interactive reader.
func (c *Cache) acquireTransfer(ctx context.Context) (func(), error) {
	c.mu.Lock()
	if c.downloadGate == nil {
		c.downloadGate = workqueue.New(8)
	}
	gate := c.downloadGate
	c.mu.Unlock()
	return gate.Acquire(ctx)
}
