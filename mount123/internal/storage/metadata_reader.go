package storage

import (
	"context"
	"fmt"
	"io"
)

// NewMetadataReader creates a sequential archive-header reader. It starts with
// sparse reads, then trades bandwidth for fewer round trips when successive
// headers are close together. A reader belongs to one scan, not multiple callers.
func (r *Remote) NewMetadataReader(ctx context.Context) io.ReaderAt {
	return &metadataReader{remote: r, ctx: ctx, previous: -1}
}

type metadataReader struct {
	remote     *Remote
	ctx        context.Context
	previous   int64
	dense      int
	windowSize int64
	start      int64
	buffer     []byte
}

func (r *metadataReader) ReadAt(p []byte, off int64) (int, error) {
	ctx, cancel := combineContexts(r.remote.lifetimeCtx, r.ctx)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if off < 0 {
		return 0, fmt.Errorf("negative offset")
	}
	if len(p) == 0 {
		return 0, nil
	}
	if off >= r.remote.size {
		return 0, io.EOF
	}
	if r.previous >= 0 {
		gap := off - r.previous
		if gap < 0 || gap > 1<<20 {
			r.dense = 0
			r.windowSize = 0
		} else if gap > 0 {
			r.dense++
		}
	}
	r.previous = off
	if r.windowSize == 0 && r.dense >= 3 {
		r.windowSize = 1 << 20
	}
	n := 0
	for n < len(p) && off+int64(n) < r.remote.size {
		pos := off + int64(n)
		if pos >= r.start && pos < r.start+int64(len(r.buffer)) {
			n += copy(p[n:], r.buffer[pos-r.start:])
			continue
		}
		if r.windowSize == 0 {
			got, err := r.remote.ReadMetadataAtContext(ctx, p[n:], pos)
			return n + got, err
		}
		// Each fill is one large Range request, not a loop of 1 MiB requests.
		start := pos
		size := min(r.windowSize, r.remote.size-start)
		key := fmt.Sprintf("metadata-window:%s:%d:%d", r.remote.key, start, size)
		h, err := r.remote.cache.Acquire(ctx, key, size, func(ctx context.Context, w io.Writer) error {
			return r.remote.fetchBlock(ctx, start, start+size-1, w)
		})
		if err != nil {
			return n, err
		}
		if cap(r.buffer) < int(size) {
			r.buffer = make([]byte, size)
		} else {
			r.buffer = r.buffer[:size]
		}
		_, err = h.ReadAt(r.buffer, 0)
		h.Close()
		if err != nil {
			r.buffer = nil
			return n, err
		}
		r.start = start
		r.windowSize = min(r.windowSize*4, 16<<20)
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}
