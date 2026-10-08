package mountfs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/binghezhouke/123/mount123/internal/faults"
	"github.com/binghezhouke/123/mount123/internal/workqueue"
)

// loadRefreshingMeta serves an existing immutable snapshot while one bounded
// background request refreshes it. A cold miss still uses the normal loader.
func (t *Tree) loadRefreshingMeta(ctx context.Context, key string, ttl time.Duration, build func(context.Context) (any, int64, error)) (any, error) {
	return t.loadRefreshingMetaWithStalePolicy(ctx, key, ttl, build, false)
}

func (t *Tree) loadRefreshingDirectoryMeta(ctx context.Context, key string, ttl time.Duration, build func(context.Context) (any, int64, error)) (any, error) {
	return t.loadRefreshingMetaWithStalePolicy(ctx, key, ttl, build, true)
}

// A cold disk restore may already be expired by the time loadMeta installs it.
// Start its refresh before returning the restored stale snapshot to the caller.
func (t *Tree) startExpiredDirectoryRefresh(key string, ttl time.Duration, build func(context.Context) (any, int64, error)) {
	t.mu.Lock()
	item := t.meta[key]
	now := time.Now()
	if item != nil && !item.expires.IsZero() && now.After(item.expires) && !item.staleBackoffUntil.After(now) && !t.refreshing[key] && t.sources[key] == nil && len(t.refreshing) < t.opts.MaxConcurrentBuilds {
		if t.refreshing == nil {
			t.refreshing = map[string]bool{}
		}
		t.refreshing[key] = true
		flight := &sourceCall{done: make(chan struct{})}
		t.sources[key] = flight
		go t.refreshMetaInBackground(key, item, ttl, build, flight, true)
	}
	t.mu.Unlock()
}

func (t *Tree) loadRefreshingMetaWithStalePolicy(ctx context.Context, key string, ttl time.Duration, build func(context.Context) (any, int64, error), keepTransientStale bool) (any, error) {
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
	now := time.Now()
	stale := item.stale || (!item.expires.IsZero() && now.After(item.expires))
	if keepTransientStale {
		t.directoryStats.memoryHits.Add(1)
		if stale {
			t.directoryStats.staleServed.Add(1)
		}
	}
	if stale && !item.staleBackoffUntil.After(now) && !t.refreshing[key] && t.sources[key] == nil && len(t.refreshing) < t.opts.MaxConcurrentBuilds {
		if t.refreshing == nil {
			t.refreshing = map[string]bool{}
		}
		t.refreshing[key] = true
		flight := &sourceCall{done: make(chan struct{})}
		t.sources[key] = flight
		go t.refreshMetaInBackground(key, item, ttl, build, flight, keepTransientStale)
	}
	t.mu.Unlock()
	return value, nil
}

func (t *Tree) refreshMetaInBackground(key string, item *metaItem, ttl time.Duration, build func(context.Context) (any, int64, error), flight *sourceCall, keepTransientStale bool) {
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
	removePersistedDirectory := false
	if current == item || current == nil {
		if err != nil && keepTransientStale && keepDirectoryStale(item, err) {
			item.stale = true
			if item.staleFailures < 6 {
				item.staleFailures++
			}
			item.staleBackoffUntil = time.Now().Add(directoryRetryDelay(item.staleFailures))
			t.directoryStats.staleFailures.Add(1)
			t.directoryStats.retryBackoffs.Add(1)
		} else if keepTransientStale && errors.Is(err, context.Canceled) {
			// Mount shutdown or an interrupted refresh leaves a valid snapshot.
		} else if err != nil || size < 0 || size > t.opts.MetadataBytes {
			removePersistedDirectory = keepTransientStale
			if current == item {
				delete(t.meta, key)
				t.metaBytes -= item.bytes
			}
			if keepTransientStale {
				t.directoryStats.permanentFailures.Add(1)
			}
		} else {
			oldBytes := int64(0)
			if current == item {
				oldBytes = item.bytes
			}
			if t.makeMetadataRoomLocked(key, oldBytes, size) {
				t.seq++
				fetchedAt := time.Now()
				if directory, ok := fresh.(*cloudDirectory); ok && !directory.fetchedAt.IsZero() {
					fetchedAt = directory.fetchedAt
				}
				expires := time.Time{}
				if ttl > 0 {
					expires = fetchedAt.Add(ttl)
				}
				updated := &metaItem{key: key, value: fresh, bytes: size, expires: expires, seq: t.seq, fetchedAt: fetchedAt}
				t.meta[key] = updated
				t.metaBytes += size - oldBytes
				flight.value = fresh
			} else {
				err = errors.New("metadata cache cannot fit refreshed snapshot")
				if keepTransientStale && current == item {
					item.stale = true
					if item.staleFailures < 6 {
						item.staleFailures++
					}
					item.staleBackoffUntil = time.Now().Add(directoryRetryDelay(item.staleFailures))
					t.directoryStats.staleFailures.Add(1)
					t.directoryStats.retryBackoffs.Add(1)
				} else if current == item {
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
	if removePersistedDirectory && t.cache != nil && t.cacheScopeStable {
		_ = t.cache.Remove(t.directorySnapshotKey(directoryParentID(key)))
	}
	flight.err = err
	close(flight.done)
	t.mu.Unlock()
}

func keepDirectoryStale(item *metaItem, err error) bool {
	if item == nil || err == nil {
		return false
	}
	return faults.IsTransient(err)
}

func directoryRetryDelay(failures uint8) time.Duration {
	delay := time.Second << min(failures-1, uint8(5))
	return min(delay, 30*time.Second)
}

func directoryParentID(key string) int64 {
	var id int64
	_, _ = fmt.Sscanf(key, "dir:%d", &id)
	return id
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
