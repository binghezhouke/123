package mountfs

import (
	"context"
	"errors"
	"syscall"

	"github.com/nwaples/rardecode/v2"
)

// Stored RAR members have no decoder prefix dependency. The library owns the
// validated range and optional full-read checksum; the ancestor supplies each
// request's context and bytes without retaining the constructor's reader.
type nestedRARStoredInput struct {
	input archiveReaderAt
	plan  *rardecode.StoredMemberRange
}

func (r *nestedRARStoredInput) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return r.plan.ReadAtWithReader(archiveRequestReader{ctx: ctx, input: r.input}, p, off)
}

func (r *nestedRARStoredInput) Close() error { return r.plan.Close() }

func nestedRARStoredMemberReader(ctx context.Context, input archiveReaderAt, size int64, m *member, password []byte) (*nestedRARStoredInput, bool, error) {
	reader := &nestedSourceReader{ReaderAt: archiveRequestReader{ctx: ctx, input: input}}
	options := append(rarOptions(password), rardecode.FileSystem(singleArchiveFS{reader, size}))
	h, plan, err := rardecode.OpenStoredMemberRange("archive.rar", *m.rarLocator, options...)
	if err != nil {
		if sourceErr := reader.sourceError(); sourceErr != nil {
			return nil, false, sourceErr
		}
		if errors.Is(err, rardecode.ErrUnsupportedStoredRange) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if h.Name != m.name || h.UnPackedSize < 0 || uint64(h.UnPackedSize) != m.size {
		_ = plan.Close()
		return nil, false, syscall.ESTALE
	}
	if err := ctx.Err(); err != nil {
		_ = plan.Close()
		return nil, false, err
	}
	return &nestedRARStoredInput{input: input, plan: plan}, true, nil
}
