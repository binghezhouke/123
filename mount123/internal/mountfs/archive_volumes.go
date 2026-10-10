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

// numeric7zVolume reports the first volume of the alternate naming scheme
// emitted by some archivers: 001.7z, 002.7z, ... .  Keep this separate from
// the conventional archive.7z.001 form because the visible archive name is
// also used for password sidecars and cache identities.
func numeric7zVolume(name string) (width int, ok bool) {
	lower := strings.ToLower(name)
	if !strings.HasSuffix(lower, ".7z") {
		return 0, false
	}
	digits := name[:len(name)-3]
	if len(digits) < 3 || strings.Trim(digits, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.Atoi(digits)
	if err != nil || n != 1 {
		return 0, false
	}
	return len(digits), true
}

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
		source, err := r.sourceAt(r.ctx, i)
		if err != nil {
			return total, err
		}
		n, err := source.ReadAtContext(r.ctx, p[:amount], off)
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
		source, err := storage.NewRemoteContext(r.owner.ctx, ctx, r.owner.cache, r.owner.cloudCacheKey(&f), f.Size,
			func(ctx context.Context) (string, error) { return r.owner.api.DownloadURL(ctx, f.ID) })
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

func (t *Tree) archiveSource(ctx context.Context, source *storage.Remote, a *archiveDescriptor) (io.ReaderAt, int64, string, error) {
	conventional := strings.HasSuffix(strings.ToLower(a.name), ".7z.001")
	width, numeric := numeric7zVolume(a.name)
	if !conventional && !numeric {
		return contextRemote{ctx: ctx, source: source}, a.size, archiveIdentity(source, a), nil
	}
	directory, err := t.cloudDirectory(ctx, a.parentID)
	if err != nil {
		return nil, 0, "", err
	}
	key := fmt.Sprintf("volumes:%d:%s:g%d", a.id, a.version, directory.generation)
	value, err := t.loadMeta(ctx, key, t.opts.DirectoryTTL, func(ctx context.Context) (any, int64, error) {
		parts := map[int]panapi.File{}
		for _, f := range directory.files {
			var number int
			if conventional {
				stem := a.name[:len(a.name)-4]
				if !strings.HasPrefix(f.Name, stem+".") {
					continue
				}
				suffix := strings.TrimPrefix(f.Name, stem+".")
				var e error
				number, e = strconv.Atoi(suffix)
				if e != nil || len(suffix) < 3 || strings.Trim(suffix, "0123456789") != "" {
					continue
				}
			} else {
				if len(f.Name) != width+3 || !strings.HasSuffix(strings.ToLower(f.Name), ".7z") {
					continue
				}
				number, _ = strconv.Atoi(f.Name[:width])
				if number < 1 || fmt.Sprintf("%0*d.7z", width, number) != f.Name {
					continue
				}
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
	return archiveIdentity(source, a) + ":" + a.kind() + ":" + t.passwordTag(a, password)
}
