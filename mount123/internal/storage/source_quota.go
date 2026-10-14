package storage

import (
	"container/list"
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

// Source quotas bound retained data, not active readers. Pins and reservations
// may temporarily exceed the share; Close and publication trim the source as
// soon as its bytes become reclaimable. The global capacity remains hard.
const DefaultSourceCachePercent = 20

type cacheOwnerKey struct{}

func WithCacheOwner(ctx context.Context, owner string) context.Context {
	if owner == "" {
		return ctx
	}
	return context.WithValue(ctx, cacheOwnerKey{}, owner)
}

func cacheOwner(ctx context.Context) string {
	owner, _ := ctx.Value(cacheOwnerKey{}).(string)
	if len(owner) != 32 {
		return ""
	}
	if _, err := hex.DecodeString(owner); err != nil {
		return ""
	}
	return owner
}

type sourceResidency struct {
	used, reserved int64
	entries        int
	lru            [cacheClassCount]*list.List
}

// ConfigureSourceLimit is called before handing the cache to readers. Existing
// extent filenames already identify a source, so upgrading does not flush it.
func (c *Cache) ConfigureSourceLimit(percent int) error {
	if percent < 0 || percent > 100 {
		return fmt.Errorf("cache-file-percent must be between 0 and 100")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrClosed
	}
	c.sourcePercent = percent
	c.sourceLimit = c.max/100*int64(percent) + c.max%100*int64(percent)/100
	if percent > 0 && c.max > 0 && c.sourceLimit == 0 {
		c.sourceLimit = 1
	}
	for owner := range c.sources {
		if err := c.trimSourceLocked(owner, 0); err != nil {
			return err
		}
	}
	return nil
}

// SourceCapacity is the normal working-set bound used when choosing between
// full-member retention and block-at-a-time archive decoding.
func (c *Cache) SourceCapacity() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sourceLimit > 0 {
		return c.sourceLimit
	}
	return c.max
}

func (c *Cache) sourceLocked(owner string) *sourceResidency {
	if c.sources == nil {
		c.sources = make(map[string]*sourceResidency)
	}
	s := c.sources[owner]
	if s == nil {
		s = &sourceResidency{lru: newClassLRUs()}
		c.sources[owner] = s
	}
	return s
}

func (c *Cache) addSourceEntryLocked(id string, e *cacheEntry) {
	if e.owner == "" {
		return
	}
	s := c.sourceLocked(e.owner)
	s.used += e.size
	s.entries++
	e.ownerLRU = s.lru[e.class].PushFront(id)
}

func (c *Cache) removeSourceEntryLocked(e *cacheEntry) {
	if e.owner == "" {
		return
	}
	s := c.sources[e.owner]
	if s == nil {
		return
	}
	s.used -= e.size
	s.entries--
	if e.ownerLRU != nil {
		s.lru[e.class].Remove(e.ownerLRU)
		e.ownerLRU = nil
	}
	if s.entries == 0 && s.reserved == 0 {
		delete(c.sources, e.owner)
	}
}

func (c *Cache) reserveSourceLocked(owner string, size int64) {
	if owner == "" {
		return
	}
	s := c.sourceLocked(owner)
	s.reserved += size
	if s.entries == 0 && s.reserved == 0 {
		delete(c.sources, owner)
	}
}

func (c *Cache) trimSourceLocked(owner string, need int64) error {
	if owner == "" || c.sourceLimit <= 0 {
		return nil
	}
	for {
		s := c.sources[owner]
		if s == nil || s.used+s.reserved+need <= c.sourceLimit {
			return nil
		}
		var id string
		for class := cacheSpeculative; class <= cacheIndex && id == ""; class++ {
			for n := s.lru[class].Back(); n != nil; n = n.Prev() {
				key := n.Value.(string)
				if e := c.entries[key]; e != nil && e.pins == 0 {
					id = key
					break
				}
			}
		}
		if id == "" {
			return nil
		} // Never invalidate a live reader or producer.
		e := c.entries[id]
		if err := os.Remove(e.path); err != nil && !os.IsNotExist(err) {
			return err
		}
		c.sourceEvictions++
		c.sourceEvictedBytes += e.size
		c.recordCapacityEvictionLocked(id, e)
		c.removeEntryLocked(id, e)
	}
}

func ownedCachePath(path, owner string) string {
	if owner == "" {
		return path
	}
	return strings.TrimSuffix(path, ".blob") + ".o" + owner + ".blob"
}

func ownedFillPath(key, path, owner string) string {
	base, _ := splitOwnedCacheName(path)
	if _, _, identity, ok := parseRangeKey(key); ok && owner == identity[:32] {
		return base
	}
	return ownedCachePath(base, owner)
}

func splitOwnedCacheName(name string) (string, string) {
	base := strings.TrimSuffix(name, ".blob")
	if i := strings.LastIndex(base, ".o"); i >= 0 && len(base[i+2:]) == 32 {
		owner := base[i+2:]
		if _, err := hex.DecodeString(owner); err == nil {
			return base[:i] + ".blob", owner
		}
	}
	return name, ""
}

func (c *Cache) fillOwnerLocked(ctx context.Context, key string, class cacheClass) string {
	if class == cacheIndex || indexKindForKey(key) != indexKindUnknown {
		return ""
	}
	if _, _, identity, ok := parseRangeKey(key); ok {
		if owner := c.rangeOwners[identity]; owner != "" {
			return owner
		}
		return identity[:32]
	}
	return cacheOwner(ctx)
}

func (c *Cache) adoptSourceLocked(id string, e *cacheEntry, owner string) error {
	if owner == "" || e.owner == owner || e.class == cacheIndex || e.kind != indexKindUnknown {
		return nil
	}
	base, _ := splitOwnedCacheName(e.path)
	path := ownedCachePath(base, owner)
	if err := os.Rename(e.path, path); err != nil {
		return err
	}
	c.removeSourceEntryLocked(e)
	e.owner, e.path = owner, path
	c.addSourceEntryLocked(id, e)
	return nil
}

// SetCacheOwner joins source versions and derived archive data under a stable,
// opaque cloud-file identity. Existing ranges are adopted without re-fetching.
func (r *Remote) SetCacheOwner(owner string) error {
	if len(owner) != 32 {
		return fmt.Errorf("invalid cache source owner")
	}
	if _, err := hex.DecodeString(owner); err != nil {
		return err
	}
	c := r.cache
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrClosed
	}
	if c.rangeOwners == nil {
		c.rangeOwners = make(map[string]string)
	}
	c.rangeOwners[r.rangeID] = owner
	for _, extent := range c.ranges[r.rangeID] {
		if e := c.entries[extent.id]; e != nil {
			if err := c.adoptSourceLocked(extent.id, e, owner); err != nil {
				return err
			}
		}
	}
	return c.trimSourceLocked(owner, 0)
}

// OpenCachedContext also adopts pre-quota decoded blocks on a warm hit.
func (c *Cache) OpenCachedContext(ctx context.Context, key string, size int64) (*Handle, error) {
	h, err := c.OpenCached(key, size)
	if err != nil || h == nil {
		return h, err
	}
	c.mu.Lock()
	if e := c.entries[h.key]; e != nil {
		err = c.adoptSourceLocked(h.key, e, cacheOwner(ctx))
	}
	c.mu.Unlock()
	if err != nil {
		_ = h.Close()
		return nil, err
	}
	return h, nil
}
