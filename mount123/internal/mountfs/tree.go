// Package mountfs exposes a read-only cloud tree and ZIP virtual directories.
package mountfs

import (
	"archive/zip"
	"compress/flate"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"hash/fnv"
	"io"
	"log"
	"math"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type API interface {
	List(context.Context, int64) ([]panapi.File, error)
	DownloadURL(context.Context, int64) (string, error)
}

// Options sets mount-local metadata and source reuse lifetimes. Zero values
// select documented defaults.
type Options struct {
	DirectoryTTL        time.Duration
	SourceTTL           time.Duration
	ZIPIndexTTL         time.Duration
	MetadataBytes       int64
	MaxEntries          int
	MaxZIPEntries       int
	MaxExpandedNodes    int
	MaxDepth            int
	MaxNameBytes        int
	MaxConcurrentBuilds int
}

func defaults(o Options) Options {
	if o.DirectoryTTL <= 0 {
		o.DirectoryTTL = 30 * time.Second
	}
	if o.SourceTTL <= 0 {
		o.SourceTTL = 30 * time.Second
	}
	if o.ZIPIndexTTL <= 0 {
		o.ZIPIndexTTL = 30 * time.Second
	}
	if o.MetadataBytes <= 0 {
		o.MetadataBytes = 64 << 20
	}
	if o.MaxEntries <= 0 {
		o.MaxEntries = 100000
	}
	if o.MaxZIPEntries <= 0 {
		o.MaxZIPEntries = 100000
	}
	if o.MaxExpandedNodes <= 0 {
		o.MaxExpandedNodes = 100000
	}
	if o.MaxDepth <= 0 {
		o.MaxDepth = 128
	}
	if o.MaxNameBytes <= 0 {
		o.MaxNameBytes = 16 << 20
	}
	if o.MaxConcurrentBuilds <= 0 {
		o.MaxConcurrentBuilds = 4
	}
	return o
}

type Tree struct {
	ctx       context.Context
	api       API
	cache     *storage.Cache
	zipDirs   bool
	opts      Options
	mu        sync.Mutex
	meta      map[string]*metaItem
	metaBytes int64
	seq       uint64
	builds    chan struct{}
	sources   map[string]*sourceCall
}
type entry struct {
	name        string
	cloud       *panapi.File
	member      *member
	source      *storage.Remote
	zipPath     string
	archiveSize int64
	directory   bool
}
type member struct {
	file             *zip.File
	reader           *contextZIPReaderAt
	name             string
	method           uint16
	flags            uint16
	crc              uint32
	compressed, size uint64
}
type Node struct {
	fs.Inode
	tree *Tree
	item *entry
}
type metaItem struct {
	key     string
	value   any
	bytes   int64
	expires time.Time
	seq     uint64
}
type sourceCall struct {
	done  chan struct{}
	value any
	err   error
}

func New(ctx context.Context, api API, cache *storage.Cache, rootID int64, zipDirs bool) *Node {
	return NewWithOptions(ctx, api, cache, rootID, zipDirs, Options{})
}
func NewWithOptions(ctx context.Context, api API, cache *storage.Cache, rootID int64, zipDirs bool, opts Options) *Node {
	if ctx == nil {
		ctx = context.Background()
	}
	t := &Tree{ctx: ctx, api: api, cache: cache, zipDirs: zipDirs, opts: defaults(opts), meta: map[string]*metaItem{}, builds: make(chan struct{}, defaults(opts).MaxConcurrentBuilds), sources: map[string]*sourceCall{}}
	return &Node{tree: t, item: &entry{directory: true, cloud: &panapi.File{ID: rootID, IsDir: true}}}
}

// Prepare validates and caches the root listing before mounting.
func (n *Node) Prepare(ctx context.Context) error { _, err := n.list(ctx); return err }

func validName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\\x00") && utf8.ValidString(name)
}

// loadMeta coalesces each key before taking a build slot. Waiters never hold a
// slot, so cancellation retries cannot deadlock the build semaphore.
func (t *Tree) loadMeta(ctx context.Context, key string, ttl time.Duration, build func(context.Context) (any, int64, error)) (any, error) {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(t.ctx, cancel)
	defer func() { stop(); cancel() }()
	if t.ctx.Err() != nil {
		cancel()
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		t.mu.Lock()
		if item := t.meta[key]; item != nil {
			if time.Now().Before(item.expires) {
				t.seq++
				item.seq = t.seq
				value := item.value
				t.mu.Unlock()
				return value, nil
			}
			delete(t.meta, key)
			t.metaBytes -= item.bytes
		}
		if flight := t.sources[key]; flight != nil {
			t.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-flight.done:
			}
			if errors.Is(flight.err, context.Canceled) || errors.Is(flight.err, context.DeadlineExceeded) {
				continue
			}
			return flight.value, flight.err
		}
		flight := &sourceCall{done: make(chan struct{})}
		t.sources[key] = flight
		t.mu.Unlock()
		var value any
		var size int64
		var err error
		select {
		case t.builds <- struct{}{}:
			if err = ctx.Err(); err == nil {
				value, size, err = build(ctx)
			}
			<-t.builds
		case <-ctx.Done():
			err = ctx.Err()
		}
		if err == nil {
			err = ctx.Err()
		}
		if err == nil && (size < 0 || size > t.opts.MetadataBytes) {
			err = fmt.Errorf("metadata snapshot exceeds %d-byte budget; increase -metadata-mib", t.opts.MetadataBytes)
		}
		t.mu.Lock()
		if err == nil {
			for t.metaBytes+size > t.opts.MetadataBytes {
				var oldest *metaItem
				for _, item := range t.meta {
					if oldest == nil || item.seq < oldest.seq {
						oldest = item
					}
				}
				if oldest == nil {
					break
				}
				delete(t.meta, oldest.key)
				t.metaBytes -= oldest.bytes
			}
			t.seq++
			t.meta[key] = &metaItem{key: key, value: value, bytes: size, expires: time.Now().Add(ttl), seq: t.seq}
			t.metaBytes += size
		}
		flight.value, flight.err = value, err
		delete(t.sources, key)
		close(flight.done)
		t.mu.Unlock()
		return value, err
	}
}

func cloudKey(f *panapi.File) string { return fmt.Sprintf("cloud:%d:%s:%d", f.ID, f.Version, f.Size) }
func (n *Node) source(ctx context.Context) (*storage.Remote, error) {
	f := n.item.cloud
	key := cloudKey(f)
	t := n.tree
	value, err := t.loadMeta(ctx, "source:"+key, t.opts.SourceTTL, func(ctx context.Context) (any, int64, error) {
		r, err := storage.NewRemoteContext(t.ctx, ctx, t.cache, key, f.Size, func(ctx context.Context) (string, error) { return t.api.DownloadURL(ctx, f.ID) })
		if err != nil {
			return nil, 0, err
		}
		return r, int64(4096 + len(key) + len(r.Key())), nil
	})
	if err != nil {
		return nil, err
	}
	return value.(*storage.Remote), nil
}

func (n *Node) list(ctx context.Context) (map[string]*entry, error) {
	if !n.item.directory {
		return nil, syscall.ENOTDIR
	}
	if n.item.cloud != nil && !n.item.cloud.IsDir || n.item.source != nil {
		source := n.item.source
		var err error
		if source == nil {
			source, err = n.source(ctx)
			if err != nil {
				return nil, err
			}
		}
		size := n.item.archiveSize
		if size == 0 && n.item.cloud != nil {
			size = n.item.cloud.Size
		}
		idx, err := n.tree.getZIP(ctx, source, size)
		if err != nil {
			return nil, err
		}
		return idx.children(n.item.zipPath, source, size), nil
	}
	return n.listCloud(ctx, fmt.Sprintf("dir:%d", n.item.cloud.ID))
}

func (t *Tree) getZIP(ctx context.Context, source *storage.Remote, size int64) (*zipIndex, error) {
	value, err := t.loadMeta(ctx, "zip:"+source.Key(), t.opts.ZIPIndexTTL, func(ctx context.Context) (any, int64, error) {
		idx, err := buildZIP(ctx, t, source, size)
		if err != nil {
			return nil, 0, err
		}
		return idx, idx.bytes, nil
	})
	if err != nil {
		return nil, err
	}
	return value.(*zipIndex), nil
}

func (n *Node) listCloud(ctx context.Context, key string) (map[string]*entry, error) {
	value, err := n.tree.loadMeta(ctx, key, n.tree.opts.DirectoryTTL, func(ctx context.Context) (any, int64, error) {
		files, err := n.tree.api.List(ctx, n.item.cloud.ID)
		if err != nil {
			return nil, 0, err
		}
		if len(files) > n.tree.opts.MaxEntries {
			return nil, 0, fmt.Errorf("directory exceeds %d entries", n.tree.opts.MaxEntries)
		}
		result := make(map[string]*entry, len(files))
		size := int64(128)
		for _, f := range files {
			if err := ctx.Err(); err != nil {
				return nil, 0, err
			}
			if !validName(f.Name) {
				return nil, 0, errors.New("cloud directory contains an invalid filename")
			}
			if _, exists := result[f.Name]; exists {
				return nil, 0, errors.New("cloud directory contains duplicate names")
			}
			size += int64(256 + len(f.Name) + len(f.Version))
			if size > n.tree.opts.MetadataBytes {
				return nil, 0, fmt.Errorf("directory exceeds metadata budget; increase -metadata-mib")
			}
			ff := f
			result[f.Name] = &entry{name: f.Name, cloud: &ff, directory: f.IsDir || (n.tree.zipDirs && strings.HasSuffix(strings.ToLower(f.Name), ".zip"))}
		}
		return result, size, nil
	})
	if err != nil {
		return nil, err
	}
	return value.(map[string]*entry), nil
}

// ZIP index paths are represented by slash-separated components on the Node.
type zipIndex struct {
	root    *zipDir
	members map[string]*member
	bytes   int64
}
type zipDir struct {
	dirs  map[string]*zipDir
	files map[string]*member
}

func (z *zipIndex) children(path string, source *storage.Remote, archiveSize int64) map[string]*entry {
	dir := z.root
	if path != "" {
		for _, part := range strings.Split(path, "/") {
			dir = dir.dirs[part]
			if dir == nil {
				return map[string]*entry{}
			}
		}
	}
	out := make(map[string]*entry, len(dir.dirs)+len(dir.files))
	for k := range dir.dirs {
		childPath := k
		if path != "" {
			childPath = path + "/" + k
		}
		out[k] = &entry{name: k, directory: true, zipPath: childPath, source: source, archiveSize: archiveSize}
	}
	for k, m := range dir.files {
		copy := *m
		copy.file = nil
		copy.reader = nil
		out[k] = &entry{name: k, member: &copy, source: source, archiveSize: archiveSize}
	}
	return out
}

type contextZIPReaderAt struct {
	source  *storage.Remote
	gate    chan struct{}
	ctx     context.Context
	bounded bool
	left    int64
}

func (r *contextZIPReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if r.bounded {
		r.left -= int64(len(p))
		if r.left < 0 {
			return 0, errors.New("ZIP index exceeds 64 MiB read budget")
		}
	}
	return r.source.ReadAtContext(r.ctx, p, off)
}
func (r *contextZIPReaderAt) withContext(ctx context.Context, fn func() error) error {
	select {
	case r.gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-r.gate }()
	r.ctx = ctx
	defer func() { r.ctx = nil }()
	return fn()
}
func (r *contextZIPReaderAt) dataOffset(ctx context.Context, f *zip.File) (int64, error) {
	var off int64
	err := r.withContext(ctx, func() error { var e error; off, e = f.DataOffset(); return e })
	return off, err
}

func buildZIP(ctx context.Context, t *Tree, source *storage.Remote, size int64) (*zipIndex, error) {
	adapter := &contextZIPReaderAt{source: source, ctx: ctx, gate: make(chan struct{}, 1)}
	var zr *zip.Reader
	err := adapter.withIndexContext(ctx, func() error {
		if err := preflightZIP(adapter, size, t.opts.MaxZIPEntries, 64<<20); err != nil {
			return err
		}
		// preflight enforces the declared central-directory byte budget. Reset
		// the per-pass I/O cap because archive/zip then parses the same bytes.
		adapter.left = (64 << 20) + zipEOCDMaxTail + 256
		var e error
		zr, e = zip.NewReader(adapter, size)
		return e
	})
	if err != nil {
		return nil, err
	}
	if len(zr.File) > t.opts.MaxZIPEntries {
		return nil, fmt.Errorf("ZIP exceeds %d entries", t.opts.MaxZIPEntries)
	}
	root := &zipDir{dirs: map[string]*zipDir{}, files: map[string]*member{}}
	index := &zipIndex{root: root, members: map[string]*member{}}
	bytes := int64(256)
	nodes, names := 0, 0
	seen := map[string]bool{}
	for _, f := range zr.File {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		bytes += 384 + int64(len(f.Name)+len(f.Extra)+len(f.Comment))
		decoded, e := zipName(f)
		if e != nil {
			return nil, e
		}
		bytes += int64(len(decoded))
		name := strings.TrimSuffix(decoded, "/")
		if name == "" || seen[name] {
			return nil, errors.New("duplicate ZIP path")
		}
		seen[name] = true
		parts := strings.Split(name, "/")
		if len(parts) > t.opts.MaxDepth {
			return nil, fmt.Errorf("ZIP path exceeds depth %d", t.opts.MaxDepth)
		}
		dir := root
		for i, part := range parts {
			if !validName(part) {
				return nil, errors.New("unsafe or non-UTF8 ZIP path")
			}
			names += len(part)
			if names > t.opts.MaxNameBytes {
				return nil, fmt.Errorf("ZIP names exceed %d bytes", t.opts.MaxNameBytes)
			}
			last := i == len(parts)-1
			directory := !last || f.FileInfo().IsDir()
			if directory {
				if _, exists := dir.files[part]; exists {
					return nil, errors.New("conflicting ZIP paths")
				}
				child := dir.dirs[part]
				if child == nil {
					child = &zipDir{dirs: map[string]*zipDir{}, files: map[string]*member{}}
					dir.dirs[part] = child
					nodes++
					bytes += 96 + int64(len(part))
				}
				dir = child
			} else {
				if _, exists := dir.dirs[part]; exists {
					return nil, errors.New("conflicting ZIP paths")
				}
				if !f.Mode().IsRegular() || f.UncompressedSize64 > math.MaxInt64 {
					return nil, errors.New("unsupported ZIP entry type or size")
				}
				m := &member{file: f, reader: adapter, name: f.Name, method: f.Method, flags: f.Flags, crc: f.CRC32, compressed: f.CompressedSize64, size: f.UncompressedSize64}
				dir.files[part] = m
				index.members[f.Name] = m
				nodes++
				bytes += 256 + int64(len(part))
			}
			if bytes > t.opts.MetadataBytes {
				return nil, fmt.Errorf("ZIP index exceeds metadata budget; increase -metadata-mib")
			}
			if nodes > t.opts.MaxExpandedNodes {
				return nil, fmt.Errorf("ZIP expanded tree exceeds %d nodes", t.opts.MaxExpandedNodes)
			}
		}
	}
	index.bytes = bytes
	return index, nil
}
func (r *contextZIPReaderAt) withIndexContext(ctx context.Context, fn func() error) error {
	select {
	case r.gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-r.gate }()
	r.ctx = ctx
	r.bounded = true
	r.left = (64 << 20) + zipEOCDMaxTail + 256
	defer func() { r.ctx = nil; r.bounded = false }()
	return fn()
}

func (n *Node) mode() uint32 {
	if n.item.directory {
		return fuse.S_IFDIR | 0555
	}
	return fuse.S_IFREG | 0444
}
func (n *Node) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = n.mode()
	out.Nlink = 1
	if !n.item.directory {
		if n.item.member != nil {
			out.Size = n.item.member.size
		} else {
			out.Size = uint64(n.item.cloud.Size)
		}
	}
	out.Blksize = 4096
	out.Blocks = (out.Size + 511) / 512
	return 0
}
func (n *Node) Setattr(context.Context, fs.FileHandle, *fuse.SetAttrIn, *fuse.AttrOut) syscall.Errno {
	return syscall.EROFS
}
func (n *Node) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	entries, err := n.list(ctx)
	if err != nil {
		return nil, toErrno(err)
	}
	e, ok := entries[name]
	if !ok {
		return nil, syscall.ENOENT
	}
	child := &Node{tree: n.tree, item: e}
	var attr fuse.AttrOut
	child.Getattr(ctx, nil, &attr)
	out.Attr = attr.Attr
	hash := fnv.New64a()
	fmt.Fprintf(hash, "%d/%s", n.StableAttr().Ino, name)
	if e.cloud != nil {
		fmt.Fprintf(hash, ":%d:%s:%d", e.cloud.ID, e.cloud.Version, e.cloud.Size)
	}
	if e.source != nil {
		fmt.Fprint(hash, ":", e.source.Key())
	}
	if e.member != nil {
		fmt.Fprintf(hash, ":%s:%d:%d", e.member.name, e.member.crc, e.member.size)
	}
	ino := hash.Sum64()
	if ino < 2 {
		ino += 2
	}
	return n.NewInode(ctx, child, fs.StableAttr{Mode: child.mode() & syscall.S_IFMT, Ino: ino}), 0
}
func (n *Node) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	entries, err := n.list(ctx)
	if err != nil {
		return nil, toErrno(err)
	}
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]fuse.DirEntry, 0, len(names))
	for _, name := range names {
		mode := uint32(fuse.S_IFREG)
		if entries[name].directory {
			mode = fuse.S_IFDIR
		}
		result = append(result, fuse.DirEntry{Name: name, Mode: mode})
	}
	return fs.NewListDirStream(result), 0
}
func (n *Node) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if flags&(syscall.O_ACCMODE|syscall.O_TRUNC|syscall.O_APPEND|syscall.O_CREAT) != syscall.O_RDONLY {
		return nil, 0, syscall.EROFS
	}
	if n.item.directory {
		return nil, 0, syscall.EISDIR
	}
	if n.item.member == nil {
		source, err := n.source(ctx)
		if err != nil {
			return nil, 0, toErrno(err)
		}
		return &handle{remote: source, size: uint64(n.item.cloud.Size)}, fuse.FOPEN_DIRECT_IO, 0
	}
	m := n.item.member
	src := n.item.source
	if m.flags&1 != 0 || (m.method != zip.Store && m.method != zip.Deflate) {
		return nil, 0, syscall.EOPNOTSUPP
	}
	idx, err := n.tree.getZIP(ctx, src, n.item.archiveSize)
	if err != nil {
		return nil, 0, toErrno(err)
	}
	full := idx.members[m.name]
	if full == nil {
		return nil, 0, syscall.EIO
	}
	if m.method == zip.Store {
		if m.compressed != m.size {
			return nil, 0, syscall.EIO
		}
		offset, err := full.reader.dataOffset(ctx, full.file)
		if err != nil {
			return nil, 0, toErrno(err)
		}
		return &handle{remote: src, base: offset, size: m.size}, fuse.FOPEN_DIRECT_IO, 0
	}
	cached, err := n.tree.cache.Acquire(ctx, src.Key()+":member:"+m.name+fmt.Sprintf(":%08x:%d", m.crc, m.size), int64(m.size), func(ctx context.Context, w io.Writer) error { return inflateMember(ctx, src, full, w) })
	if err != nil {
		return nil, 0, toErrno(err)
	}
	return &handle{reader: cached, closer: cached, size: m.size}, fuse.FOPEN_DIRECT_IO, 0
}

type contextRemote struct {
	ctx    context.Context
	source *storage.Remote
}

func (r contextRemote) ReadAt(p []byte, off int64) (int, error) {
	return r.source.ReadAtContext(r.ctx, p, off)
}
func inflateMember(ctx context.Context, src *storage.Remote, m *member, w io.Writer) error {
	if m.compressed > math.MaxInt64 || m.size > math.MaxInt64 {
		return errors.New("ZIP member size exceeds supported range")
	}
	offset, err := m.reader.dataOffset(ctx, m.file)
	if err != nil {
		return err
	}
	section := io.NewSectionReader(contextRemote{ctx: ctx, source: src}, offset, int64(m.compressed))
	zr := flate.NewReader(section)
	defer zr.Close()
	h := crc32.NewIEEE()
	written, err := io.Copy(io.MultiWriter(w, h), &contextReader{ctx: ctx, r: zr})
	if err != nil {
		return err
	}
	if written != int64(m.size) || h.Sum32() != m.crc {
		return errors.New("ZIP member size or CRC mismatch")
	}
	return ctx.Err()
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

type handle struct {
	reader io.ReaderAt
	closer io.Closer
	remote *storage.Remote
	base   int64
	size   uint64
}

func (h *handle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
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
	var n int
	var err error
	if h.remote != nil {
		n, err = h.remote.ReadAtContext(ctx, dest, h.base+off)
	} else {
		n, err = h.reader.ReadAt(dest, off)
	}
	if err != nil && err != io.EOF {
		return nil, toErrno(err)
	}
	return fuse.ReadResultData(dest[:n]), 0
}
func (h *handle) Release(context.Context) syscall.Errno {
	if h.closer != nil {
		return toErrno(h.closer.Close())
	}
	return 0
}
func toErrno(err error) syscall.Errno {
	if err == nil {
		return 0
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno
	}
	if errors.Is(err, context.Canceled) {
		return syscall.EINTR
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return syscall.ETIMEDOUT
	}
	log.Printf("mount read: %v", err)
	return syscall.EIO
}
