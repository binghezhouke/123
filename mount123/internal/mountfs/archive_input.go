package mountfs

import (
	"context"
	"io"
	"syscall"
)

// archiveReaderAt carries the current operation through every ancestor layer.
// storage.Remote and nestedMemberReader both implement this contract.
type archiveReaderAt interface {
	ReadAtContext(context.Context, []byte, int64) (int, error)
}

type archiveRequestReader struct {
	ctx   context.Context
	input archiveReaderAt
}

func (r archiveRequestReader) ReadAt(p []byte, off int64) (int, error) {
	return r.input.ReadAtContext(r.ctx, p, off)
}

type archivePlainInput struct{ reader io.ReaderAt }

func (r archivePlainInput) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.ReadAt(p, off)
}

type archiveOffsetInput struct {
	input       archiveReaderAt
	start, size int64
}

func (r archiveOffsetInput) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, syscall.EINVAL
	}
	if len(p) == 0 {
		return 0, nil
	}
	if off >= r.size {
		return 0, io.EOF
	}
	length := min(int64(len(p)), r.size-off)
	n, err := r.input.ReadAtContext(ctx, p[:length], r.start+off)
	if err == nil && n < len(p) {
		err = io.EOF
	}
	return n, err
}

type archiveConcatInput struct {
	parts []archiveReaderAt
	sizes []int64
}

func (r archiveConcatInput) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, syscall.EINVAL
	}
	if len(p) == 0 {
		return 0, nil
	}
	total := 0
	for i, size := range r.sizes {
		if off >= size {
			off -= size
			continue
		}
		amount := min(int64(len(p)), size-off)
		n, err := r.parts[i].ReadAtContext(ctx, p[:amount], off)
		total += n
		p = p[n:]
		if err != nil && !(err == io.EOF && int64(n) == amount) {
			return total, err
		}
		if int64(n) != amount {
			return total, io.ErrUnexpectedEOF
		}
		if len(p) == 0 {
			return total, nil
		}
		off = 0
	}
	return total, io.EOF
}
