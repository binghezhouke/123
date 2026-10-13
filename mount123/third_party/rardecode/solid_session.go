package rardecode

import (
	stdcontext "context"
	"io"
	"io/fs"
)

// SolidSession reads RAR members in archive order while retaining the
// decoder state between solid members. A member must be consumed or closed
// before calling Next. The session is intentionally single-threaded; callers
// that need concurrent reads should materialize completed members into their
// own cache.
type SolidSession struct {
	ctx          stdcontext.Context
	err          error
	archive      *ReadCloser
	current      *solidMemberReader
	ordinal      int
	startOrdinal int
	pending      *fileBlockList
	group        int
	closed       bool
}

// SolidMember is one sequentially decoded member. Group identifies the
// contiguous solid group and is stable only within this session. A new group
// starts at the first member and whenever the archive marks a member as not
// continuing solid state.
type SolidMember struct {
	Header  *FileHeader
	Ordinal int
	Group   int
	reader  *solidMemberReader
}

func (m *SolidMember) Read(p []byte) (int, error) {
	if m == nil {
		return 0, io.ErrClosedPipe
	}
	if m.reader == nil {
		return 0, io.ErrClosedPipe
	}
	return m.reader.Read(p)
}

func (m *SolidMember) WriteTo(w io.Writer) (int64, error) {
	if m == nil {
		return 0, io.ErrClosedPipe
	}
	if m.reader == nil {
		return 0, io.ErrClosedPipe
	}
	return m.reader.WriteTo(w)
}

func (m *SolidMember) Close() error {
	if m == nil || m.reader == nil {
		return nil
	}
	return m.reader.Close()
}

// OpenSolidSession opens a multi-volume-aware sequential decoder. It uses the
// same FileSystem, Password, and dictionary options as OpenReader.
func OpenSolidSession(name string, opts ...Option) (*SolidSession, error) {
	return OpenSolidSessionContext(stdcontext.Background(), name, opts...)
}

// OpenSolidSessionContext checks cancellation between decoded reads. Custom
// FileSystem implementations must cancel blocking I/O themselves. Methods,
// including Close, must be serialized by the caller.
func OpenSolidSessionContext(ctx stdcontext.Context, name string, opts ...Option) (*SolidSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Solid sessions are integrity-sensitive: force per-member checksum
	// verification even when a caller supplies SkipCheck for other readers.
	// Keep the option accepted for API compatibility, but do not let it weaken
	// the session's verification guarantee.
	verifiedOpts := append([]Option(nil), opts...)
	verifiedOpts = append(verifiedOpts, func(o *options) { o.skipCheck = false })
	archive, err := OpenReader(name, verifiedOpts...)
	if err != nil {
		return nil, err
	}
	return &SolidSession{archive: archive, ctx: ctx}, nil
}

// OpenSolidSessionFrom starts at a known, zero-based archive member ordinal.
// It skips preceding packed members without constructing their decoders or
// verifying their data checksums. It otherwise has the same integrity and
// dictionary policy as OpenSolidSession.
func OpenSolidSessionFrom(name string, startOrdinal int, opts ...Option) (*SolidSession, error) {
	return OpenSolidSessionFromContext(stdcontext.Background(), name, startOrdinal, opts...)
}

// OpenSolidSessionFromContext reparses headers, including intervening volumes,
// with the current password. The selected member must be a nonempty compressed
// regular file with known size and Solid false, so its decoder resets the group
// dictionary. A solid continuation returns ErrSolidOpen; other unsupported
// targets and negative ordinals return fs.ErrInvalid. An ordinal beyond the
// archive returns io.EOF. Header and volume errors are returned unchanged.
//
// Ordinal remains the absolute archive ordinal; Group starts at zero within
// this session. Packed skips may read data when the FileSystem cannot seek.
// Cancellation is checked between headers/volume blocks and decoded reads;
// custom FileSystem implementations must cancel blocking I/O themselves.
func OpenSolidSessionFromContext(ctx stdcontext.Context, name string, startOrdinal int, opts ...Option) (*SolidSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if startOrdinal < 0 {
		return nil, fs.ErrInvalid
	}
	opt := getOptions(opts)
	opt.skipCheck = false
	v, err := openVolume(name, opt)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*SolidSession, error) {
		_ = v.Close()
		return nil, err
	}
	pr := newPackedFileReader(v, opt)
	for ordinal := 0; ; ordinal++ {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		blocks, err := pr.nextFile()
		if err != nil {
			return fail(err)
		}
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		if ordinal == startOrdinal {
			h := blocks.firstBlock()
			if h.Solid {
				return fail(ErrSolidOpen)
			}
			if !h.Mode().IsRegular() || h.UnKnownSize || h.UnPackedSize <= 0 || h.decVer == 0 {
				return fail(fs.ErrInvalid)
			}
			archive := &ReadCloser{Reader: Reader{f: pr}, cl: v, vm: v.vm}
			return &SolidSession{archive: archive, ctx: ctx, ordinal: startOrdinal,
				startOrdinal: startOrdinal, pending: blocks}, nil
		}
		// Complete split members at the packed-header layer. nextFile would
		// perform this loop too, but checking each block bounds cancellation
		// latency without ever initializing a preceding member decoder.
		for !pr.currFile().last {
			if err := ctx.Err(); err != nil {
				return fail(err)
			}
			if err := pr.nextBlock(); err != nil {
				return fail(err)
			}
		}
	}
}

// Next advances to the next member. If the previous member was only partly
// read, Next drains it first so the underlying solid decoder remains valid.
func (s *SolidSession) Next() (*SolidMember, error) {
	if s == nil || s.closed {
		if s != nil && s.err != nil {
			return nil, s.err
		}
		return nil, io.ErrClosedPipe
	}
	if s.err != nil {
		return nil, s.err
	}
	if err := s.check(); err != nil {
		return nil, err
	}
	if s.current != nil {
		if err := s.current.Close(); err != nil {
			return nil, err
		}
		s.current = nil
	}
	var header *FileHeader
	var err error
	if s.pending != nil {
		blocks := s.pending
		s.pending = nil
		s.archive.f, err = s.archive.f.newArchiveFile(blocks)
		header = &blocks.firstBlock().FileHeader
	} else {
		header, err = s.archive.Next()
	}
	if err != nil {
		if err == io.EOF {
			s.err = io.EOF
			_ = s.Close()
		} else {
			s.fail(err)
		}
		return nil, err
	}
	if s.ordinal > s.startOrdinal && !header.Solid {
		s.group++
	}
	member := &SolidMember{
		Header:  func() *FileHeader { h := *header; return &h }(),
		Ordinal: s.ordinal,
		Group:   s.group,
	}
	member.reader = &solidMemberReader{session: s}
	s.current = member.reader
	s.ordinal++
	return member, nil
}

// Close releases the archive and any open volume files. It is safe to call
// more than once.
func (s *SolidSession) Close() error {
	if s == nil || s.closed {
		return nil
	}
	s.closed = true
	if s.archive == nil {
		return nil
	}
	err := s.archive.Close()
	s.archive = nil
	s.current = nil
	s.pending = nil
	return err
}

// Read reads the current member. It returns io.ErrClosedPipe when Next has
// not produced a member or after the session has been closed.
func (s *SolidSession) Read(p []byte) (int, error) {
	if s == nil {
		return 0, io.ErrClosedPipe
	}
	if s.err != nil {
		return 0, s.err
	}
	if s.closed || s.current == nil || s.current.closed {
		return 0, io.ErrClosedPipe
	}
	if s.current.verified {
		return 0, io.EOF
	}
	if err := s.check(); err != nil {
		return 0, err
	}
	n, err := s.archive.Read(p)
	if err == io.EOF {
		s.current.verified = true
	}
	if err != nil && err != io.EOF {
		s.fail(err)
	}
	return n, err
}

// WriteTo copies through bounded reads so cancellation is checked during
// decoding. Only a successful complete copy is eligible for cache publication.
func (s *SolidSession) WriteTo(w io.Writer) (int64, error) {
	if s == nil {
		return 0, io.ErrClosedPipe
	}
	if s.err != nil {
		return 0, s.err
	}
	if s.closed || s.current == nil || s.current.closed {
		return 0, io.ErrClosedPipe
	}
	n, err := io.CopyBuffer(w, struct{ io.Reader }{s}, make([]byte, 64<<10))
	if err != nil {
		s.fail(err)
	}
	return n, err
}

type solidMemberReader struct {
	session  *SolidSession
	closed   bool
	verified bool
}

func (r *solidMemberReader) Read(p []byte) (int, error) {
	if r == nil || r.closed || r.session == nil || r.session.current != r {
		return 0, io.ErrClosedPipe
	}
	return r.session.Read(p)
}

func (r *solidMemberReader) WriteTo(w io.Writer) (int64, error) {
	if r == nil || r.closed || r.session == nil || r.session.current != r {
		return 0, io.ErrClosedPipe
	}
	return r.session.WriteTo(w)
}

func (r *solidMemberReader) Close() error {
	if r == nil || r.closed {
		return nil
	}
	if r.session == nil {
		r.closed = true
		return nil
	}
	if r.closed {
		return nil
	}
	if r.session.err != nil {
		r.closed = true
		return r.session.err
	}
	if r.session.closed || r.session.current != r {
		r.closed = true
		return nil
	}
	var err error
	if !r.verified {
		_, err = r.session.WriteTo(io.Discard)
	}
	r.closed = true
	if r.session.current == r {
		r.session.current = nil
	}
	return err
}

// Volumes returns a snapshot of volume names opened so far, not a content
// version identity. The caller must combine it with its source identities.
// It returns nil after Close or a terminal EOF/error; take the snapshot before
// that transition if the caller needs to retain the opened-volume list.
func (s *SolidSession) Volumes() []string {
	if s == nil || s.archive == nil {
		return nil
	}
	return append([]string(nil), s.archive.Volumes()...)
}
func (s *SolidSession) check() error {
	if s.err != nil {
		return s.err
	}
	if s.closed {
		return io.ErrClosedPipe
	}
	if err := s.ctx.Err(); err != nil {
		s.fail(err)
		return err
	}
	return nil
}
func (s *SolidSession) fail(err error) {
	if s.err == nil {
		s.err = err
	}
	_ = s.Close()
}
