package mountfs

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"syscall"
	"time"

	"github.com/binghezhouke/123/mount123/internal/iostats"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/binghezhouke/123/mount123/internal/workqueue"
	"github.com/bodgit/sevenzip"
)

// Immutable metadata; no decoder, password or signed URL is persisted.
type sevenStreamLocation struct {
	Stream int   `json:"stream"`
	Offset int64 `json:"offset"`
	Size   int64 `json:"size"`
}

func (s *sevenStreamLocation) valid(memberSize uint64) bool {
	return s != nil && s.Stream >= 0 && s.Offset >= 0 && s.Size > 0 && s.Offset <= s.Size && memberSize <= uint64(s.Size-s.Offset)
}

const maxSolid7zCacheBytes int64 = 1 << 30
const solid7zFillTimeout = 5 * time.Minute

func (t *Tree) solid7zCacheable(m *member) bool {
	return m.format == ".7z" && m.sevenStream.valid(m.size) && m.sevenStream.Size <= min(maxSolid7zCacheBytes, t.cache.Capacity()/4)
}

func (t *Tree) solid7zKey(identity string, a *archiveDescriptor, m *member, password []byte) string {
	return t.diskCacheScope() + ":" + identity + fmt.Sprintf(":7z-stream-v1:%d:%d:", m.sevenStream.Stream, m.sevenStream.Size) + t.passwordTag(a, password)
}

// extractOtherMember keeps stream waiters outside the build gate. The shared
// stream fill alone owns a slot, preventing same-group waiters from occupying
// every slot while the only producer remains queued behind them.
func (t *Tree) extractOtherMember(ctx context.Context, source *storage.Remote, a *archiveDescriptor, m *member, password []byte, w io.Writer) error {
	reader, size, identity, err := t.archiveSource(ctx, source, a)
	if err != nil {
		return err
	}
	if t.solid7zCacheable(m) {
		group, err := t.acquireSolid7z(ctx, source, a, m, password, identity)
		if err == nil {
			defer group.Close()
			stream := io.NewSectionReader(growingGroupReader{ctx: ctx, group: group}, m.sevenStream.Offset, int64(m.size))
			hash := crc32.NewIEEE()
			n, err := io.CopyBuffer(io.MultiWriter(w, hash), stream, make([]byte, 256<<10))
			if err != nil {
				return err
			}
			if n != int64(m.size) {
				return io.ErrUnexpectedEOF
			}
			if hash.Sum32() != m.crc {
				return errArchiveIntegrity
			}
			return nil
		}
		// A full group must not make a formerly readable member fail merely
		// because its reservation cannot fit alongside active member caches.
		if !errors.Is(err, syscall.ENOSPC) {
			return err
		}
	}
	release, err := t.acquireBuild(ctx)
	if err != nil {
		return err
	}
	defer release()
	started := time.Now()
	defer t.observeStage(iostats.StageDecompression, started)
	return extractArchiveMember(ctx, reader, size, m, password, w)
}

func (t *Tree) acquireSolid7z(ctx context.Context, source *storage.Remote, a *archiveDescriptor, m *member, password []byte, identity string) (*storage.GrowingHandle, error) {
	location := *m.sevenStream
	key := t.solid7zKey(identity, a, m, password)
	// The asynchronous fill outlives this member's request and password buffer.
	// Capture an encrypted copy: on a cache hit this callback may never run.
	protected, err := t.encryptPassword(key, password)
	if err != nil {
		return nil, err
	}
	lifetime := t.ctx
	if workqueue.IsBackground(ctx) {
		lifetime = workqueue.Background(lifetime)
	}
	return t.cache.AcquireGrowingRetained(ctx, lifetime, key, location.Size, func(fillCtx context.Context, w io.Writer) error {
		fillCtx, cancel := context.WithTimeout(fillCtx, solid7zFillTimeout)
		defer cancel()
		password, err := t.decryptPassword(key, protected)
		if err != nil {
			return err
		}
		defer clear(password)
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
		started := time.Now()
		defer t.observeStage(iostats.StageDecompression, started)
		zr, err := sevenzip.NewReaderWithPassword(reader, size, string(password))
		if err != nil {
			return archiveReadError(fillCtx, err, password)
		}
		streams := zr.Streams()
		if location.Stream >= len(streams) || streams[location.Stream].UncompressedSize > math.MaxInt64 || int64(streams[location.Stream].UncompressedSize) != location.Size || m.ordinal < 0 || m.ordinal >= len(zr.File) {
			return syscall.ESTALE
		}
		f := zr.File[m.ordinal]
		offset, ok := f.StreamOffset()
		if !ok || f.Stream != location.Stream || offset != location.Offset || f.Name != m.name || f.UncompressedSize != m.size || f.CRC32 != m.crc {
			return syscall.ESTALE
		}
		ranges, err := zr.PackedRanges(location.Stream)
		if err != nil {
			return archiveReadError(fillCtx, err, password)
		}
		packed := newPacked7zReader(fillCtx, reader, ranges, t.cache.Capacity())
		body := reader
		if packed != nil {
			defer packed.Close()
			body = packed
		}
		rc, err := zr.OpenStreamWithReader(location.Stream, body)
		if err != nil {
			return archiveReadError(fillCtx, err, password)
		}
		err = copyValidated7zStream(fillCtx, zr, location.Stream, location.Size, rc, w)
		err = errors.Join(err, rc.Close())
		if err == nil && packed != nil {
			err = packed.Finish()
		}
		return archiveReadError(fillCtx, err, password)
	})
}

// Some solid groups omit their group CRC and provide only per-file CRCs.
// Check these too before publishing a group that will serve other members.
func copyValidated7zStream(ctx context.Context, zr *sevenzip.Reader, stream int, size int64, rc io.Reader, w io.Writer) error {
	reader := &contextReader{ctx: ctx, r: rc}
	buffer := make([]byte, 256<<10)
	var position int64
	for _, f := range zr.File {
		offset, ok := f.StreamOffset()
		if !ok || f.Stream != stream {
			continue
		}
		if offset != position || f.UncompressedSize > uint64(size-position) {
			return syscall.ESTALE
		}
		hash := crc32.NewIEEE()
		n, err := io.CopyBuffer(io.MultiWriter(w, hash), io.LimitReader(reader, int64(f.UncompressedSize)), buffer)
		if err != nil {
			return err
		}
		if n != int64(f.UncompressedSize) {
			return io.ErrUnexpectedEOF
		}
		if f.CRC32 != 0 && hash.Sum32() != f.CRC32 {
			return errArchiveIntegrity
		}
		position += n
	}
	if position != size {
		return syscall.ESTALE
	}
	// A final read surfaces the stream-level validation result.
	var tail [1]byte
	n, err := reader.Read(tail[:])
	if n != 0 {
		return syscall.EIO
	}
	if err != io.EOF {
		return err
	}
	return nil
}

type growingGroupReader struct {
	ctx   context.Context
	group *storage.GrowingHandle
}

func (r growingGroupReader) ReadAt(p []byte, off int64) (int, error) {
	return r.group.ReadAt(r.ctx, p, off)
}
