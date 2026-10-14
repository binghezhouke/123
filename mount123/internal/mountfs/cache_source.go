package mountfs

import (
	"context"
	"fmt"

	"github.com/binghezhouke/123/mount123/internal/storage"
)

func (t *Tree) cacheSourceOwner(id int64) string {
	if t.cache == nil || id <= 0 {
		return ""
	}
	return t.cache.StableDigest("cache-source-v1", fmt.Sprintf("%s:%d", t.diskCacheScope(), id))[:32]
}

func (t *Tree) archiveCacheContext(ctx context.Context, a *archiveDescriptor) context.Context {
	if a == nil {
		return ctx
	}
	return storage.WithCacheOwner(ctx, t.cacheSourceOwner(a.id))
}

// Reserve part of a source's share for its compressed input and other members.
// Oversized members use the existing demand decoder instead of reserving their
// full uncompressed length, in both web and FUSE readers.
func (t *Tree) demandCacheMember(m *member) bool {
	return !t.opts.DisableStreamMembers && m != nil && t.cache != nil && t.cache.SourceCapacity() < t.cache.Capacity() && m.size > uint64(t.cache.SourceCapacity()/2)
}
