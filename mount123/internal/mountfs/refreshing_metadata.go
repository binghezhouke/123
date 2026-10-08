package mountfs

import (
	"context"
	"errors"
	"time"

	"github.com/binghezhouke/123/mount123/internal/workqueue"
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
	if time.Now().After(item.expires) && !t.refreshing[key] && t.sources[key] == nil && len(t.refreshing) < t.opts.MaxConcurrentBuilds {
		if t.refreshing == nil {
			t.refreshing = map[string]bool{}
		}
		t.refreshing[key] = true
		flight := &sourceCall{done: make(chan struct{})}
		t.sources[key] = flight
		go t.refreshMetaInBackground(key, item, ttl, build, flight)
	}
	t.mu.Unlock()
	return value, nil
}

func (t *Tree) refreshMetaInBackground(key string, item *metaItem, ttl time.Duration, build func(context.Context) (any, int64, error), flight *sourceCall) {
	refreshCtx, cancel := context.WithTimeout(workqueue.Background(t.ctx), 45*time.Second)
	defer cancel()
	var fresh any
	var size int64
	release, err := t.acquireBuild(refreshCtx)
	if err == nil {
		fresh, size, err = build(refreshCtx)
		release()
	}
	if err == nil {
		err = refreshCtx.Err()
	}
	if err == nil && (size < 0 || size > t.opts.MetadataBytes) {
		err = errors.New("metadata snapshot exceeds configured budget")
	}
	t.mu.Lock()
	delete(t.refreshing, key)
	if t.sources[key] == flight {
		delete(t.sources, key)
	}
	current := t.meta[key]
	if current == item || current == nil {
		// An invalid/newly inaccessible snapshot must not keep serving forever.
		if err != nil || size < 0 || size > t.opts.MetadataBytes {
			if current == item {
				delete(t.meta, key)
				t.metaBytes -= item.bytes
			}
		} else {
			oldBytes := int64(0)
			if current == item {
				oldBytes = item.bytes
			}
			if t.makeMetadataRoomLocked(key, oldBytes, size) {
				t.seq++
				expires := time.Time{}
				if ttl > 0 {
					expires = time.Now().Add(ttl)
				}
				updated := &metaItem{key: key, value: fresh, bytes: size, expires: expires, seq: t.seq}
				t.meta[key] = updated
				t.metaBytes += size - oldBytes
				flight.value = fresh
			} else {
				err = errors.New("metadata cache cannot fit refreshed snapshot")
				if current == item {
					delete(t.meta, key)
					t.metaBytes -= item.bytes
				}
			}
		}
	} else {
		// A manual or foreground refresh installed a newer snapshot while this
		// request was running. Do not let this result overwrite it.
		flight.value = current.value
	}
	flight.err = err
	close(flight.done)
	t.mu.Unlock()
}

// makeMetadataRoomLocked evicts least-recently-used snapshots other than the
// key being replaced. The caller holds t.mu.
func (t *Tree) makeMetadataRoomLocked(key string, oldBytes, newBytes int64) bool {
	if oldBytes < 0 || newBytes < 0 || newBytes > t.opts.MetadataBytes {
		return false
	}
	for t.metaBytes-oldBytes+newBytes > t.opts.MetadataBytes {
		var oldest *metaItem
		for _, candidate := range t.meta {
			if candidate.key == key {
				continue
			}
			if oldest == nil || candidate.seq < oldest.seq {
				oldest = candidate
			}
		}
		if oldest == nil {
			return false
		}
		delete(t.meta, oldest.key)
		t.metaBytes -= oldest.bytes
	}
	return true
}
