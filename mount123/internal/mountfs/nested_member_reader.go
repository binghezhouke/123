package mountfs

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/binghezhouke/123/mount123/internal/storage"
)

const nestedMemberBlockBytes int64 = 1 << 20

// Ancestors fill synchronously while a child block is reserved. Budget those
// reservations across the maximum chain, leaving half for remote range reads.
// Normal caches retain 1 MiB blocks; small caches use power-of-two blocks.
func (t *Tree) nestedBlockBytes() int64 {
	depth := int64(defaults(t.opts).MaxArchiveDepth)
	budget := t.cache.SourceCapacity() / 2 / (depth + 1)
	block := nestedMemberBlockBytes
	for block > budget && block > 1 {
		block /= 2
	}
	return block
}

// nestedMemberReader persists only demanded decompressed blocks. The live
// decoder and position form a forward continuation point. An optional resume
// source can skip already indexed compressed prefixes after a backward miss or
// idle expiry; formats without one still replay the prefix.
// Partial blocks are not full-member CRC verified; they are isolated from the
// verified full-member cache. The inner parser validates its header CRCs.
type nestedMemberReader struct {
	mu         sync.Mutex
	serial     sync.Once
	turn       chan struct{}
	owner      *Tree
	operation  *nestedReadContext
	ctx        context.Context
	cache      *storage.Cache
	key        string
	cacheOwner string
	size       int64
	blockBytes int64
	open       func(context.Context) (io.ReadCloser, error)
	// Optional shared group continuation. False selects the private stream;
	// errors are never converted to a retry that could hide corruption.
	fill func(context.Context, int64, int64, io.Writer) (bool, error)
	// resume returns a stream starting after the current position and at or
	// before the target, or nil when no reusable checkpoint is available.
	resume   func(context.Context, int64, int64) (io.ReadCloser, int64, error)
	gate     func(context.Context) (func(), error)
	stream   io.ReadCloser
	position int64
	idle     *time.Timer
	poolIdle bool // Pool generation/timer owns idle expiry for leased readers.
	// A child-index checkpoint collector may finish an already-open stream
	// through a cached final block, but never opens or replays to create points.
	finishCachedTail bool
	closed           atomic.Bool
}

func (r *nestedMemberReader) blockSize() int64 {
	if r.blockBytes > 0 {
		return r.blockBytes
	}
	return nestedMemberBlockBytes
}

func (r *nestedMemberReader) reset() {
	if r.stream != nil {
		_ = r.stream.Close()
		r.stream = nil
	}
	r.position = 0
}

func (r *nestedMemberReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed.Load() {
		return nil
	}
	r.closed.Store(true)
	if r.idle != nil {
		r.idle.Stop()
	}
	r.reset()
	r.open = nil
	r.fill = nil
	r.resume = nil
	return nil
}

func (r *nestedMemberReader) ReadAt(p []byte, off int64) (int, error) {
	return r.ReadAtContext(r.ctx, p, off)
}

func (r *nestedMemberReader) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	ctx = storage.WithCacheOwner(ctx, r.cacheOwner)
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.closed.Load() {
		return 0, storage.ErrClosed
	}
	if off < 0 {
		return 0, syscall.EINVAL
	}
	if len(p) == 0 {
		return 0, nil
	}
	if off >= r.size {
		return 0, io.EOF
	}
	// A complete disk hit needs neither the decoder mutex nor a build permit.
	// Stop on the first miss without creating a flight; only then enter the
	// recursive admission path. Other readers may publish more blocks meanwhile.
	cached, err := r.readCached(ctx, p, off)
	if err != nil || cached == len(p) {
		return cached, err
	}
	p, off = p[cached:], off+int64(cached)
	decoded, err := r.readUncached(ctx, p, off)
	return cached + decoded, err
}

func (r *nestedMemberReader) readCached(ctx context.Context, p []byte, off int64) (int, error) {
	total := 0
	for len(p) > 0 && off < r.size {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		if err := r.ctx.Err(); err != nil {
			return total, err
		}
		blockBytes := r.blockSize()
		start := off / blockBytes * blockBytes
		length := min(blockBytes, r.size-start)
		h, err := r.cache.OpenCachedContext(ctx, fmt.Sprintf("%s:block:%d", r.key, start), length)
		if err != nil || h == nil {
			return total, err
		}
		amount := min(int64(len(p)), start+length-off)
		n, err := h.ReadAt(p[:amount], off-start)
		closeErr := h.Close()
		total += n
		if err != nil {
			return total, err
		}
		if closeErr != nil {
			return total, closeErr
		}
		if n != int(amount) {
			return total, io.ErrUnexpectedEOF
		}
		off, p = off+int64(n), p[n:]
	}
	if len(p) != 0 {
		return total, io.EOF
	}
	return total, nil
}

func (r *nestedMemberReader) readUncached(ctx context.Context, p []byte, off int64) (int, error) {
	// Mount shutdown also interrupts requests whose caller has no deadline.
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(r.ctx, cancel)
	defer func() { stop(); cancel() }()
	var release func()
	var err error
	if r.owner != nil {
		ctx, release, err = r.owner.acquireNestedDecode(ctx)
	} else {
		release, err = r.gate(ctx)
	}
	if err != nil {
		return 0, err
	}
	defer release()
	// Waiters must be cancellable even while another request owns the decoder.
	r.serial.Do(func() { r.turn = make(chan struct{}, 1) })
	select {
	case r.turn <- struct{}{}:
		defer func() { <-r.turn }()
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed.Load() {
		return 0, storage.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if r.operation == nil {
		r.operation = &nestedReadContext{}
	}
	r.operation.bind(ctx)
	defer r.operation.bind(nil)
	if off < 0 {
		return 0, syscall.EINVAL
	}
	if len(p) == 0 {
		return 0, nil
	}
	if off >= r.size {
		return 0, io.EOF
	}
	total := 0
	for len(p) > 0 && off < r.size {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		blockBytes := r.blockSize()
		start := off / blockBytes * blockBytes
		length := min(blockBytes, r.size-start)
		handle, err := r.cache.Acquire(ctx, fmt.Sprintf("%s:block:%d", r.key, start), length, func(ctx context.Context, w io.Writer) error {
			if r.fill != nil {
				if handled, err := r.fill(ctx, start, length, w); handled || err != nil {
					return err
				}
			}
			var err error
			if r.stream != nil && start < r.position {
				r.reset()
			}
			if r.resume != nil && start > r.position {
				candidate, position, resumeErr := r.resume(r.operation, start, r.position)
				if resumeErr != nil {
					return resumeErr
				}
				if candidate != nil {
					if position <= r.position || position > start {
						_ = candidate.Close()
						return syscall.EIO
					}
					r.reset()
					r.stream, r.position = candidate, position
				}
			}
			if r.stream == nil {
				r.stream, err = r.open(r.operation)
				if err != nil {
					r.reset()
					return err
				}
			}
			input := &contextReader{ctx: ctx, r: r.stream}
			skipped, err := io.CopyN(io.Discard, input, start-r.position)
			r.position += skipped
			if err == nil {
				var n int64
				n, err = io.CopyN(w, input, length)
				r.position += n
			}
			if err != nil {
				r.reset()
				return err
			}
			if r.position == r.size {
				if err := r.finishEOF(ctx); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return total, err
		}
		amount := min(int64(len(p)), start+length-off)
		n, err := handle.ReadAt(p[:amount], off-start)
		closeErr := handle.Close()
		total += n
		off += int64(n)
		p = p[n:]
		if err != nil {
			return total, err
		}
		if closeErr != nil {
			return total, closeErr
		}
		if n != int(amount) {
			return total, io.ErrUnexpectedEOF
		}
	}
	// A preceding uncached block can advance this collector almost to EOF,
	// while the final block is already cached by a different reading role.
	// Finish only that bounded remainder inside the admitted operation. Pure
	// disk hits and collectors farther from EOF never enter this path.
	remaining := r.size - r.position
	if r.finishCachedTail && off == r.size && r.stream != nil && remaining > 0 && remaining <= r.blockSize() {
		input := &contextReader{ctx: ctx, r: r.stream}
		n, err := io.CopyN(io.Discard, input, remaining)
		r.position += n
		if err != nil {
			r.reset()
			return total, err
		}
		if err := r.finishEOF(ctx); err != nil {
			return total, err
		}
	}
	// Bound retained codec state while preserving a continuation point for the
	// next nearby demand. Disk blocks remain reusable after the decoder closes.
	if r.stream != nil && !r.poolIdle {
		if r.idle != nil {
			r.idle.Stop()
		}
		r.idle = time.AfterFunc(30*time.Second, func() { r.mu.Lock(); defer r.mu.Unlock(); r.reset() })
	}
	if len(p) > 0 {
		return total, io.EOF
	}
	return total, nil
}

// CopyN can stop at the advertised size before surfacing a checksum/source
// error supplied with the final bytes. Always confirm terminal EOF, then close.
func (r *nestedMemberReader) finishEOF(ctx context.Context) error {
	var trailing [1]byte
	n, err := (&contextReader{ctx: ctx, r: r.stream}).Read(trailing[:])
	if n != 0 {
		err = errArchiveIntegrity
	} else if err == nil {
		err = io.ErrNoProgress
	}
	r.reset()
	if err == io.EOF {
		return nil
	}
	return err
}

var _ io.ReaderAt = (*nestedMemberReader)(nil)
var _ io.Closer = (*nestedMemberReader)(nil)
