package mountfs

import (
	"context"
	"io"
	"sync"

	"github.com/binghezhouke/123/mount123/internal/storage"
)

// A frozen nested part does not map its local header or start an ancestor
// decoder until that part is read. Failed/canceled initialization is retryable.
type nestedVolumeInput struct {
	mu            sync.Mutex
	owner         *Tree
	parent        *nestedArchive
	member        *member
	input         archiveReaderAt
	reader        io.Closer
	pending       chan struct{}
	closed        bool
	passwordKnown bool
}

func (r *nestedVolumeInput) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	for {
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			return 0, storage.ErrClosed
		}
		if input := r.input; input != nil {
			r.mu.Unlock()
			return input.ReadAtContext(ctx, p, off)
		}
		if pending := r.pending; pending != nil {
			r.mu.Unlock()
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-pending:
			}
			continue
		}
		pending := make(chan struct{})
		r.pending = pending
		r.mu.Unlock()
		var input archiveReaderAt
		var reader io.Closer
		var err error
		if r.passwordKnown {
			var password []byte
			password, err = r.owner.viewPassword(ctx, r.parent, false)
			if err == nil {
				var m *member
				m, err = viewMember(r.parent, r.member)
				if err == nil {
					input, reader, err = r.owner.viewMemberInputPrepared(ctx, r.parent, m, password, true)
				}
			}
			clear(password)
		} else {
			input, reader, err = r.owner.viewMemberInput(ctx, r.parent, r.member, true)
		}
		r.mu.Lock()
		closed := r.closed
		if err == nil && !closed {
			r.input, r.reader = input, reader
		}
		r.pending = nil
		close(pending)
		r.mu.Unlock()
		if closed {
			if reader != nil {
				_ = reader.Close()
			}
			return 0, storage.ErrClosed
		}
		if err != nil {
			if reader != nil {
				_ = reader.Close()
			}
			return 0, err
		}
		return input.ReadAtContext(ctx, p, off)
	}
}

func (r *nestedVolumeInput) Close() error {
	r.mu.Lock()
	r.closed = true
	reader := r.reader
	r.reader, r.input = nil, nil
	r.mu.Unlock()
	if reader != nil {
		return reader.Close()
	}
	return nil
}
