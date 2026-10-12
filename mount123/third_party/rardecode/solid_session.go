package rardecode

import (
	stdcontext "context"
	"io"
)

// SolidSession reads RAR members in archive order while retaining the
// decoder state between solid members. A member must be consumed or closed
// before calling Next. The session is intentionally single-threaded; callers
// that need concurrent reads should materialize completed members into their
// own cache.
type SolidSession struct {
	ctx     stdcontext.Context
	err     error
	archive *ReadCloser
	current *solidMemberReader
	ordinal int
	group   int
	closed  bool
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
	return m.reader.Read(p)
}

func (m *SolidMember) WriteTo(w io.Writer) (int64, error) {
	if m == nil {
		return 0, io.ErrClosedPipe
	}
	return m.reader.WriteTo(w)
}

func (m *SolidMember) Close() error {
	if m == nil {
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
	archive, err := OpenReader(name, opts...)
	if err != nil {
		return nil, err
	}
	return &SolidSession{archive: archive, ctx: ctx}, nil
}

// Next advances to the next member. If the previous member was only partly
// read, Next drains it first so the underlying solid decoder remains valid.
func (s *SolidSession) Next() (*SolidMember, error) {
	if s == nil || s.closed {
		return nil, io.ErrClosedPipe
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
	header, err := s.archive.Next()
	if err != nil {
		s.fail(err)
		return nil, err
	}
	if s.ordinal > 0 && !header.Solid {
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
	return err
}

// Read reads the current member. It returns io.ErrClosedPipe when Next has
// not produced a member or after the session has been closed.
func (s *SolidSession) Read(p []byte) (int, error) {
	if s == nil || s.closed || s.current == nil {
		return 0, io.ErrClosedPipe
	}
	if err := s.check(); err != nil {
		return 0, err
	}
	n, err := s.archive.Read(p)
	if err != nil && err != io.EOF {
		s.fail(err)
	}
	return n, err
}

// WriteTo copies through bounded reads so cancellation is checked during
// decoding. Only a successful complete copy is eligible for cache publication.
func (s *SolidSession) WriteTo(w io.Writer) (int64, error) {
	if s == nil || s.closed || s.current == nil {
		return 0, io.ErrClosedPipe
	}
	n, err := io.CopyBuffer(w, struct{ io.Reader }{s}, make([]byte, 64<<10))
	if err != nil {
		s.fail(err)
	}
	return n, err
}

type solidMemberReader struct {
	session *SolidSession
	closed  bool
}

func (r *solidMemberReader) Read(p []byte) (int, error) {
	if r == nil || r.closed || r.session == nil {
		return 0, io.ErrClosedPipe
	}
	return r.session.Read(p)
}

func (r *solidMemberReader) WriteTo(w io.Writer) (int64, error) {
	if r == nil || r.closed || r.session == nil {
		return 0, io.ErrClosedPipe
	}
	return r.session.WriteTo(w)
}

func (r *solidMemberReader) Close() error {
	if r == nil || r.closed {
		return nil
	}
	r.closed = true
	if r.session == nil || r.session.closed {
		return nil
	}
	_, err := r.session.WriteTo(io.Discard)
	return err
}

// Volumes returns a snapshot of volume names opened so far, not a content
// version identity. The caller must combine it with its source identities.
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
