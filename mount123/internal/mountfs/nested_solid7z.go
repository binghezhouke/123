package mountfs

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"syscall"

	"github.com/bodgit/sevenzip"
)

type nestedSolid7zPlan struct {
	key         string
	fingerprint [32]byte
	charge      int64
	parent      *nestedArchive
	member      member
	password    protectedPassword
}

func solidLayoutFingerprint(layout *sevenzip.StreamLayout) [32]byte {
	data, _ := json.Marshal(layout)
	return sha256.Sum256(data)
}

// Account for the decoder's actual dictionary and worst-case literal models,
// including a model transition, not the group's uncompressed size. Retained
// ancestors currently qualify only when all their compressed inputs are known
// clear ZIP coders. Other ancestry remains readable on the existing path.
func nestedSolid7zCharge(n *nestedArchive, layout *sevenzip.StreamLayout, decoder *sevenzip.LZMA2DecoderInfo) (int64, bool) {
	if n == nil || n.indexBytes < 0 || n.index == nil || n.format != ".7z" {
		return 0, false
	}
	charge := uint64(n.indexBytes)
	add := func(value uint64) bool {
		if value > math.MaxInt64-charge {
			return false
		}
		charge += value
		return true
	}
	for _, value := range []uint64{decoder.DictionarySize, 1, decoder.MaxLiteralModelBytes, decoder.MaxLiteralModelBytes,
		decoder.PackedBufferSize, 256 << 10, 64 << 10, uint64(len(layout.Members)) * 64, 4096} {
		if !add(value) {
			return 0, false
		}
	}
	for view := n; view != nil; view = view.parent {
		if view.parent == nil {
			inputs, ok := nestedZIPRawInputs(viewInput(view))
			if !ok || inputs <= 0 || uint64(inputs) > math.MaxInt64/(1<<20) || !add(uint64(inputs)*(1<<20)) {
				return 0, false
			}
			continue
		}
		if view.parent.format != ".zip" || len(view.lazyInputs) == 0 {
			return 0, false
		}
		for _, input := range view.lazyInputs {
			part, ok := input.(*nestedVolumeInput)
			if !ok || part.parent != view.parent || part.member == nil {
				return 0, false
			}
			m := part.member
			if m.encrypted || m.flags&1 != 0 || m.aes != nil || (m.method != zip.Store && m.method != zip.Deflate) || !add(uint64(nestedDeflateResidentUnit)) {
				return 0, false
			}
		}
	}
	return int64(charge), true
}

func (t *Tree) prepareNestedSolid7z(ctx context.Context, n *nestedArchive, m *member, password []byte) (*nestedSolid7zPlan, error) {
	if t.nestedSolid7zPool == nil || n.format != ".7z" || !m.sevenStream.valid(m.size) {
		return nil, nil
	}
	source := &nestedSourceReader{ReaderAt: archiveRequestReader{ctx: ctx, input: viewInput(n)}}
	zr, err := sevenzip.NewReaderWithPassword(source, n.size, string(password))
	if err != nil {
		if e := source.sourceError(); e != nil {
			return nil, e
		}
		return nil, archiveReadError(ctx, err, password)
	}
	layout, err := zr.StreamLayout(m.sevenStream.Stream)
	if err != nil {
		return nil, err
	}
	decoder, err := layout.PlainLZMA2DecoderInfo()
	if errors.Is(err, sevenzip.ErrUnsupportedDecoderInfo) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if m.ordinal < 0 || m.ordinal >= len(zr.File) {
		return nil, syscall.ESTALE
	}
	f := zr.File[m.ordinal]
	offset, ok := f.StreamOffset()
	if !ok || f.Name != m.name || f.UncompressedSize != m.size || f.CRC32 != m.crc || f.Stream != layout.Index || offset != m.sevenStream.Offset || layout.UncompressedSize != uint64(m.sevenStream.Size) {
		return nil, syscall.ESTALE
	}
	charge, ok := nestedSolid7zCharge(n, layout, decoder)
	if !ok || charge > t.nestedSolid7zPool.maxBytes {
		return nil, nil
	}
	fingerprint := solidLayoutFingerprint(layout)
	key := t.cache.StableDigest("nested-solid7z-v1", n.identity+"\x00"+t.passwordTag(n.archive, password)+fmt.Sprintf(":%x", fingerprint))
	protected, err := t.encryptPassword(key, password)
	if err != nil {
		return nil, err
	}
	// Keep only scalar member identity, not a transient member/source reader.
	bound := member{name: m.name, size: m.size, crc: m.crc, ordinal: m.ordinal, sevenStream: m.sevenStream}
	return &nestedSolid7zPlan{key: key, fingerprint: fingerprint, charge: charge, parent: n, member: bound, password: protected}, nil
}

func (t *Tree) openNestedSolid7zState(ctx context.Context, plan *nestedSolid7zPlan) (*nestedSolid7zState, error) {
	pw, err := t.decryptPassword(plan.key, plan.password)
	if err != nil {
		return nil, err
	}
	defer clear(pw)
	op := &nestedReadContext{}
	op.bind(ctx)
	source := &nestedSourceReader{ReaderAt: archiveRequestReader{ctx: op, input: viewInput(plan.parent)}}
	zr, err := sevenzip.NewReaderWithPassword(source, plan.parent.size, string(pw))
	if err != nil {
		if e := source.sourceError(); e != nil {
			return nil, e
		}
		return nil, archiveReadError(ctx, err, pw)
	}
	layout, err := zr.StreamLayout(plan.member.sevenStream.Stream)
	if err != nil {
		return nil, err
	}
	if solidLayoutFingerprint(layout) != plan.fingerprint {
		return nil, syscall.ESTALE
	}
	rc, err := zr.OpenStreamWithReader(layout.Index, source)
	if err != nil {
		if e := source.sourceError(); e != nil {
			return nil, e
		}
		return nil, archiveReadError(ctx, err, pw)
	}
	return &nestedSolid7zState{stream: rc, operation: op, source: source, layout: layout}, nil
}

// fill borrows the group per demanded block. The block cache still owns
// publication/singleflight; this layer never publishes discarded prefixes.
func (t *Tree) fillNestedSolid7z(ctx context.Context, plan *nestedSolid7zPlan, start, length int64, w io.Writer) (handled bool, err error) {
	e, err := t.nestedSolid7zPool.acquire(ctx, plan.key, plan.charge, plan.parent)
	if err != nil || e == nil {
		return false, err
	}
	healthy := true
	defer func() { t.nestedSolid7zPool.release(e, healthy) }()
	if err := ctx.Err(); err != nil {
		return true, err
	}
	target := plan.member.sevenStream.Offset + start
	if e.state != nil && target < e.state.position {
		return false, nil
	}
	if e.state == nil {
		e.state, err = t.openNestedSolid7zState(ctx, plan)
		if err != nil {
			healthy = false
			return true, err
		}
	}
	s := e.state
	s.operation.bind(ctx)
	defer s.operation.bind(nil)
	if err = s.consume(ctx, target, io.Discard); err == nil {
		err = s.consume(ctx, target+length, w)
	}
	healthy = err == nil
	return true, err
}
