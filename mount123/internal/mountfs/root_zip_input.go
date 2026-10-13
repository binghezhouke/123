package mountfs

import (
	"context"
	"errors"
	"io"
	"math"
	"syscall"

	"github.com/binghezhouke/123/mount123/internal/storage"
)

// rootZIPDataOffset validates local metadata against the raw archive extent;
// synthetic index metadata must never enlarge the readable member payload.
func rootZIPDataOffset(ctx context.Context, m *member, prepared *preparedArchiveInput) (int64, error) {
	bound := *m
	bound.reader = &contextZIPReaderAt{source: archiveOffsetInput{input: prepared.input, size: prepared.size}, gate: make(chan struct{}, 1)}
	offset, err := bound.dataOffset(ctx)
	if err != nil {
		return 0, err
	}
	if m.compressed > math.MaxInt64 || offset < 0 || offset > prepared.size || int64(m.compressed) > prepared.size-offset {
		return 0, errors.New("ZIP member exceeds raw archive bounds")
	}
	return offset, nil
}

func rootZIPMemberIdentity(prepared *preparedArchiveInput) string {
	if prepared.layout.kind == archiveSingle {
		if source, ok := prepared.input.(*storage.Remote); ok {
			return source.Key()
		}
	}
	return prepared.identity
}

// Synthetic index bytes are already cached metadata. Only reads delegated to
// the frozen raw input count toward index progress.
type archiveIndexProgressInput struct {
	input    archiveReaderAt
	size     int64
	progress func(int64)
}

func (r archiveIndexProgressInput) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	n, err := r.input.ReadAtContext(ctx, p, off)
	if n > 0 {
		r.progress(min(r.size, off+int64(n)))
	}
	return n, err
}

func (n *Node) newArchiveInputHandle(input archiveReaderAt, base int64, size uint64) *handle {
	h := &handle{input: input, base: base, size: size}
	if source, ok := input.(readAheadSource); ok && !n.tree.opts.DisableReadAhead && n.tree.opts.ReadAheadMaxBytes > 0 && base >= 0 && size <= uint64(math.MaxInt64-base) {
		h.readAhead = newReadAhead(n.tree.ctx, source, base, size, n.tree.opts.ReadAheadMaxBytes)
		if n.tree.cache != nil {
			h.readAhead.statsTracker = n.tree.cache.IOStats()
		}
	}
	return h
}

// Prefetch uses the same frozen volume order as foreground reads. Source
// initialization stays lazy, including when the requested range spans disks.
func (r *volumeReaderAt) PrefetchRangeAtContext(ctx context.Context, off, length int64) error {
	if off < 0 || length < 0 {
		return syscall.EINVAL
	}
	for i, size := range r.sizes {
		if length == 0 {
			return nil
		}
		if off >= size {
			off -= size
			continue
		}
		amount := min(length, size-off)
		source, err := r.sourceAt(ctx, i)
		if err != nil {
			return err
		}
		if err := source.PrefetchRangeAtContext(ctx, off, amount); err != nil {
			return err
		}
		length -= amount
		off = 0
	}
	if length != 0 {
		return io.EOF
	}
	return nil
}

func (r *volumeReaderAt) DownloadStats() storage.DownloadStats {
	if r.owner == nil || r.owner.cache == nil {
		return storage.DownloadStats{}
	}
	return r.owner.cache.DownloadStats()
}
