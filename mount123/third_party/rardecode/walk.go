// Copyright 2026 mount123 contributors. All rights reserved.
// Distributed under the BSD-style license in LICENSE.
package rardecode

import "io"

// MemberLocator contains only an immutable header offset, never reader or key state.
type MemberLocator struct {
	offset int64
	solid  bool
	valid  bool
}

// LocatorRecord is the password-free, serializable part of a member location.
type LocatorRecord struct {
	Offset int64 `json:"offset"`
	Solid  bool  `json:"solid"`
	Valid  bool  `json:"valid"`
}

// ExportLocator returns immutable positioning metadata without reader or
// password state.
func ExportLocator(l MemberLocator) LocatorRecord {
	return LocatorRecord{Offset: l.offset, Solid: l.solid, Valid: l.valid}
}

// ImportLocator validates persisted offsets against the current archive size.
func ImportLocator(record LocatorRecord, archiveSize int64) (MemberLocator, error) {
	if archiveSize <= 0 || record.Offset < 0 || record.Offset >= archiveSize {
		return MemberLocator{}, ErrInvalidHeaderOff
	}
	if record.Valid && record.Offset < 7 {
		return MemberLocator{}, ErrInvalidHeaderOff
	}
	return MemberLocator{offset: record.Offset, solid: record.Solid, valid: record.Valid}, nil
}

func (l MemberLocator) Direct() bool { return l.valid && !l.solid }

// Walk visits headers without decoding member data or retaining all headers.
func Walk(name string, visit func(*FileHeader) error, opts ...Option) error {
	return WalkMembers(name, func(h *FileHeader, _ MemberLocator) error { return visit(h) }, opts...)
}

// WalkMembers also supplies a location usable to reopen independent members.
func WalkMembers(name string, visit func(*FileHeader, MemberLocator) error, opts ...Option) error {
	return walkMembers(name, 0, func(h *FileHeader, locator MemberLocator, _ int64) error {
		return visit(h, locator)
	}, opts...)
}

// WalkMembersFrom resumes a metadata walk at a previously returned resume
// offset. The offset must be the end of a complete file's packed blocks as
// returned to the callback; it is validated by parsing the next header with
// the freshly initialized archive state. A zero offset starts at the first
// file. This is intentionally limited to a single archive stream: callers
// using multi-volume archives must retain the volume-aware source and use a
// fresh walk.
func WalkMembersFrom(name string, offset int64, visit func(*FileHeader, MemberLocator, int64) error, opts ...Option) error {
	if offset < 0 {
		return ErrInvalidHeaderOff
	}
	return walkMembers(name, offset, visit, opts...)
}

func walkMembers(name string, offset int64, visit func(*FileHeader, MemberLocator, int64) error, opts ...Option) error {
	options := getOptions(opts)
	v, err := openVolume(name, options)
	if err != nil {
		return err
	}
	defer v.Close()
	if offset > 0 {
		// openVolume has parsed the archive header, which initializes version,
		// encryption and volume state. Seeking after that header preserves the
		// state required to parse subsequent file headers without replaying the
		// earlier file entries.
		if offset < v.br.off || !v.br.canSeek() {
			return ErrInvalidHeaderOff
		}
		if err := v.br.seek(offset); err != nil {
			return err
		}
		v.n = 0
	}
	pr := newPackedFileReader(v, options)
	for {
		if offset == 0 {
			offset = v.br.off + v.n
		}
		blocks, err := pr.nextFile()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		first := blocks.firstBlock()
		h := first.FileHeader
		locator := MemberLocator{offset: offset, solid: h.Solid, valid: first.first && first.last && first.volnum == 0}
		// nextFile returns the first block without reading the rest of a
		// split member. Advance through continuation blocks now (discarding
		// their packed bytes, never decoding them) so the token is the exact
		// boundary after the complete member.
		for !pr.currFile().last {
			if err := pr.nextBlock(); err != nil {
				return err
			}
		}
		last := pr.currFile()
		next := last.dataOff + last.PackedSize
		if err := visit(&h, locator, next); err != nil {
			return err
		}
		offset = next
	}
}

// OpenMember reparses the archive prefix and target header using the current
// password, then decodes only that member. Solid members need sequential access.
func OpenMember(name string, locator MemberLocator, opts ...Option) (*FileHeader, io.ReadCloser, error) {
	if !locator.Direct() {
		return nil, nil, ErrSolidOpen
	}
	options := getOptions(opts)
	v, err := openVolume(name, options)
	if err != nil {
		return nil, nil, err
	}
	fail := func(err error) (*FileHeader, io.ReadCloser, error) { v.Close(); return nil, nil, err }
	if locator.offset < v.br.off {
		return fail(ErrInvalidHeaderOff)
	}
	if err := v.br.seek(locator.offset); err != nil {
		return fail(err)
	}
	v.n = 0
	h, err := v.nextBlock()
	if err != nil {
		return fail(err)
	}
	if h.Solid || !h.first || !h.last {
		return fail(ErrSolidOpen)
	}
	pr := newPackedFileReader(v, options)
	f, err := pr.newArchiveFile(newFileBlockList(h))
	if err != nil {
		return fail(err)
	}
	return &h.FileHeader, &fileCloser{archiveFile: f, Closer: v}, nil
}
