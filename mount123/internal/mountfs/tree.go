// Package mountfs exposes a read-only cloud tree and ZIP virtual directories.
package mountfs

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
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
type Tree struct {
	ctx     context.Context
	api     API
	cache   *storage.Cache
	zipDirs bool
}
type entry struct {
	name      string
	cloud     *panapi.File
	member    *zip.File
	source    *storage.Remote
	children  map[string]*entry
	directory bool
}
type Node struct {
	fs.Inode
	tree    *Tree
	item    *entry
	mu      sync.Mutex
	entries map[string]*entry
	expires time.Time
}

func New(ctx context.Context, api API, cache *storage.Cache, rootID int64, zipDirs bool) *Node {
	return &Node{tree: &Tree{ctx: ctx, api: api, cache: cache, zipDirs: zipDirs}, item: &entry{directory: true, cloud: &panapi.File{ID: rootID, IsDir: true}}}
}

// Prepare validates and caches the root listing before mounting.
func (n *Node) Prepare(ctx context.Context) error { _, err := n.list(ctx); return err }

func validName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\\x00") && utf8.ValidString(name)
}
func (n *Node) source() (*storage.Remote, error) {
	f := n.item.cloud
	return storage.NewRemote(n.tree.ctx, n.tree.cache, fmt.Sprintf("cloud:%d:%s:%d", f.ID, f.Version, f.Size), f.Size, func(ctx context.Context) (string, error) { return n.tree.api.DownloadURL(ctx, f.ID) })
}
func (n *Node) list(ctx context.Context) (map[string]*entry, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.entries != nil && (n.expires.IsZero() || time.Now().Before(n.expires)) {
		return n.entries, nil
	}
	if n.item.children != nil {
		n.entries = n.item.children
		return n.entries, nil
	}
	if !n.item.directory {
		return nil, syscall.ENOTDIR
	}
	if !n.item.cloud.IsDir {
		source, err := n.source()
		if err != nil {
			return nil, err
		}
		// Limit index I/O as well as the number of exposed entries.
		budget := &indexReader{source: source, left: 64 << 20}
		archive, err := zip.NewReader(budget, n.item.cloud.Size)
		if err != nil {
			return nil, err
		}
		budget.unlimited = true // ZIP members retain this ReaderAt after index parsing.
		entries, err := zipEntries(archive, source)
		if err != nil {
			return nil, err
		}
		n.entries = entries
		return n.entries, nil
	}
	files, err := n.tree.api.List(ctx, n.item.cloud.ID)
	if err != nil {
		return nil, err
	}
	result := make(map[string]*entry, len(files))
	for _, f := range files {
		if !validName(f.Name) {
			return nil, fmt.Errorf("cloud directory contains an invalid filename")
		}
		if _, exists := result[f.Name]; exists {
			return nil, fmt.Errorf("cloud directory contains duplicate names")
		}
		result[f.Name] = &entry{name: f.Name, cloud: &f, directory: f.IsDir || (n.tree.zipDirs && strings.HasSuffix(strings.ToLower(f.Name), ".zip"))}
	}
	n.entries = result
	n.expires = time.Now().Add(30 * time.Second)
	return result, nil
}

type indexReader struct {
	source    io.ReaderAt
	left      int64
	unlimited bool
}

func (r *indexReader) ReadAt(p []byte, off int64) (int, error) {
	if !r.unlimited {
		r.left -= int64(len(p))
		if r.left < 0 {
			return 0, fmt.Errorf("ZIP index exceeds 64 MiB read budget")
		}
	}
	return r.source.ReadAt(p, off)
}
func zipEntries(z *zip.Reader, source *storage.Remote) (map[string]*entry, error) {
	if len(z.File) > 100000 {
		return nil, fmt.Errorf("ZIP exceeds 100000 entries")
	}
	root := &entry{directory: true, children: map[string]*entry{}}
	seen := map[string]bool{}
	for _, f := range z.File {
		decoded, err := zipName(f)
		if err != nil {
			return nil, err
		}
		name := strings.TrimSuffix(decoded, "/")
		if seen[name] {
			return nil, fmt.Errorf("duplicate ZIP path")
		}
		seen[name] = true
		parts := strings.Split(name, "/")
		dir := root
		for i, part := range parts {
			if !validName(part) {
				return nil, fmt.Errorf("unsafe or non-UTF8 ZIP path")
			}
			last := i == len(parts)-1
			directory := !last || f.FileInfo().IsDir()
			child, exists := dir.children[part]
			if exists {
				if !directory || !child.directory {
					return nil, fmt.Errorf("conflicting ZIP paths")
				}
			} else {
				child = &entry{name: part, directory: directory, source: source}
				if directory {
					child.children = map[string]*entry{}
				} else {
					if !f.Mode().IsRegular() || f.UncompressedSize64 > math.MaxInt64 {
						return nil, fmt.Errorf("unsupported ZIP entry type or size")
					}
					child.member = f
				}
				dir.children[part] = child
			}
			dir = child
		}
	}
	return root.children, nil
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
			out.Size = n.item.member.UncompressedSize64
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
		fmt.Fprintf(hash, ":%d:%s", e.cloud.ID, e.cloud.Version)
	}
	if e.source != nil {
		fmt.Fprint(hash, e.source.Key())
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
		source, err := n.source()
		if err != nil {
			return nil, 0, toErrno(err)
		}
		return &handle{reader: source}, fuse.FOPEN_DIRECT_IO, 0
	}
	f := n.item.member
	if f.Flags&1 != 0 || (f.Method != zip.Store && f.Method != zip.Deflate) {
		return nil, 0, syscall.EOPNOTSUPP
	}
	if f.Method == zip.Store {
		offset, err := f.DataOffset()
		if err != nil {
			return nil, 0, toErrno(err)
		}
		if f.CompressedSize64 != f.UncompressedSize64 {
			return nil, 0, syscall.EIO
		}
		return &handle{reader: io.NewSectionReader(n.item.source, offset, int64(f.UncompressedSize64))}, fuse.FOPEN_DIRECT_IO, 0
	}
	cached, err := n.tree.cache.Acquire(ctx, n.item.source.Key()+":member:"+f.Name, int64(f.UncompressedSize64), func(ctx context.Context, w io.Writer) error {
		r, err := f.Open()
		if err != nil {
			return err
		}
		defer r.Close()
		_, err = io.Copy(w, &contextReader{ctx: ctx, r: r})
		return err
	})
	if err != nil {
		return nil, 0, toErrno(err)
	}
	return &handle{reader: cached, closer: cached}, fuse.FOPEN_DIRECT_IO, 0
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
}

func (h *handle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	if err := ctx.Err(); err != nil {
		return nil, toErrno(err)
	}
	n, err := h.reader.ReadAt(dest, off)
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
