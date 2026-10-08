package mountfs

import (
	"context"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
)

type volumeReaderAt struct {
	mu      sync.Mutex
	owner   *Tree
	parts   []panapi.File
	ctx     context.Context
	sources []*storage.Remote
	sizes   []int64
}

func (r *volumeReaderAt) ReadAt(p []byte, off int64) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
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
		if r.sources[i] == nil {
			f := r.parts[i]
			src, err := storage.NewRemoteContext(r.owner.ctx, r.ctx, r.owner.cache, r.owner.cloudCacheKey(&f), f.Size,
				func(ctx context.Context) (string, error) { return r.owner.api.DownloadURL(ctx, f.ID) })
			if err != nil {
				return total, err
			}
			r.sources[i] = src
		}
		n, err := r.sources[i].ReadAtContext(r.ctx, p[:amount], off)
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

func (t *Tree) archiveSource(ctx context.Context, source *storage.Remote, a *archiveDescriptor) (io.ReaderAt, int64, string, error) {
	if !strings.HasSuffix(strings.ToLower(a.name), ".7z.001") {
		return contextRemote{ctx: ctx, source: source}, a.size, archiveIdentity(source, a), nil
	}
	stem := a.name[:len(a.name)-4]
	directory, err := t.cloudDirectory(ctx, a.parentID)
	if err != nil {
		return nil, 0, "", err
	}
	key := fmt.Sprintf("volumes:%d:%s:g%d", a.id, a.version, directory.generation)
	value, err := t.loadMeta(ctx, key, t.opts.DirectoryTTL, func(ctx context.Context) (any, int64, error) {
		parts := map[int]panapi.File{}
		for _, f := range directory.files {
			if !strings.HasPrefix(f.Name, stem+".") {
				continue
			}
			suffix := strings.TrimPrefix(f.Name, stem+".")
			number, e := strconv.Atoi(suffix)
			if e != nil || len(suffix) < 3 || strings.Trim(suffix, "0123456789") != "" {
				continue
			}
			if number < 1 || number > 1000 || f.IsDir || f.Size <= 0 {
				return nil, 0, syscall.EIO
			}
			if _, exists := parts[number]; exists {
				return nil, 0, syscall.EIO
			}
			parts[number] = f
		}
		result := make([]panapi.File, 0, len(parts))
		cost := int64(128)
		for i := 1; i <= len(parts); i++ {
			f, ok := parts[i]
			if !ok {
				return nil, 0, syscall.EIO
			}
			result = append(result, f)
			cost += int64(256 + len(f.Name) + len(f.Version))
		}
		if len(result) == 0 || result[0].ID != a.id || result[0].Version != a.version || result[0].Size != a.size {
			return nil, 0, syscall.ESTALE
		}
		return result, cost, nil
	})
	if err != nil {
		return nil, 0, "", err
	}
	parts := value.([]panapi.File)
	reader := &volumeReaderAt{ctx: ctx, owner: t, parts: parts, sources: make([]*storage.Remote, len(parts))}
	reader.sources[0] = source
	size := int64(0)
	keys := make([]string, 0, len(parts))
	for _, f := range parts {
		if size > math.MaxInt64-f.Size {
			return nil, 0, "", syscall.EFBIG
		}
		size += f.Size
		reader.sizes = append(reader.sizes, f.Size)
		keys = append(keys, t.cloudCacheKey(&f))
	}
	// The ordered parts and their pinned remote versions form the cache identity.
	return reader, size, archiveIdentity(source, a) + ":" + strings.Join(keys, "|"), nil
}

// API identity survives renames, moves and changes of CDN URLs/validators.
// Without a cloud version, keep the remote reader's conservative identity.
func archiveIdentity(source *storage.Remote, a *archiveDescriptor) string {
	if a.version == "" {
		return source.Key()
	}
	return fmt.Sprintf("cloud:%d:%s:%d", a.id, a.version, a.size)
}
func (t *Tree) archiveTaskKey(source *storage.Remote, a *archiveDescriptor, password []byte) string {
	return archiveIdentity(source, a) + ":" + archiveKind(a.name) + ":" + t.passwordTag(a, password)
}
