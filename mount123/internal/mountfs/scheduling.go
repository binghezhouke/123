package mountfs

import (
	"context"
	"time"

	"github.com/binghezhouke/123/mount123/internal/iostats"
	"github.com/binghezhouke/123/mount123/internal/workqueue"
)

func (t *Tree) acquireBuild(ctx context.Context) (func(), error) {
	started := time.Now()
	defer t.observeStage(iostats.StageBuildQueue, started)
	t.buildOnce.Do(func() {
		limit := cap(t.builds)
		if limit == 0 {
			limit = defaults(t.opts).MaxConcurrentBuilds
		}
		t.buildGate = workqueue.New(limit)
		if t.scanShedding() {
			t.buildGate.SetBackgroundLimit(shedBackgroundBuilds)
		}
	})
	return t.buildGate.Acquire(ctx)
}
