package mountfs

import (
	"context"
	"fmt"
	"io"
	"sync"
	"syscall"
	"time"

	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/bodgit/sevenzip"
)

const nestedMemberBlockBytes int64 = 256 << 10

// nestedMemberReader persists only demanded decompressed blocks. The live
// decoder and position form a forward continuation point; no codec snapshot is
// serialized. A backward cache miss (or idle expiry) replays the stream. Solid
// streams still have to decode their prefix on the first tail read.
// Partial blocks are not full-member CRC verified; they are isolated from the
// verified full-member cache. The inner parser validates its header CRCs.
type nestedMemberReader struct {
	mu       sync.Mutex
	ctx      context.Context
	cache    *storage.Cache
	key      string
	size     int64
	open     func(context.Context) (io.ReadCloser, error)
	gate     func(context.Context) (func(), error)
	stream   io.ReadCloser
	position int64
	idle     *time.Timer
	closed   bool
}

func (t *Tree) newNestedMemberReader(ctx context.Context, source *storage.Remote, a *archiveDescriptor, identity string, m *member, password []byte) (*nestedMemberReader, error) {
	key := t.diskCacheScope() + ":" + identity + fmt.Sprintf(":nested-range-v1:%q:%d:%08x:", m.name, m.size, m.crc) + t.passwordTag(a, password)
	protected, err := t.encryptPassword(key, password)
	if err != nil {
		return nil, err
	}
	return &nestedMemberReader{ctx: context.WithoutCancel(ctx), cache: t.cache, key: key, size: int64(m.size), gate: t.acquireBuild, open: func(readCtx context.Context) (io.ReadCloser, error) {
		pw, err := t.decryptPassword(key, protected)
		if err != nil {
			return nil, err
		}
		defer clear(pw)
		reader, size, current, err := t.archiveSource(readCtx, source, a)
		if err != nil {
			return nil, err
		}
		if current != identity {
			return nil, syscall.ESTALE
		}
		zr, err := sevenzip.NewReaderWithPassword(reader, size, string(pw))
		if err != nil {
			return nil, archiveReadError(readCtx, err, pw)
		}
		if m.ordinal < 0 || m.ordinal >= len(zr.File) {
			return nil, syscall.ESTALE
		}
		f := zr.File[m.ordinal]
		if f.Name != m.name || f.UncompressedSize != m.size || f.CRC32 != m.crc {
			return nil, syscall.ESTALE
		}
		return f.Open()
	}}, nil
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
	if r.closed {
		return nil
	}
	r.closed = true
	if r.idle != nil {
		r.idle.Stop()
	}
	r.reset()
	r.open = nil
	return nil
}

func (r *nestedMemberReader) ReadAt(p []byte, off int64) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
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
	total := 0
	for len(p) > 0 && off < r.size {
		if err := r.ctx.Err(); err != nil {
			return total, err
		}
		start := off / nestedMemberBlockBytes * nestedMemberBlockBytes
		length := min(nestedMemberBlockBytes, r.size-start)
		handle, err := r.cache.Acquire(r.ctx, fmt.Sprintf("%s:block:%d", r.key, start), length, func(ctx context.Context, w io.Writer) error {
			release, err := r.gate(ctx)
			if err != nil {
				return err
			}
			defer release()
			if r.stream != nil && start < r.position {
				r.reset()
			}
			if r.stream == nil {
				r.stream, err = r.open(ctx)
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
				r.reset()
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
	// Bound retained codec state while preserving a continuation point for the
	// next nearby demand. Disk blocks remain reusable after the decoder closes.
	if r.stream != nil {
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

var _ io.ReaderAt = (*nestedMemberReader)(nil)
var _ io.Closer = (*nestedMemberReader)(nil)
