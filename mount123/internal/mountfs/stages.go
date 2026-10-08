package mountfs

import (
	"time"

	"github.com/binghezhouke/123/mount123/internal/iostats"
)

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
