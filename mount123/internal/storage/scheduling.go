package storage

import (
	"context"
	"time"
)

func (c *Cache) acquireTransfer(ctx context.Context, bytes int64, file string, priority *downloadPriority) (func(), error) {
	started := time.Now()
	release, err := c.downloads.Acquire(ctx, bytes, file, priority)
	c.stats.ObserveTransferQueue(time.Since(started))
	return release, err
}
