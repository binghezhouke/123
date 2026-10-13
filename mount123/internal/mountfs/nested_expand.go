package mountfs

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/binghezhouke/123/mount123/internal/storage"
)

func (t *Tree) rootArchiveView(ctx context.Context, source *storage.Remote, a *archiveDescriptor, idx *zipIndex, password []byte) (*nestedArchive, error) {
	reader, size, identity, err := t.archiveSource(ctx, source, a)
	if err != nil {
		return nil, err
	}
	var input archiveReaderAt
	if volumes, ok := reader.(*volumeReaderAt); ok {
		input = volumes
	} else {
		input = source
	}
	key := t.cache.StableDigest("nested-view-v1", t.diskCacheScope()+"\x00"+identity+"\x00"+a.kind()+"\x00"+t.passwordTag(a, password))
	protected, err := t.encryptPassword(key, password)
	if err != nil {
		return nil, err
	}
	idx.mu.RLock()
	indexBytes := idx.bytes
	idx.mu.RUnlock()
	return &nestedArchive{input: input, size: size, format: a.kind(), index: idx, archive: a, identity: key, cachePrefix: key,
		protected: protected, protectedSet: true, refs: 1, indexBytes: indexBytes}, nil
}

func (t *Tree) buildNestedArchive(ctx context.Context, source *storage.Remote, a *archiveDescriptor, idx *zipIndex, memberPath string, m *member) (*nestedArchive, error) {
	var password []byte
	var err error
	if idx == nil {
		if a.kind() == ".zip" {
			idx, err = t.getZIP(ctx, source, a.size, a)
		} else {
			idx, err = t.otherIndex(ctx, source, a, nil)
			if needsPassword(err) || errors.Is(err, syscall.EACCES) {
				password, err = t.otherPassword(ctx, a)
				if err == nil {
					idx, err = t.otherIndex(ctx, source, a, password)
				}
			}
		}
		if err != nil {
			clear(password)
			return nil, err
		}
	}
	defer clear(password)
	parent, err := t.rootArchiveView(ctx, source, a, idx, password)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	return t.enterNestedArchive(ctx, parent, memberPath, m)
}

func (t *Tree) viewPassword(ctx context.Context, n *nestedArchive, discover bool) ([]byte, error) {
	n.passwordMu.Lock()
	defer n.passwordMu.Unlock()
	var password []byte
	var err error
	if n.protectedSet {
		password, err = t.decryptPassword(n.cachePrefix, n.protected)
	} else {
		password = append([]byte(nil), n.password...)
	}
	if err != nil || len(password) > 0 || !discover {
		return password, err
	}
	clear(password)
	if n.archive != nil {
		password, err = t.otherPassword(ctx, n.archive)
	}
	if err != nil {
		return nil, err
	}
	if len(password) == 0 && n.parent != nil {
		password, err = t.viewPassword(ctx, n.parent, true)
	}
	if err != nil {
		return nil, err
	}
	if len(password) > 0 {
		protected, e := t.encryptPassword(n.cachePrefix, password)
		if e != nil {
			clear(password)
			return nil, e
		}
		clear(n.protected.ciphertext)
		n.protected, n.protectedSet = protected, true
	}
	return password, nil
}

func viewInput(n *nestedArchive) archiveReaderAt {
	if n.input != nil {
		return n.input
	}
	return archivePlainInput{reader: n.reader}
}

// Resolve metadata against this exact parent. FUSE copies omit transient ZIP
// readers; looking up the original also prevents accidental cross-layer use.
func viewMember(n *nestedArchive, m *member) (*member, error) {
	if n.index == nil || m == nil {
		return nil, syscall.ENOENT
	}
	n.index.mu.RLock()
	original := n.index.members[m.name]
	n.index.mu.RUnlock()
	if original == nil || original.size != m.size || original.crc != m.crc {
		return nil, syscall.ESTALE
	}
	copy := *original
	copy.format = n.format
	return &copy, nil
}

func (t *Tree) viewMemberInput(ctx context.Context, n *nestedArchive, original *member) (archiveReaderAt, *nestedMemberReader, error) {
	m, err := viewMember(n, original)
	if err != nil {
		return nil, nil, err
	}
	// 7z metadata does not expose per-stream encryption. Retain the existing
	// sidecar discovery for that format; ZIP and RAR expose encrypted members.
	password, err := t.viewPassword(ctx, n, m.encrypted || n.format == ".7z")
	if err != nil {
		return nil, nil, err
	}
	defer clear(password)
	input := viewInput(n)
	// Password discovery can load cloud metadata through the same admission
	// queue. Complete it before taking a decoder permit, including on a miss.
	canMapZIP := n.format == ".zip" && !m.encrypted && m.flags&1 == 0 && m.method == zip.Store && m.compressed == m.size
	if n.format == ".7z" || canMapZIP {
		operation, release, e := t.acquireNestedDecode(ctx)
		if e != nil {
			return nil, nil, e
		}
		defer release()
		ctx = operation
	}
	bound := archiveRequestReader{ctx: ctx, input: input}
	if n.format == ".7z" {
		direct, ok, e := nestedCopyMemberReader(bound, n.size, m, m.ordinal, password)
		if e != nil {
			return nil, nil, e
		}
		if ok {
			mapped := direct.(nestedOffsetReader)
			return archiveOffsetInput{input: input, start: mapped.start, size: mapped.size}, nil, nil
		}
	}
	if canMapZIP {
		offset, e := m.dataOffset(ctx)
		if e != nil {
			return nil, nil, e
		}
		if offset < 0 || offset > n.size || m.size > uint64(n.size-offset) {
			return nil, nil, syscall.EIO
		}
		return archiveOffsetInput{input: input, start: offset, size: int64(m.size)}, nil, nil
	}
	blockBytes := t.nestedBlockBytes()
	identity, _ := json.Marshal(struct {
		Parent, Name, Password string
		Ordinal                int
		Size                   uint64
		CRC                    uint32
		BlockBytes             int64
	}{n.identity, m.name, t.passwordTag(n.archive, password), m.ordinal, m.size, m.crc, blockBytes})
	key := t.cache.StableDigest("nested-member-range-v4", string(identity))
	protected, err := t.encryptPassword(key, password)
	if err != nil {
		return nil, nil, err
	}
	lifetime := t.ctx
	if lifetime == nil {
		lifetime = context.Background()
	}
	r := &nestedMemberReader{ctx: lifetime, owner: t, cache: t.cache, key: key, size: int64(m.size), blockBytes: blockBytes, open: func(readCtx context.Context) (io.ReadCloser, error) {
		pw, e := t.decryptPassword(key, protected)
		if e != nil {
			return nil, e
		}
		defer clear(pw)
		stream, e := openArchiveMemberStream(readCtx, archiveRequestReader{ctx: readCtx, input: input}, n.size, m, pw)
		if e != nil {
			return nil, archiveReadError(readCtx, e, pw)
		}
		return stream, nil
	}}
	return r, r, nil
}

func nestedParts(idx *zipIndex, m *member) ([]*member, int64, error) {
	parts := []*member{m}
	if strings.HasSuffix(strings.ToLower(m.name), ".7z.001") {
		stem := m.name[:len(m.name)-4]
		idx.mu.RLock()
		for name, candidate := range idx.members {
			if !strings.HasPrefix(name, stem+".") || name == m.name {
				continue
			}
			suffix := strings.TrimPrefix(name, stem+".")
			number, err := strconv.Atoi(suffix)
			if err != nil || len(suffix) != 3 || number < 2 {
				continue
			}
			parts = append(parts, candidate)
		}
		idx.mu.RUnlock()
		sort.Slice(parts, func(i, j int) bool { return parts[i].name < parts[j].name })
		for i, part := range parts {
			if part.name != fmt.Sprintf("%s.%03d", stem, i+1) {
				return nil, 0, syscall.EIO
			}
		}
	}
	var size int64
	for _, part := range parts {
		if part.size > math.MaxInt64 || int64(part.size) > math.MaxInt64-size {
			return nil, 0, syscall.EFBIG
		}
		size += int64(part.size)
	}
	return parts, size, nil
}

// enterNestedArchive is the single explicit hop used by both frontends. It
// retains the full ancestor chain and indexes through demand-backed inputs.
func (t *Tree) enterNestedArchive(ctx context.Context, parent *nestedArchive, memberPath string, m *member) (*nestedArchive, error) {
	kind := archiveKind(m.name)
	if kind != ".7z" && kind != ".rar" && kind != ".zip" {
		return nil, syscall.EOPNOTSUPP
	}
	policy := defaultNestedArchivePolicy
	policy.MaxDepth = defaults(t.opts).MaxArchiveDepth
	if err := policy.allow(parent.depth+1, int64(m.size)); err != nil {
		return nil, err
	}
	releaseParent, err := parent.retain()
	if err != nil {
		return nil, err
	}
	child := &nestedArchive{parent: parent, parentRelease: releaseParent, refs: 1, depth: parent.depth + 1, format: kind}
	success := false
	defer func() {
		if !success {
			_ = child.Close()
		}
	}()
	parts, size, err := nestedParts(parent.index, m)
	if err != nil {
		return nil, err
	}
	if err = policy.allow(child.depth, size); err != nil {
		return nil, err
	}
	child.size = size
	a := archiveDescriptor{}
	if parent.archive != nil {
		a = *parent.archive
	}
	a.name, a.size, a.format = m.name, size, kind
	child.archive = &a
	type partIdentity struct {
		Name    string
		Ordinal int
		Size    uint64
		CRC     uint32
	}
	identities := make([]partIdentity, 0, len(parts))
	inputs := make([]archiveReaderAt, 0, len(parts))
	sizes := make([]int64, 0, len(parts))
	for _, part := range parts {
		input, lazy, e := t.viewMemberInput(ctx, parent, part)
		if e != nil {
			return nil, e
		}
		inputs = append(inputs, input)
		sizes = append(sizes, int64(part.size))
		if lazy != nil {
			child.lazyReaders = append(child.lazyReaders, lazy)
		}
		identities = append(identities, partIdentity{part.name, part.ordinal, part.size, part.crc})
	}
	child.input = inputs[0]
	if len(inputs) > 1 {
		child.input = archiveConcatInput{parts: inputs, sizes: sizes}
	}
	parentPassword, err := t.viewPassword(ctx, parent, false)
	if err != nil {
		return nil, err
	}
	parentPasswordTag := t.passwordTag(parent.archive, parentPassword)
	clear(parentPassword)
	encoded, _ := json.Marshal(struct {
		Parent, ParentPassword, Format string
		Parts                          []partIdentity
	}{parent.identity, parentPasswordTag, kind, identities})
	child.cachePrefix = t.cache.StableDigest("nested-view-input-v1", string(encoded))
	// Parse clear headers without password discovery. For encrypted headers, try
	// the current inner sidecar/shared password, then the effective parent secret.
	index := func(password []byte) (*zipIndex, error) {
		operation, release, e := t.acquireNestedDecode(ctx)
		if e != nil {
			return nil, e
		}
		defer release()
		bound := archiveRequestReader{ctx: operation, input: child.input}
		if kind == ".zip" {
			return buildZIP(operation, t, child.input, size)
		}
		var members []archiveMember
		if kind == ".rar" {
			members, e = indexNestedRAR(operation, bound, size, password, policy)
		} else {
			members, e = indexNested7z(operation, bound, size, password, policy)
		}
		if e != nil {
			return nil, e
		}
		idx, e := nestedIndexFromMembers(members, policy)
		if e == nil {
			for _, member := range idx.members {
				member.format = kind
			}
		}
		return idx, e
	}
	child.index, err = index(nil)
	if needsPassword(err) || errors.Is(err, syscall.EACCES) {
		password, e := t.viewPassword(ctx, child, true)
		if e != nil {
			return nil, e
		}
		child.index, err = index(password)
		clear(password)
	}
	if err != nil {
		return nil, archiveReadError(ctx, err, nil)
	}
	child.indexBytes = parent.indexBytes + child.index.bytes
	if child.index.bytes > policy.MaxIndexBytes || child.indexBytes > defaults(t.opts).MetadataBytes {
		return nil, syscall.EFBIG
	}
	password, err := t.viewPassword(ctx, child, false)
	if err != nil {
		return nil, err
	}
	child.identity = t.cache.StableDigest("nested-view-identity-v1", child.cachePrefix+"\x00"+t.passwordTag(&a, password))
	clear(password)
	success = true
	return child, nil
}

type nestedReadHandle struct {
	ctx     context.Context
	input   archiveReaderAt
	decoder *nestedMemberReader
	release func()
	once    sync.Once
	closed  atomic.Bool
}

func (h *nestedReadHandle) ReadAt(p []byte, off int64) (int, error) {
	return h.ReadAtContext(h.ctx, p, off)
}
func (h *nestedReadHandle) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	if h.closed.Load() {
		return 0, storage.ErrClosed
	}
	return h.input.ReadAtContext(ctx, p, off)
}
func (h *nestedReadHandle) Close() error {
	var err error
	h.once.Do(func() {
		h.closed.Store(true)
		if h.decoder != nil {
			err = h.decoder.Close()
		}
		if h.release != nil {
			h.release()
		}
	})
	return err
}
func (t *Tree) openNestedArchiveMember(ctx context.Context, n *nestedArchive, m *member) (*nestedReadHandle, int64, error) {
	release, err := n.retain()
	if err != nil {
		return nil, 0, err
	}
	input, decoder, err := t.viewMemberInput(ctx, n, m)
	if err != nil {
		release()
		return nil, 0, err
	}
	if decoder == nil {
		integrityErr := errArchiveIntegrity
		if n.format == ".zip" {
			integrityErr = zip.ErrChecksum
		}
		input = newArchiveCheckedInput(input, int64(m.size), m.crc, n.format == ".zip" || m.crc != 0, integrityErr)
	}
	return &nestedReadHandle{ctx: ctx, input: input, decoder: decoder, release: release}, int64(m.size), nil
}
