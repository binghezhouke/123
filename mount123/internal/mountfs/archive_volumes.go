package mountfs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"syscall"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
)

type volumeReaderAt struct {
	mu           sync.Mutex
	owner        *Tree
	parts        []panapi.File
	ctx          context.Context
	sources      []*storage.Remote
	sizes        []int64
	initializing map[int]chan struct{}
}

func (r *volumeReaderAt) ReadAt(p []byte, off int64) (int, error) {
	return r.ReadAtContext(r.ctx, p, off)
}

func (r *volumeReaderAt) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if off < 0 {
		return 0, syscall.EINVAL
	}
	total := 0
	for i, size := range r.sizes {
		if off >= size {
			off -= size
			continue
		}
		amount := int64(len(p))
		if amount > size-off {
			amount = size - off
		}
		source, err := r.sourceAt(ctx, i)
		if err != nil {
			return total, err
		}
		n, err := source.ReadAtContext(ctx, p[:amount], off)
		total += n
		p = p[n:]
		if err != nil && !(err == io.EOF && int64(n) == amount) {
			return total, err
		}
		if int64(n) != amount {
			return total, io.ErrUnexpectedEOF
		}
		if len(p) == 0 {
			return total, nil
		}
		off = 0
	}
	return total, io.EOF
}

// Only source initialization is coalesced. Network reads never hold the volume
// mutex, so independent packed inputs and following windows can run in parallel.
func (r *volumeReaderAt) sourceAt(ctx context.Context, index int) (*storage.Remote, error) {
	for {
		r.mu.Lock()
		if source := r.sources[index]; source != nil {
			r.mu.Unlock()
			return source, nil
		}
		if pending := r.initializing[index]; pending != nil {
			r.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-pending:
			}
			continue
		}
		if r.initializing == nil {
			r.initializing = make(map[int]chan struct{})
		}
		pending := make(chan struct{})
		r.initializing[index] = pending
		r.mu.Unlock()
		f := r.parts[index]
		source, err := r.owner.cloudArchiveSource(ctx, &f)
		r.mu.Lock()
		if err == nil {
			r.sources[index] = source
		}
		delete(r.initializing, index)
		close(pending)
		r.mu.Unlock()
		return source, err
	}
}

func (t *Tree) prepareCloudArchiveInput(ctx context.Context, source *storage.Remote, a *archiveDescriptor) (*preparedArchiveInput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if source == nil || a == nil || a.size <= 0 {
		return nil, syscall.EIO
	}
	kind := archiveSingle
	if _, ok := byteSplitStem(a.name); ok {
		kind = archiveByteSplit
	}
	var lastDisk uint32
	if kind == archiveSingle && a.kind() == ".zip" {
		hint, err := readZIPVolumeHint(ctx, source, a.size)
		if err != nil {
			return nil, err
		}
		kind, lastDisk = hint.kind, hint.lastDisk
	}
	if kind == archiveSingle {
		// Archive content is immutable; the API file ID pins this input even
		// when the API omits a content version.
		return &preparedArchiveInput{input: source, size: a.size, identity: archiveIdentity(source, a), layout: archiveVolumeLayout{kind: archiveSingle, sizes: []int64{a.size}}, durable: true}, nil
	}
	directory, err := t.cloudDirectory(ctx, a.parentID)
	if err != nil {
		return nil, err
	}
	candidates := make([]archiveVolumeCandidate, len(directory.files))
	for i, f := range directory.files {
		candidates[i] = archiveVolumeCandidate{f.Name, f.Size, f.IsDir}
	}
	indices, sizes, total, err := discoverArchiveVolumes(a.name, kind, lastDisk, candidates)
	if err != nil {
		return nil, err
	}
	parts := make([]panapi.File, len(indices))
	pinned := 0
	if kind == archiveZIPDisks {
		pinned = len(indices) - 1
	}
	for i, index := range indices {
		parts[i] = directory.files[index]
	}
	f := parts[pinned]
	if f.ID != a.id || f.Version != a.version || f.Size != a.size {
		return nil, syscall.ESTALE
	}
	lifetime := t.ctx
	if lifetime == nil {
		lifetime = context.Background()
	}
	reader := &volumeReaderAt{ctx: lifetime, owner: t, parts: parts, sources: make([]*storage.Remote, len(parts)), sizes: sizes}
	reader.sources[pinned] = source
	type partIdentity struct {
		ID      int64
		Version string
		Size    int64
	}
	identities := make([]partIdentity, len(parts))
	for i, f := range parts {
		identities[i] = partIdentity{f.ID, f.Version, f.Size}
	}
	encoded, _ := json.Marshal(struct {
		Kind  archiveSplitKind
		Parts []partIdentity
	}{kind, identities})
	identity := "archive-split-v2:" + string(encoded)
	if a.kind() == ".7z" && kind == archiveByteSplit {
		// Existing canonical 7z sets keep their warm index and member keys.
		keys := make([]string, len(parts))
		for i, part := range parts {
			keys[i] = t.cloudCacheKey(&part)
		}
		identity = archiveIdentity(source, a) + ":" + strings.Join(keys, "|")
	}
	return &preparedArchiveInput{input: reader, size: total, identity: identity, layout: archiveVolumeLayout{kind: kind, sizes: sizes}, durable: true}, nil
}

func (t *Tree) archiveSource(ctx context.Context, source *storage.Remote, a *archiveDescriptor) (io.ReaderAt, int64, string, error) {
	prepared, err := t.prepareCloudArchiveInput(ctx, source, a)
	if err != nil {
		return nil, 0, "", err
	}
	if volumes, ok := prepared.input.(*volumeReaderAt); ok {
		return volumes, prepared.size, prepared.identity, nil
	}
	// Preserve the packed 7z window adapter used by existing single sources.
	return contextRemote{ctx: ctx, source: source}, prepared.size, prepared.identity, nil
}

// Archive contents are immutable under a cloud file ID. Identity survives
// renames, moves, omitted API versions and changes of CDN URLs/validators.
func archiveIdentity(source *storage.Remote, a *archiveDescriptor) string {
	return fmt.Sprintf("cloud:%d:%s:%d", a.id, a.version, a.size)
}
func (t *Tree) archiveTaskKey(source *storage.Remote, a *archiveDescriptor, password []byte) string {
	return archiveIdentity(source, a) + ":" + a.kind() + ":" + t.passwordTag(a, password)
}
