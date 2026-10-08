package mountfs

import (
	"context"

	"github.com/binghezhouke/123/mount123/internal/workqueue"
)

func (t *Tree) acquireBuild(ctx context.Context) (func(), error) {
	t.buildOnce.Do(func() {
		limit := cap(t.builds)
		if limit == 0 {
			limit = defaults(t.opts).MaxConcurrentBuilds
		}
		t.buildGate = workqueue.New(limit)
	})
	return t.buildGate.Acquire(ctx)
}
