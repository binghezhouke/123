package mountfs

import (
	"archive/zip"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"math"
	"syscall"

	kflate "github.com/klauspost/compress/flate"
)

// zipCheckpointCRC continues the public IEEE CRC from a prefix sum. The
// dictionary seeds inflate history, but must never be hashed as new output.
type zipCheckpointCRC struct{ value uint32 }

func (h *zipCheckpointCRC) Write(p []byte) (int, error) {
	h.value = crc32.Update(h.value, crc32.IEEETable, p)
	return len(p), nil
}
func (h *zipCheckpointCRC) Sum32() uint32 { return h.value }
func (h *zipCheckpointCRC) Sum(p []byte) []byte {
	return binary.BigEndian.AppendUint32(p, h.value)
}
func (h *zipCheckpointCRC) Reset()         { h.value = 0 }
func (h *zipCheckpointCRC) Size() int      { return 4 }
func (h *zipCheckpointCRC) BlockSize() int { return 1 }

// Capture source failures separately from codec errors. Bufio or inflate can
// report EOF before surfacing an error supplied with the final input bytes.
type zipCheckpointSource struct {
	ctx    context.Context
	source archiveReaderAt
	err    error
}

func (r *zipCheckpointSource) ReadAt(p []byte, off int64) (int, error) {
	n, err := r.source.ReadAtContext(r.ctx, p, off)
	if err != nil && err != io.EOF && r.err == nil {
		r.err = err
	}
	return n, err
}

type zipCheckpointStream struct {
	ctx      context.Context
	source   *zipCheckpointSource
	checked  *zipMemberChecksum
	decoder  io.ReadCloser
	hooks    zipDeflateCheckpointHooks
	size     int64
	inputLen int64
	ready    *zipDeflateCheckpoint
	pending  *zipDeflateCheckpoint
	valid    bool
	invalid  bool
	closed   bool
}

func openZIPDeflateCheckpointStream(ctx context.Context, source archiveReaderAt, m *member, dataOffset int64, resume *zipDeflateCheckpoint, hooks zipDeflateCheckpointHooks) (io.ReadCloser, error) {
	fail := func(err error) (io.ReadCloser, error) {
		if hooks.Close != nil {
			hooks.Close()
		}
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if source == nil || m == nil || dataOffset < 0 {
		return fail(syscall.EINVAL)
	}
	if m.method != zip.Deflate || m.encrypted || m.flags&1 != 0 || m.aes != nil {
		return fail(syscall.EOPNOTSUPP)
	}
	if m.size > math.MaxInt64 || m.compressed > math.MaxInt64 || dataOffset > math.MaxInt64-int64(m.compressed) {
		return fail(syscall.EFBIG)
	}
	r := &zipCheckpointStream{ctx: ctx, size: int64(m.size), inputLen: int64(m.compressed), hooks: hooks}
	r.source = &zipCheckpointSource{ctx: ctx, source: source}
	hash := &zipCheckpointCRC{}
	inputOffset, outputOffset := int64(0), int64(0)
	options := []kflate.ReaderOpt{kflate.WithPartialBlock(), kflate.WithEobCallback(r.onCheckpoint)}
	if resume != nil {
		if resume.OutputOffset < 0 || resume.OutputOffset >= r.size || resume.InputOffset < 0 || resume.InputOffset >= r.inputLen || resume.BitOffset > 7 || int64(len(resume.Window)) != min(resume.OutputOffset, 32768) {
			return fail(syscall.EINVAL)
		}
		inputOffset, outputOffset, hash.value = resume.InputOffset, resume.OutputOffset, resume.CRC
		options = append(options, kflate.WithResumeFrom(kflate.InflateCheckpoint{
			UncompressedOffset: outputOffset, CompressedOffset: inputOffset,
			BitOffset: resume.BitOffset, Window: resume.Window,
		}))
	}
	r.checked = &zipMemberChecksum{ctx: ctx, hash: hash, left: r.size - outputOffset, want: m.crc, checkCRC: true}
	r.decoder = kflate.NewReaderOpts(io.NewSectionReader(r.source, dataOffset+inputOffset, r.inputLen-inputOffset), options...)
	r.checked.reader = r.decoder
	if r.source.err != nil {
		_ = r.decoder.Close()
		return fail(r.source.err)
	}
	return &archiveMemberStream{ctx: ctx, reader: r, closer: r}, nil
}

func (r *zipCheckpointStream) onCheckpoint(cp kflate.InflateCheckpoint) {
	// The upstream callback also runs for final blocks. Such a point cannot
	// resume at another block and has no useful member output after it.
	if cp.Final || cp.UncompressedOffset < 0 || cp.UncompressedOffset >= r.size || cp.CompressedOffset < 0 || cp.CompressedOffset >= r.inputLen || cp.BitOffset > 7 || r.hooks.Save == nil || r.hooks.Want == nil {
		return
	}
	delivered := r.size - r.checked.left
	if cp.UncompressedOffset < delivered || (cp.UncompressedOffset == delivered && r.ready != nil) || (cp.UncompressedOffset > delivered && r.pending != nil) || !r.hooks.Want(cp.UncompressedOffset) {
		return
	}
	point := &zipDeflateCheckpoint{OutputOffset: cp.UncompressedOffset, InputOffset: cp.CompressedOffset, BitOffset: cp.BitOffset, Window: append([]byte(nil), cp.Window...)}
	if point.OutputOffset == delivered {
		// Empty blocks, or a window flushed before EOB was consumed, can
		// generate a point for bytes already delivered by the preceding Read.
		point.CRC = r.checked.hash.Sum32()
		r.ready = point
	} else {
		// Block output may still be in inflate's pending buffer. Keep at most
		// one copied window, and pair it only when the checksum catches up.
		r.pending = point
	}
}

func (r *zipCheckpointStream) Read(p []byte) (int, error) {
	r.ready = nil
	n, err := r.checked.Read(p)
	if ctxErr := r.ctx.Err(); ctxErr != nil {
		err = ctxErr
	} else if r.source.err != nil {
		err = r.source.err
	}
	if err != nil && err != io.EOF {
		r.ready, r.pending = nil, nil
		var corrupt kflate.CorruptInputError
		if r.ctx.Err() == nil && r.source.err == nil && (errors.Is(err, zip.ErrChecksum) || errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &corrupt)) && !r.invalid {
			r.invalid = true
			if r.hooks.Invalid != nil {
				r.hooks.Invalid()
			}
		}
		return n, err
	}
	if r.ready != nil {
		r.hooks.Save(*r.ready)
		r.ready = nil
	}
	if r.pending != nil {
		delivered := r.size - r.checked.left
		if r.pending.OutputOffset == delivered {
			r.pending.CRC = r.checked.hash.Sum32()
			r.hooks.Save(*r.pending)
			r.pending = nil
		} else if r.pending.OutputOffset < delivered {
			// Never attach a later CRC to an earlier output boundary.
			r.pending = nil
		}
	}
	if ctxErr := r.ctx.Err(); ctxErr != nil {
		r.pending = nil
		return n, ctxErr
	}
	if err == io.EOF && !r.valid {
		r.valid = true
		if r.hooks.Valid != nil {
			r.hooks.Valid()
		}
	}
	return n, err
}

func (r *zipCheckpointStream) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	r.ready, r.pending = nil, nil
	err := r.decoder.Close()
	if r.hooks.Close != nil {
		r.hooks.Close()
	}
	return err
}
