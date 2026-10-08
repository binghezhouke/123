package mountfs

import (
	"context"
	"time"
)

// loadRefreshingMeta serves an existing immutable snapshot while one bounded
// background request refreshes it. A cold miss still uses the normal loader.
func (t *Tree) loadRefreshingMeta(ctx context.Context, key string, ttl time.Duration, build func(context.Context) (any, int64, error)) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := t.ctx.Err(); err != nil {
		return nil, err
	}
	t.mu.Lock()
	item := t.meta[key]
	if item == nil {
		t.mu.Unlock()
		return t.loadMeta(ctx, key, ttl, build)
	}
	t.seq++
	item.seq = t.seq
	value := item.value
	if time.Now().After(item.expires) && !t.refreshing[key] && len(t.refreshing) < t.opts.MaxConcurrentBuilds {
		if t.refreshing == nil {
			t.refreshing = map[string]bool{}
		}
		t.refreshing[key] = true
		go func() {
			refreshCtx, cancel := context.WithTimeout(t.ctx, 45*time.Second)
			defer cancel()
			var fresh any
			var size int64
			var err error
			select {
			case t.builds <- struct{}{}:
				fresh, size, err = build(refreshCtx)
				<-t.builds
			case <-refreshCtx.Done():
				err = refreshCtx.Err()
			}
			if err == nil {
				err = refreshCtx.Err()
			}
			t.mu.Lock()
			defer t.mu.Unlock()
			delete(t.refreshing, key)
			if t.meta[key] != item {
				return
			}
			// An invalid/newly inaccessible snapshot must not keep serving forever.
			if err != nil || size < 0 || t.metaBytes-item.bytes+size > t.opts.MetadataBytes {
				delete(t.meta, key)
				t.metaBytes -= item.bytes
				return
			}
			t.metaBytes += size - item.bytes
			item.value = fresh
			item.bytes = size
			item.expires = time.Now().Add(ttl)
		}()
	}
	t.mu.Unlock()
	return value, nil
}
