package mountfs

import (
	"context"
	"io"

	"github.com/binghezhouke/123/mount123/internal/storage"
)

// serviceGrowingReader binds HTTP read waits to the request while the shared
// producer follows the same lifetime and retention policy as FUSE.
type serviceGrowingReader struct {
	ctx    context.Context
	handle *storage.GrowingHandle
}

func (r *serviceGrowingReader) ReadAt(p []byte, off int64) (int, error) {
	return r.handle.ReadAt(r.ctx, p, off)
}

func (r *serviceGrowingReader) Close() error { return r.handle.Close() }

func (s *Service) acquireMemberReader(ctx context.Context, key string, size int64, retainZIP bool, fill func(context.Context, io.Writer) error) (io.ReaderAt, error) {
	var growing *storage.GrowingHandle
	var err error
	if retainZIP {
		growing, err = s.tree.acquireGrowingZIPMember(ctx, key, uint64(size), fill)
	}
	if err == nil && growing == nil {
		growing, err = s.tree.acquireGrowingMember(ctx, key, uint64(size), fill)
	}
	if err != nil {
		return nil, err
	}
	if growing != nil {
		return &serviceGrowingReader{ctx: ctx, handle: growing}, nil
	}
	return s.tree.cache.Acquire(ctx, key, size, fill)
}

// The producer can outlive OpenArchiveMember and its deferred password clear.
// Keep only a protected copy in the closure, and decrypt it for the fill.
func (s *Service) memberFillWithPassword(key string, password []byte, fill func(context.Context, io.Writer, []byte) error) (func(context.Context, io.Writer) error, error) {
	protected, err := s.tree.encryptPassword(key, password)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, w io.Writer) error {
		pw, err := s.tree.decryptPassword(key, protected)
		if err != nil {
			return err
		}
		defer clear(pw)
		return archiveReadError(ctx, fill(ctx, w, pw), pw)
	}, nil
}
