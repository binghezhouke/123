package rardecode

import (
	"bytes"
	"errors"
	"hash"
	"io"
	"io/fs"
	"sync"
)

// ErrUnsupportedStoredRange means the current member requires the ordinary
// decoder. It is distinct from a malformed header or a source I/O failure.
var ErrUnsupportedStoredRange = errors.New("rardecode: member does not support stored range reads")

// StoredMemberRange is an owned plan for one independent, unencrypted Store
// member. It retains no archive source, password, decoder or request context.
// Every read supplies the original archive's ReaderAt, in absolute coordinates.
type StoredMemberRange struct {
	offset, size int64
	newHash      func() hash.Hash
	want         []byte
	mu           sync.Mutex
	closed       bool
	inFlight     int
	generation   uint64
	tracking     bool
	position     int64
	checksum     hash.Hash
}

// OpenStoredMemberRange reparses the archive prefix and target header. Only a
// complete, regular Store member in a non-solid, single-volume archive is safe
// to map; all other supported archive layouts retain their ordinary decoder.
// The constructor closes its filesystem handle before returning the plan.
func OpenStoredMemberRange(name string, locator MemberLocator, opts ...Option) (*FileHeader, *StoredMemberRange, error) {
	if !locator.valid || locator.solid {
		return nil, nil, ErrUnsupportedStoredRange
	}
	options := getOptions(opts)
	v, err := openVolume(name, options)
	if err != nil {
		return nil, nil, err
	}
	defer v.Close()
	info, err := v.f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if info.Size() < 0 || locator.offset < v.br.off || locator.offset >= info.Size() {
		return nil, nil, ErrInvalidHeaderOff
	}
	if err := v.br.seek(locator.offset); err != nil {
		return nil, nil, err
	}
	v.n = 0
	h, err := v.nextBlock()
	if err != nil {
		return nil, nil, err
	}
	if h.decVer != 0 || h.UnKnownSize || h.Encrypted || h.HeaderEncrypted ||
		h.Solid || h.arcSolid || h.arcMulti || !h.first || !h.last || h.volnum != 0 || v.num != 0 ||
		h.PackedSize != h.UnPackedSize || h.UnPackedSize < 0 || !h.Mode().IsRegular() ||
		len(h.hashKey) != 0 || h.unsupportedHash {
		return nil, nil, ErrUnsupportedStoredRange
	}
	if h.dataOff < 0 || h.dataOff > info.Size() || h.PackedSize > info.Size()-h.dataOff {
		return nil, nil, ErrShortFile
	}
	plan := &StoredMemberRange{offset: h.dataOff, size: h.UnPackedSize}
	if !options.skipCheck {
		plan.newHash, plan.want = h.hash, append([]byte(nil), h.sum...)
	}
	return &h.FileHeader, plan, nil
}

// ReadAtWithReader reads only the requested member range. Full single reads and
// contiguous non-overlapping reads from zero retain the native file checksum.
// Sparse or concurrent partial reads do not claim complete-member validation.
func (r *StoredMemberRange) ReadAtWithReader(body io.ReaderAt, p []byte, off int64) (int, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return 0, fs.ErrClosed
	}
	if body == nil || off < 0 {
		r.mu.Unlock()
		return 0, fs.ErrInvalid
	}
	if len(p) == 0 {
		r.mu.Unlock()
		return 0, nil
	}
	if off >= r.size {
		r.mu.Unlock()
		if off == 0 && r.size == 0 && r.newHash != nil && len(r.want) != 0 {
			if !bytes.Equal(r.newHash().Sum(nil), r.want) {
				return 0, ErrBadFileChecksum
			}
		}
		return 0, io.EOF
	}
	length := min(int64(len(p)), r.size-off)
	truncated := length < int64(len(p))
	p = p[:length]
	if r.inFlight != 0 {
		r.generation++
		r.tracking, r.checksum = false, nil
	} else if off == 0 {
		r.generation++
		r.position = 0
		r.tracking = r.newHash != nil && len(r.want) != 0
		if r.tracking {
			r.checksum = r.newHash()
		}
	} else if !r.tracking || off != r.position {
		r.generation++
		r.tracking, r.checksum = false, nil
	}
	generation, checksum := r.generation, r.checksum
	r.inFlight++
	r.mu.Unlock()

	n, sourceErr := body.ReadAt(p, r.offset+off)
	valid := n >= 0 && n <= len(p)
	if !valid {
		n, sourceErr = 0, ErrShortFile
	}
	if n < len(p) && (sourceErr == nil || sourceErr == io.EOF) {
		sourceErr = ErrShortFile
	}
	validSource := sourceErr == nil || sourceErr == io.EOF
	full := off == 0 && int64(n) == r.size
	var checksumErr error
	if validSource && full && r.newHash != nil && len(r.want) != 0 {
		independent := r.newHash()
		_, _ = independent.Write(p[:n])
		if !bytes.Equal(independent.Sum(nil), r.want) {
			checksumErr = ErrBadFileChecksum
		}
	} else if validSource && checksum != nil {
		// This operation exclusively borrows the current hash. A concurrent
		// operation invalidates the generation and never touches this instance.
		// Hashing and all source I/O stay outside the lifecycle mutex.
		_, _ = checksum.Write(p[:n])
	}
	r.mu.Lock()
	r.inFlight--
	if r.generation == generation && r.tracking {
		if !validSource || full {
			r.tracking, r.checksum = false, nil
		} else {
			r.position += int64(n)
			if r.position == r.size {
				if !bytes.Equal(checksum.Sum(nil), r.want) {
					checksumErr = ErrBadFileChecksum
				}
				r.tracking, r.checksum = false, nil
			}
		}
	}
	r.mu.Unlock()
	if !validSource {
		return n, sourceErr
	}
	if checksumErr != nil {
		return n, checksumErr
	}
	if truncated && sourceErr == nil {
		return n, io.EOF
	}
	return n, sourceErr
}

// Close rejects new reads without waiting for source I/O already in progress.
// An active complete read can still finish its independent checksum check.
func (r *StoredMemberRange) Close() error {
	r.mu.Lock()
	r.closed = true
	r.generation++
	r.tracking, r.checksum = false, nil
	r.mu.Unlock()
	return nil
}
