package mountfs

import (
	"context"
	"hash"
	"hash/crc32"
	"io"
	"math"
	"sync"
	"syscall"

	"github.com/bodgit/sevenzip"
	"github.com/nwaples/rardecode/v2"
)

// openArchiveMemberStream opens only the selected member's decoder. The caller
// owns the returned stream and the lifetime of reader. Closing a partial stream
// releases its decoder without draining the member; successful EOF retains the
// archive library's checksum validation. No decoded bytes are prefetched.
func openArchiveMemberStream(ctx context.Context, reader io.ReaderAt, size int64, m *member, password []byte) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if reader == nil || size < 0 || m == nil {
		return nil, syscall.EINVAL
	}
	if m.size > math.MaxInt64 {
		return nil, syscall.EFBIG
	}
	input := archiveMemberInput{ctx: ctx, reader: io.NewSectionReader(reader, 0, size)}
	wrap := func(r io.Reader, closer io.Closer) io.ReadCloser {
		return &archiveMemberStream{ctx: ctx, reader: r, closer: closer}
	}
	switch m.format {
	case ".7z":
		zr, err := sevenzip.NewReaderWithPassword(input, size, string(password))
		if err != nil {
			return nil, err
		}
		if m.ordinal < 0 || m.ordinal >= len(zr.File) {
			return nil, syscall.ESTALE
		}
		f := zr.File[m.ordinal]
		if f.Name != m.name || f.UncompressedSize != m.size || f.CRC32 != m.crc {
			return nil, syscall.ESTALE
		}
		offset, hasStream := f.StreamOffset()
		if !hasStream {
			if m.size != 0 {
				return nil, syscall.ESTALE
			}
			// Empty files have no decoder to own or retain in the file pool.
			r, err := f.Open()
			if err != nil {
				return nil, err
			}
			return wrap(&archiveMember7zChecksum{reader: r, hash: crc32.NewIEEE(), want: m.crc}, r), nil
		}
		streams := zr.Streams()
		if f.Stream < 0 || f.Stream >= len(streams) || streams[f.Stream].UncompressedSize > math.MaxInt64 {
			return nil, syscall.ESTALE
		}
		groupSize := int64(streams[f.Stream].UncompressedSize)
		if offset < 0 || offset > groupSize || int64(m.size) > groupSize-offset {
			return nil, syscall.ESTALE
		}
		if location := m.sevenStream; location != nil && (location.Stream != f.Stream || location.Offset != offset || location.Size != groupSize) {
			return nil, syscall.ESTALE
		}
		r, err := zr.OpenStreamWithReader(f.Stream, input)
		if err != nil {
			return nil, err
		}
		// File.Open.Close only returns a partial decoder to zr.pool. Own the
		// group stream directly so partial Close actually releases its codec.
		owned := wrap(r, r)
		if _, err := io.CopyN(io.Discard, owned, offset); err != nil {
			_ = owned.Close()
			return nil, err
		}
		return wrap(&archiveMember7zChecksum{reader: owned, hash: crc32.NewIEEE(), left: int64(m.size), want: m.crc}, owned), nil
	case ".rar":
		options := append(rarOptions(password), rardecode.FileSystem(singleArchiveFS{input, size}))
		if m.rarLocator != nil && m.rarLocator.Direct() {
			h, r, err := rardecode.OpenMember("archive.rar", *m.rarLocator, options...)
			if err != nil {
				return nil, err
			}
			if h.Name != m.name || h.UnPackedSize < 0 || uint64(h.UnPackedSize) != m.size {
				_ = r.Close()
				return nil, syscall.ESTALE
			}
			return wrap(r, r), nil
		}
		if m.ordinal < 0 {
			return nil, syscall.ESTALE
		}
		session, err := rardecode.OpenSolidSessionContext(ctx, "archive.rar", options...)
		if err != nil {
			return nil, err
		}
		for i := 0; i <= m.ordinal; i++ {
			sm, err := session.Next()
			if err != nil {
				_ = session.Close()
				return nil, err
			}
			if i != m.ordinal {
				continue
			}
			if sm.Header.Name != m.name || sm.Header.UnPackedSize < 0 || uint64(sm.Header.UnPackedSize) != m.size {
				_ = session.Close()
				return nil, syscall.ESTALE
			}
			// SolidMember.Close drains unread bytes to advance the decoder.
			// This stream never advances again: close the session directly.
			return wrap(sm, session), nil
		}
		_ = session.Close()
		return nil, syscall.ESTALE
	case ".zip":
		if m.file == nil || m.encrypted || m.flags&1 != 0 || m.file.Flags&1 != 0 {
			return nil, syscall.EOPNOTSUPP
		}
		r, err := m.file.Open()
		if err != nil {
			return nil, err
		}
		return wrap(r, r), nil
	default:
		return nil, syscall.EOPNOTSUPP
	}
}

type archiveMemberInput struct {
	ctx    context.Context
	reader io.ReaderAt
}

// sevenzip.File.Open bounds the member but does not verify its file CRC.
// Check length and nonzero CRCs, including a read consuming exactly its size.
// The decoder does not expose CRC presence, so zero retains the existing 7z
// compatibility rule: an optional CRC is unchecked, but length is still checked.
type archiveMember7zChecksum struct {
	reader io.Reader
	hash   hash.Hash32
	left   int64
	want   uint32
}

func (r *archiveMember7zChecksum) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.left == 0 {
		if r.want != 0 && r.hash.Sum32() != r.want {
			return 0, errArchiveIntegrity
		}
		return 0, io.EOF
	}
	if int64(len(p)) > r.left {
		p = p[:r.left]
	}
	n, err := r.reader.Read(p)
	_, _ = r.hash.Write(p[:n])
	r.left -= int64(n)
	if err != nil && err != io.EOF {
		return n, err
	}
	if r.left == 0 && r.want != 0 && r.hash.Sum32() != r.want {
		return n, errArchiveIntegrity
	}
	if err == io.EOF && r.left != 0 {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

func (r archiveMemberInput) ReadAt(p []byte, off int64) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.ReadAt(p, off)
}

type archiveMemberStream struct {
	mu          sync.Mutex
	ctx         context.Context
	reader      io.Reader
	closer      io.Closer
	closed      bool
	terminalErr error
}

func (r *archiveMemberStream) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, io.ErrClosedPipe
	}
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.terminalErr != nil {
		return 0, r.terminalErr
	}
	n, err := r.reader.Read(p)
	if err != nil {
		// io.CopyN can discard an error returned with its final requested
		// bytes. Keep it observable by the caller's explicit EOF check.
		r.terminalErr = err
	}
	return n, err
}

func (r *archiveMemberStream) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	err := r.closer.Close()
	r.reader, r.closer = nil, nil
	return err
}
