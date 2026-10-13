package mountfs

import (
	"context"
	"hash/crc32"
	"io"
	"sync"
	"syscall"
)

// Direct members keep random reads direct. CRC verification is possible only
// for a complete read or a contiguous, nonoverlapping sequence starting at 0.
func newArchiveCheckedInput(input archiveReaderAt, size int64, crc uint32, checkCRC bool, integrityErr error) archiveReaderAt {
	if !checkCRC {
		return input
	}
	if integrityErr == nil {
		integrityErr = errArchiveIntegrity
	}
	return &archiveCheckedInput{input: input, size: size, want: crc, integrityErr: integrityErr}
}

type archiveCheckedInput struct {
	input        archiveReaderAt
	size         int64
	want         uint32
	integrityErr error
	mu           sync.Mutex
	generation   uint64
	inFlight     int
	tracking     bool
	position     int64
	checksum     uint32
}

func (r *archiveCheckedInput) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if r.input == nil || r.size < 0 || off < 0 {
		return 0, syscall.EINVAL
	}
	if len(p) == 0 {
		return 0, nil
	}
	if off >= r.size {
		if off == 0 && r.size == 0 && r.want != 0 {
			return 0, r.integrityErr
		}
		return 0, io.EOF
	}
	want := min(int64(len(p)), r.size-off)
	truncated := want < int64(len(p))
	p = p[:want]
	r.mu.Lock()
	if r.inFlight != 0 {
		// Completion order need not match offset order. Disable this pass for
		// every overlapping operation, rather than combining unrelated bytes.
		r.generation++
		r.tracking = false
	} else if off == 0 {
		r.generation++
		r.tracking, r.position, r.checksum = true, 0, 0
	} else if !r.tracking || off != r.position {
		r.generation++
		r.tracking = false
	}
	generation, tracking, previous := r.generation, r.tracking, r.checksum
	r.inFlight++
	r.mu.Unlock()

	n, err := r.input.ReadAtContext(ctx, p, off)
	valid := err == nil || err == io.EOF
	full := off == 0 && int64(n) == r.size
	var checksum uint32
	if valid && (tracking || full) {
		if full {
			// A single complete response can be checked independently even if
			// another request invalidated the shared sequential tracker.
			previous = 0
		}
		checksum = crc32.Update(previous, crc32.IEEETable, p[:n])
	}
	r.mu.Lock()
	r.inFlight--
	complete := false
	if tracking && r.tracking && r.generation == generation {
		if valid {
			r.position += int64(n)
			r.checksum = checksum
			complete = r.position == r.size
			if (err == io.EOF && !complete) || n != len(p) {
				r.tracking = false
			}
		} else {
			r.tracking = false
		}
	}
	r.mu.Unlock()
	if valid && (full || complete) && checksum != r.want {
		return n, r.integrityErr
	}
	if truncated && err == nil {
		err = io.EOF
	}
	return n, err
}
