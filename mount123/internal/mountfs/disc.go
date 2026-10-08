package mountfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"path"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/binghezhouke/123/mount123/internal/discimage"
	"github.com/binghezhouke/123/mount123/internal/iostats"
	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// discEntry identifies one path inside an ISO image. The image source remains
// on entry.source/archive; only this small, immutable locator is copied to an
// inode.
type discEntry struct {
	path     string
	size     uint64
	modified time.Time
}

type discDirectory struct {
	entries map[string]*entry
	names   []string
	bytes   int64
	format  string
}

func (n *Node) isDisc() bool {
	if n == nil || n.item == nil {
		return false
	}
	if n.item.disc != nil {
		return true
	}
	if n.item.cloud == nil || n.item.cloud.IsDir || n.tree.opts.DisableISODirs {
		return false
	}
	return strings.EqualFold(path.Ext(n.item.cloud.Name), ".iso")
}

func (t *Tree) cloudIsDirectory(f panapi.File) bool {
	return f.IsDir || (t.zipDirs && archiveKind(f.Name) != "") || (!t.opts.DisableISODirs && strings.EqualFold(path.Ext(f.Name), ".iso"))
}

func (n *Node) discSource(ctx context.Context) (*storage.Remote, int64, *archiveDescriptor, error) {
	var source *storage.Remote
	var size int64
	archive := n.item.archive
	if n.item.disc != nil {
		source = n.item.source
		size = n.item.archiveSize
	} else if n.item.cloud != nil {
		source = n.item.source
		size = n.item.cloud.Size
		if archive == nil {
			f := n.item.cloud
			archive = &archiveDescriptor{id: f.ID, parentID: f.ParentID, name: f.Name, version: f.Version, size: f.Size}
		}
	}
	if size == 0 && n.item.archiveSize > 0 {
		size = n.item.archiveSize
	}
	if source == nil {
		if n.item.cloud == nil {
			return nil, 0, nil, syscall.EIO
		}
		var err error
		source, err = n.source(ctx)
		if err != nil {
			return nil, 0, nil, err
		}
	}
	if size < 0 {
		return nil, 0, nil, syscall.EFBIG
	}
	return source, size, archive, nil
}

func discPath(item *entry) string {
	if item != nil && item.disc != nil && item.disc.path != "" {
		return item.disc.path
	}
	return "."
}

func validDiscPath(p string, maxDepth int) bool {
	if p == "." {
		return true
	}
	if p == "" || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00") {
		return false
	}
	parts := strings.Split(p, "/")
	if len(parts) > maxDepth {
		return false
	}
	for _, part := range parts {
		if !validName(part) {
			return false
		}
	}
	return path.Clean(p) == p
}

func (n *Node) discDirectory(ctx context.Context) (*discDirectory, error) {
	source, size, archive, err := n.discSource(ctx)
	if err != nil {
		return nil, err
	}
	p := discPath(n.item)
	if !validDiscPath(p, n.tree.opts.MaxDepth) {
		return nil, syscall.EINVAL
	}
	key := fmt.Sprintf("disc:%s:%s", source.Key(), p)
	value, err := n.tree.loadMeta(ctx, key, 0, func(buildCtx context.Context) (any, int64, error) {
		reader := newDiscContextReaderAt(buildCtx, source, size)
		image, err := discimage.Open(reader, size)
		if err != nil {
			return nil, 0, err
		}
		listed, err := image.ReadDir(p)
		if err != nil {
			return nil, 0, err
		}
		if len(listed) > n.tree.opts.MaxEntries {
			return nil, 0, fmt.Errorf("disc directory exceeds %d entries", n.tree.opts.MaxEntries)
		}
		directory := &discDirectory{entries: make(map[string]*entry, len(listed)), names: make([]string, 0, len(listed)), format: image.Format}
		bytes := int64(128)
		nameBytes := 0
		seen := make(map[string]struct{}, len(listed))
		for _, child := range listed {
			if err := buildCtx.Err(); err != nil {
				return nil, 0, err
			}
			if !validName(child.Name) {
				return nil, 0, errors.New("disc directory contains an invalid filename")
			}
			if _, exists := seen[child.Name]; exists {
				return nil, 0, errors.New("disc directory contains duplicate names")
			}
			seen[child.Name] = struct{}{}
			nameBytes += len(child.Name)
			if nameBytes > n.tree.opts.MaxNameBytes {
				return nil, 0, fmt.Errorf("disc directory names exceed %d-byte budget", n.tree.opts.MaxNameBytes)
			}
			if child.Size < 0 {
				return nil, 0, syscall.EFBIG
			}
			childPath := child.Name
			if p != "." {
				childPath = path.Join(p, child.Name)
			}
			if !validDiscPath(childPath, n.tree.opts.MaxDepth) {
				return nil, 0, errors.New("disc path exceeds depth or contains an invalid component")
			}
			locator := &discEntry{path: childPath, size: uint64(child.Size), modified: child.ModTime}
			item := &entry{name: child.Name, source: source, archiveSize: size, archive: archive, disc: locator, directory: child.Dir}
			directory.entries[child.Name] = item
			directory.names = append(directory.names, child.Name)
			bytes += int64(384 + len(child.Name) + len(childPath))
			if bytes > n.tree.opts.MetadataBytes {
				return nil, 0, fmt.Errorf("disc directory exceeds metadata budget; increase -metadata-mib")
			}
		}
		sort.Strings(directory.names)
		directory.bytes = bytes
		return directory, bytes, nil
	})
	if err != nil {
		return nil, err
	}
	return value.(*discDirectory), nil
}

func (n *Node) listDisc(ctx context.Context) (map[string]*entry, error) {
	directory, err := n.discDirectory(ctx)
	if err != nil {
		return nil, err
	}
	return directory.entries, nil
}

func (n *Node) lookupDisc(ctx context.Context, name string) (map[string]*entry, error) {
	if !validName(name) {
		return nil, nil
	}
	directory, err := n.discDirectory(ctx)
	if err != nil {
		return nil, err
	}
	item := directory.entries[name]
	if item == nil {
		return nil, nil
	}
	return map[string]*entry{name: item}, nil
}

func (n *Node) opendirDisc(ctx context.Context) (fs.FileHandle, uint32, syscall.Errno) {
	directory, err := n.discDirectory(ctx)
	if err != nil {
		return nil, 0, toErrno(err)
	}
	release, err := n.tree.pinDirectorySnapshot(directory, directory.bytes)
	if err != nil {
		return nil, 0, syscall.ENOMEM
	}
	snapshot := &frozenDirectorySnapshot{entries: directory.entries}
	return newFrozenDirectoryHandle(n, snapshot, directory.names, release), 0, 0
}

type discContextReaderAt struct {
	source   *storage.Remote
	size     int64
	mu       sync.RWMutex
	ctx      context.Context
	gate     chan struct{}
	prefetch bool
}

func newDiscContextReaderAt(ctx context.Context, source *storage.Remote, size int64) *discContextReaderAt {
	return &discContextReaderAt{source: source, size: size, ctx: ctx, gate: make(chan struct{}, 1)}
}

func (r *discContextReaderAt) ReadAt(p []byte, off int64) (int, error) {
	r.mu.RLock()
	ctx := r.ctx
	r.mu.RUnlock()
	if ctx == nil {
		ctx = context.Background()
	}
	if r.prefetch && len(p) > 0 && off >= 0 && off < r.size {
		length := min(int64(len(p)), r.size-off)
		if err := r.source.PrefetchRangeAtContext(ctx, off, length); err != nil {
			return 0, err
		}
	}
	return r.source.ReadAtContext(ctx, p, off)
}

func (r *discContextReaderAt) withContext(ctx context.Context, fn func() error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case r.gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	r.mu.Lock()
	previous := r.ctx
	r.ctx = ctx
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.ctx = previous
		r.mu.Unlock()
		<-r.gate
	}()
	return fn()
}

func (n *Node) openDisc(ctx context.Context) (fs.FileHandle, uint32, syscall.Errno) {
	if n.item.directory || n.item.disc == nil {
		return nil, 0, syscall.EISDIR
	}
	if n.item.disc.size > math.MaxInt64 {
		return nil, 0, syscall.EFBIG
	}
	source, imageSize, _, err := n.discSource(ctx)
	if err != nil {
		return nil, 0, toErrno(err)
	}
	path := n.item.disc.path
	if !validDiscPath(path, n.tree.opts.MaxDepth) || path == "." {
		return nil, 0, syscall.EINVAL
	}
	readerAt := newDiscContextReaderAt(ctx, source, imageSize)
	var image *discimage.FS
	var reader io.ReaderAt
	err = readerAt.withContext(ctx, func() error {
		var openErr error
		image, openErr = discimage.Open(readerAt, imageSize)
		if openErr != nil {
			return openErr
		}
		reader, openErr = image.OpenFile(path)
		return openErr
	})
	if err != nil {
		return nil, 0, toErrno(err)
	}
	h := &discHandle{reader: reader, readerAt: readerAt, size: n.item.disc.size}
	if n.tree.cache != nil {
		h.stats = n.tree.cache.IOStats()
	}
	if !n.tree.opts.DisableReadAhead && n.tree.opts.ReadAheadMaxBytes > 0 && h.size <= math.MaxInt64 {
		prefetch := &discPrefetchSource{source: source, imageSize: imageSize, path: path, size: n.item.disc.size}
		h.readAhead = newReadAhead(n.tree.ctx, prefetch, 0, h.size, n.tree.opts.ReadAheadMaxBytes)
		h.readAhead.statsTracker = h.stats
	}
	return h, fuse.FOPEN_DIRECT_IO, 0
}

type discHandle struct {
	mu        sync.Mutex
	reader    io.ReaderAt
	readerAt  *discContextReaderAt
	size      uint64
	stats     *iostats.Tracker
	readAhead *readAhead
	closed    bool
}

func (h *discHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	started := time.Now()
	if h.stats != nil {
		defer func() { h.stats.ObserveForegroundRead(time.Since(started)) }()
	}
	if err := ctx.Err(); err != nil {
		return nil, toErrno(err)
	}
	if off < 0 {
		return nil, syscall.EINVAL
	}
	if uint64(off) >= h.size {
		return fuse.ReadResultData(nil), 0
	}
	if uint64(len(dest)) > h.size-uint64(off) {
		dest = dest[:h.size-uint64(off)]
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, syscall.EBADF
	}
	h.mu.Unlock()
	var n int
	err := h.readerAt.withContext(ctx, func() error {
		var readErr error
		n, readErr = h.reader.ReadAt(dest, off)
		return readErr
	})
	if err != nil && err != io.EOF {
		return nil, toErrno(err)
	}
	if n != len(dest) {
		// The request has already been clipped to the immutable declared
		// length; a missing extent is an I/O error, not a successful EOF.
		return nil, syscall.EIO
	}
	if h.stats != nil && n > 0 {
		h.stats.RecordForegroundRead(uint64(n), time.Since(started))
	}
	if h.readAhead != nil && n > 0 {
		h.readAhead.observe(off, int64(n), time.Since(started))
	}
	return fuse.ReadResultData(dest[:n]), 0
}

func (h *discHandle) Release(context.Context) syscall.Errno {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return 0
	}
	h.closed = true
	readAhead := h.readAhead
	h.mu.Unlock()
	if readAhead != nil {
		readAhead.Close()
	}
	return 0
}

type discPrefetchSource struct {
	source    *storage.Remote
	imageSize int64
	path      string
	size      uint64
}

func (p *discPrefetchSource) DownloadStats() storage.DownloadStats {
	return p.source.DownloadStats()
}

func (p *discPrefetchSource) PrefetchRangeAtContext(ctx context.Context, off, size int64) error {
	if off < 0 || size < 0 || off > math.MaxInt64-size {
		return syscall.EINVAL
	}
	if size == 0 {
		return nil
	}
	if uint64(off) > p.size || uint64(size) > p.size-uint64(off) {
		return syscall.EINVAL
	}
	readerAt := newDiscContextReaderAt(ctx, p.source, p.imageSize)
	readerAt.prefetch = true
	var image *discimage.FS
	var reader io.ReaderAt
	err := readerAt.withContext(ctx, func() error {
		var openErr error
		image, openErr = discimage.Open(readerAt, p.imageSize)
		if openErr != nil {
			return openErr
		}
		reader, openErr = image.OpenFile(p.path)
		return openErr
	})
	if err != nil {
		return err
	}
	section := io.NewSectionReader(reader, off, size)
	buffer := make([]byte, 64<<10)
	n, err := io.CopyBuffer(io.Discard, section, buffer)
	if err != nil {
		return err
	}
	if n != size {
		return io.ErrUnexpectedEOF
	}
	return ctx.Err()
}
