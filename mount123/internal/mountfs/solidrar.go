package mountfs

import (
	"context"
	"fmt"
	"hash/crc32"
	"io"
	"syscall"
	"time"

	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/binghezhouke/123/mount123/internal/workqueue"
	"github.com/nwaples/rardecode/v2"
)

const solidRARFillTimeout = 5 * time.Minute

func (t *Tree) solidRARCacheable(m *member) bool {
	if m == nil || t.cache == nil {
		return false
	}
	limit := min(maxSolidRARCacheBytes, t.cache.Capacity()/4)
	return m.format == ".rar" && m.rarLocator != nil && m.rarGroupSize > 0 && m.rarGroupOffset <= m.rarGroupSize && m.size <= m.rarGroupSize-m.rarGroupOffset && m.rarGroupSize <= uint64(limit)
}

func (t *Tree) solidRARKey(identity string, a *archiveDescriptor, m *member, password []byte) string {
	return t.diskCacheScope() + ":" + identity + fmt.Sprintf(":rar-solid-v1:%d:%d:", m.rarGroupStart, m.rarGroupSize) + t.passwordTag(a, password)
}

func (t *Tree) acquireSolidRAR(ctx context.Context, source *storage.Remote, a *archiveDescriptor, m *member, password []byte, identity string) (*storage.GrowingHandle, error) {
	key := t.solidRARKey(identity, a, m, password)
	protected, err := t.encryptPassword(key, password)
	if err != nil {
		return nil, err
	}
	lifetime := t.ctx
	if workqueue.IsBackground(ctx) {
		lifetime = workqueue.Background(lifetime)
	}
	return t.cache.AcquireGrowingRetained(ctx, lifetime, key, int64(m.rarGroupSize), func(fillCtx context.Context, w io.Writer) error {
		fillCtx, cancel := context.WithTimeout(fillCtx, solidRARFillTimeout)
		defer cancel()
		pw, err := t.decryptPassword(key, protected)
		if err != nil {
			return err
		}
		defer clear(pw)
		reader, size, currentIdentity, err := t.archiveSource(fillCtx, source, a)
		if err != nil {
			return err
		}
		if currentIdentity != identity {
			return syscall.ESTALE
		}
		release, err := t.acquireBuild(fillCtx)
		if err != nil {
			return err
		}
		defer release()
		session, err := rardecode.OpenSolidSessionContext(fillCtx, "archive.rar", append(rarOptions(pw), rardecode.FileSystem(singleArchiveFS{reader, size}))...)
		if err != nil {
			return archiveReadError(fillCtx, err, pw)
		}
		defer session.Close()
		for i := 0; i < m.rarGroupStart; i++ {
			member, err := session.Next()
			if err != nil {
				return archiveReadError(fillCtx, err, pw)
			}
			if err := member.Close(); err != nil {
				return archiveReadError(fillCtx, err, pw)
			}
		}
		var written uint64
		for written < m.rarGroupSize {
			member, err := session.Next()
			if err != nil {
				return archiveReadError(fillCtx, err, pw)
			}
			if member.Header.UnPackedSize < 0 {
				return syscall.EIO
			}
			n, err := member.WriteTo(w)
			if err != nil {
				return archiveReadError(fillCtx, err, pw)
			}
			written += uint64(n)
			if written > m.rarGroupSize {
				return syscall.EIO
			}
		}
		if written != m.rarGroupSize {
			return io.ErrUnexpectedEOF
		}
		return nil
	})
}

func (t *Tree) extractSolidRARMember(ctx context.Context, source *storage.Remote, a *archiveDescriptor, m *member, password []byte, identity string, w io.Writer) error {
	group, err := t.acquireSolidRAR(ctx, source, a, m, password, identity)
	if err != nil {
		return err
	}
	defer group.Close()
	stream := io.NewSectionReader(growingGroupReader{ctx: ctx, group: group}, int64(m.rarGroupOffset), int64(m.size))
	hash := crc32.NewIEEE()
	n, err := io.CopyBuffer(io.MultiWriter(w, hash), stream, make([]byte, 256<<10))
	if err != nil {
		return err
	}
	if n != int64(m.size) {
		return io.ErrUnexpectedEOF
	}
	if m.crc != 0 && hash.Sum32() != m.crc {
		return errArchiveIntegrity
	}
	return nil
}
