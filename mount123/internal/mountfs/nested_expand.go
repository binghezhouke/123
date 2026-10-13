package mountfs

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/binghezhouke/123/mount123/internal/storage"
)

func (t *Tree) rootArchiveView(ctx context.Context, source *storage.Remote, a *archiveDescriptor, idx *zipIndex, password []byte) (*nestedArchive, error) {
	var input archiveReaderAt
	var size int64
	var identity string
	if a.kind() == ".zip" {
		if idx == nil || idx.prepared == nil {
			return nil, syscall.EIO
		}
		input, size, identity = idx.prepared.input, idx.prepared.size, idx.prepared.identity
	} else {
		reader, rawSize, rawIdentity, err := t.archiveSource(ctx, source, a)
		if err != nil {
			return nil, err
		}
		size, identity = rawSize, rawIdentity
		if volumes, ok := reader.(*volumeReaderAt); ok {
			input = volumes
		} else {
			input = source
		}
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

func (t *Tree) viewMemberInput(ctx context.Context, n *nestedArchive, original *member) (archiveReaderAt, io.Closer, error) {
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
	return t.viewMemberInputPrepared(ctx, n, m, password)
}

func (t *Tree) viewMemberInputPrepared(ctx context.Context, n *nestedArchive, m *member, password []byte) (archiveReaderAt, io.Closer, error) {
	input := viewInput(n)
	// Password discovery can load cloud metadata through the same admission
	// queue. Complete it before taking a decoder permit, including on a miss.
	canMapZIP := n.format == ".zip" && !m.encrypted && m.flags&1 == 0 && m.method == zip.Store && m.compressed == m.size
	canMapRAR := n.format == ".rar" && !m.encrypted && m.rarLocator != nil && m.rarLocator.Direct()
	if n.format == ".7z" || canMapZIP || canMapRAR {
		operation, release, e := t.acquireNestedDecode(ctx)
		if e != nil {
			return nil, nil, e
		}
		defer release()
		ctx = operation
	}
	if n.format == ".7z" {
		direct, owned, ok, e := nestedCopyMemberReader(ctx, input, n.size, m, m.ordinal, password)
		if e != nil {
			return nil, nil, e
		}
		if ok {
			return direct, owned, nil
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
	if canMapRAR {
		direct, ok, e := nestedRARStoredMemberReader(ctx, input, n.size, m, password)
		if e != nil {
			return nil, nil, e
		}
		if ok {
			return direct, direct, nil
		}
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

func nestedVolumeParts(idx *zipIndex, m *member, kind archiveSplitKind, lastDisk uint32) ([]*member, []int64, int64, error) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	candidates := make([]archiveVolumeCandidate, 0, len(idx.members))
	members := make([]*member, 0, len(idx.members))
	for name, candidate := range idx.members {
		if candidate.size > math.MaxInt64 {
			return nil, nil, 0, syscall.EFBIG
		}
		candidates = append(candidates, archiveVolumeCandidate{name, int64(candidate.size), false})
		members = append(members, candidate)
	}
	// Directory names are candidates too: a required volume cannot silently be
	// replaced by a directory, including one represented only by child entries.
	parent := m.name
	slash := strings.LastIndexByte(parent, '/')
	prefix, directory := "", ""
	if slash >= 0 {
		prefix, directory = parent[:slash+1], parent[:slash]
	}
	if dir := idx.dirLocked(directory); dir != nil {
		for name := range dir.dirs {
			candidates = append(candidates, archiveVolumeCandidate{prefix + name, 0, true})
			members = append(members, nil)
		}
	}
	indices, sizes, total, err := discoverArchiveVolumes(m.name, kind, lastDisk, candidates)
	if err != nil {
		return nil, nil, 0, err
	}
	parts := make([]*member, len(indices))
	for i, index := range indices {
		parts[i] = members[index]
	}
	return parts, sizes, total, nil
}

func nestedParts(idx *zipIndex, m *member) ([]*member, int64, error) {
	kind := archiveSingle
	if _, ok := byteSplitStem(m.name); ok {
		kind = archiveByteSplit
	}
	parts, _, size, err := nestedVolumeParts(idx, m, kind, 0)
	return parts, size, err
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
	splitKind := archiveSingle
	if _, ok := byteSplitStem(m.name); ok {
		splitKind = archiveByteSplit
	}
	var finalInput *nestedVolumeInput
	var lastDisk uint32
	if kind == ".zip" && splitKind == archiveSingle {
		finalInput = &nestedVolumeInput{owner: t, parent: parent, member: m}
		child.lazyInputs = append(child.lazyInputs, finalInput)
		recorder := &nestedSourceReader{}
		hint, e := readZIPVolumeHint(ctx, nestedSourceInput{input: finalInput, recorder: recorder}, int64(m.size))
		if e != nil {
			if sourceErr := recorder.sourceError(); sourceErr != nil {
				return nil, sourceErr
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if nestedEncryptedInput(finalInput) && (errors.Is(e, syscall.EIO) || errors.Is(e, syscall.EOPNOTSUPP)) {
				return nil, syscall.EACCES
			}
			return nil, e
		}
		splitKind, lastDisk = hint.kind, hint.lastDisk
	}
	parts, sizes, size, err := nestedVolumeParts(parent.index, m, splitKind, lastDisk)
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
	// Resolve possible parent secrets before an index decoder permit is held.
	// Lazy volume mapping then only decrypts this already-discovered value.
	discoverPassword := parent.format == ".7z"
	for _, part := range parts {
		discoverPassword = discoverPassword || part.encrypted
	}
	knownPassword, err := t.viewPassword(ctx, parent, discoverPassword)
	if err != nil {
		return nil, err
	}
	clear(knownPassword)
	if finalInput != nil {
		finalInput.passwordKnown = true
	}
	identities := make([]partIdentity, 0, len(parts))
	inputs := make([]archiveReaderAt, 0, len(parts))
	for _, part := range parts {
		input := finalInput
		if input == nil || part.name != m.name {
			input = &nestedVolumeInput{owner: t, parent: parent, member: part, passwordKnown: true}
			child.lazyInputs = append(child.lazyInputs, input)
		}
		inputs = append(inputs, input)
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
	namespace := "nested-view-input-v1"
	if kind == ".zip" && splitKind != archiveSingle {
		encoded, _ = json.Marshal(struct {
			Parent, ParentPassword, Format string
			Parts                          []partIdentity
			Split                          archiveSplitKind
		}{parent.identity, parentPasswordTag, kind, identities, splitKind})
		namespace = "nested-view-input-v2"
	}
	child.cachePrefix = t.cache.StableDigest(namespace, string(encoded))
	// Parse clear headers without password discovery. For encrypted headers, try
	// the current inner sidecar/shared password, then the effective parent secret.
	index := func(password []byte) (result *zipIndex, err error) {
		operation, release, e := t.acquireNestedDecode(ctx)
		if e != nil {
			return nil, e
		}
		defer release()
		bound := &nestedSourceReader{ReaderAt: archiveRequestReader{ctx: operation, input: child.input}}
		defer func() {
			if sourceErr := bound.sourceError(); sourceErr != nil {
				result, err = nil, nestedSourceError{sourceErr}
				return
			}
			if err == nil || operation.Err() != nil || !nestedEncryptedInput(child.input) {
				return
			}
			var errno syscall.Errno
			if !errors.As(err, &errno) || errors.Is(err, syscall.EIO) {
				// A rejected inner header after successful AES body reads is
				// indistinguishable from an incorrect parent password.
				err = syscall.EACCES
			}
		}()
		if kind == ".zip" {
			raw := &preparedArchiveInput{input: nestedSourceInput{input: child.input, recorder: bound}, size: size, identity: child.cachePrefix, layout: archiveVolumeLayout{kind: splitKind, sizes: sizes}}
			indexInput, indexSize, e := prepareZIPIndexInput(operation, raw, defaults(t.opts).MaxZIPEntries, min(policy.MaxIndexBytes, defaults(t.opts).MetadataBytes))
			if e != nil {
				return nil, e
			}
			idx, e := buildZIP(operation, t, indexInput, indexSize)
			if e == nil {
				idx.attachZIPReader(child.input)
			}
			return idx, e
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
		var sourceErr nestedSourceError
		if errors.As(err, &sourceErr) {
			return nil, sourceErr.err
		}
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
	owned   io.Closer
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
		if h.owned != nil {
			err = h.owned.Close()
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
	input, owned, err := t.viewMemberInput(ctx, n, m)
	if err != nil {
		release()
		return nil, 0, err
	}
	if _, decoded := input.(*nestedMemberReader); !decoded {
		integrityErr := errArchiveIntegrity
		if n.format == ".zip" {
			integrityErr = zip.ErrChecksum
		}
		input = newArchiveCheckedInput(input, int64(m.size), m.crc, n.format == ".zip" || m.crc != 0, integrityErr)
	}
	return &nestedReadHandle{ctx: ctx, input: input, owned: owned, release: release}, int64(m.size), nil
}
