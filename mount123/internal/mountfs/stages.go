package mountfs

import (
	"context"
	"time"

	"github.com/binghezhouke/123/mount123/internal/iostats"
	"github.com/binghezhouke/123/mount123/internal/workqueue"
)

func (t *Tree) beginOperation(ctx context.Context, op iostats.Operation) func() {
	stats := t.ioStats()
	if stats == nil {
		return func() {}
	}
	t.observeScan()
	return stats.BeginOperation(op, workqueue.IsBackground(ctx))
}

func (t *Tree) ioStats() *iostats.Tracker {
	if t == nil || t.cache == nil {
		return nil
	}
	return t.cache.IOStats()
}

// observeStage records one inclusive operation phase without attaching request
// identity. Stage durations may overlap with each other and with HTTP/cache
// timing; they are useful for attribution, not additive accounting.
func (t *Tree) observeStage(stage iostats.Stage, started time.Time) {
	if t == nil || t.cache == nil {
		return
	}
	if stats := t.cache.IOStats(); stats != nil {
		stats.ObserveStage(stage, time.Since(started))
	}
}

func (n *Node) observeStage(stage iostats.Stage, started time.Time) {
	if n != nil && n.tree != nil {
		n.tree.observeStage(stage, started)
	}
}
