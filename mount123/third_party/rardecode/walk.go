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
	options := getOptions(opts)
	v, err := openVolume(name, options)
	if err != nil {
		return err
	}
	defer v.Close()
	pr := newPackedFileReader(v, options)
	for {
		offset := v.br.off + v.n
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
		if err := visit(&h, locator); err != nil {
			return err
		}
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
