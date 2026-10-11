package mountfs

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/bodgit/sevenzip"
	"github.com/hanwen/go-fuse/v2/fs"
	"io"
	"runtime"
	"strings"
	"sync"
	"syscall"
)

// nestedArchive is created only after an explicit Lookup of an archive member.
// Copy/Store outer members are exposed through a bounded offset ReaderAt.
// Compressed outer members use the bounded cache fallback; neither path is
// activated by Readdir, only by explicit Lookup.
type nestedArchive struct {
	mu          sync.Mutex
	reader      io.ReaderAt
	size        int64
	format      string
	index       *zipIndex
	password    []byte
	cachePrefix string
	// handles pins fallback members in the cache while the virtual archive is
	// reachable. Copy/Store members do not need a pin because they read from the
	// outer source directly. The finalizer is a last-resort cleanup for FUSE
	// inode lifetimes, which do not expose a corresponding Go release hook.
	handles     []*storage.Handle
	lazyReaders []*nestedMemberReader
	closed      bool
}

// Close releases cache pins held by compressed outer members. It is
// deliberately idempotent so callers and the finalizer can both invoke it.
func (n *nestedArchive) Close() error {
	if n == nil {
		return nil
	}
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return nil
	}
	n.closed = true
	handles := n.handles
	n.handles = nil
	password := n.password
	n.password = nil
	n.mu.Unlock()
	var first error
	for _, h := range handles {
		if err := h.Close(); err != nil && first == nil {
			first = err
		}
	}
	for _, r := range n.lazyReaders {
		if err := r.Close(); err != nil && first == nil {
			first = err
		}
	}
	clear(password)
	return first
}

type nestedConcatReader struct {
	parts []io.ReaderAt
	sizes []int64
	total int64
}

func (r *nestedConcatReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, syscall.EINVAL
	}
	if off >= r.total {
		return 0, io.EOF
	}
	if int64(len(p)) > r.total-off {
		p = p[:int(r.total-off)]
	}
	read := 0
	for i, size := range r.sizes {
		if off >= size {
			off -= size
			continue
		}
		n, err := r.parts[i].ReadAt(p, off)
		read += n
		p = p[n:]
		if err != nil && !(err == io.EOF && n > 0) {
			return read, err
		}
		if len(p) == 0 {
			return read, nil
		}
		off = 0
	}
	return read, io.EOF
}

// nestedOffsetReader maps a member's uncompressed stream to its packed bytes.
// It is safe only when the outer 7z coder is Copy/Store; compressed streams
// continue through the bounded cache fallback below.
type nestedOffsetReader struct {
	r           io.ReaderAt
	start, size int64
}

func (r nestedOffsetReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= r.size {
		if off == r.size && len(p) == 0 {
			return 0, nil
		}
		return 0, io.EOF
	}
	truncated := int64(len(p)) > r.size-off
	if truncated {
		p = p[:int(r.size-off)]
	}
	n, err := r.r.ReadAt(p, r.start+off)
	if (n < len(p) || truncated) && err == nil {
		err = io.EOF
	}
	return n, err
}

func nestedCopyMemberReader(reader io.ReaderAt, size int64, outer *member, ordinal int, password []byte) (io.ReaderAt, bool, error) {
	// PackedRanges points at the encrypted packed bytes when the outer 7z
	// uses 7zAES. They look like a valid Copy stream, but mapping them
	// directly would expose ciphertext. Keep the decoder path whenever a
	// password is involved; it also remains correct for shared-password
	// metadata on an otherwise unencrypted archive.
	if len(password) > 0 {
		return nil, false, nil
	}
	zr, err := sevenzip.NewReaderWithPassword(reader, size, string(password))
	if err != nil {
		return nil, false, err
	}
	if ordinal < 0 || ordinal >= len(zr.File) {
		return nil, false, syscall.ESTALE
	}
	f := zr.File[ordinal]
	if f.Name != outer.name || f.UncompressedSize != outer.size {
		return nil, false, syscall.ESTALE
	}
	if outer.sevenStream == nil {
		return nil, false, nil
	}
	ranges, err := zr.PackedRanges(outer.sevenStream.Stream)
	if err != nil || len(ranges) != 1 {
		return nil, false, nil
	}
	streams := zr.Streams()
	if outer.sevenStream.Stream < 0 || outer.sevenStream.Stream >= len(streams) {
		return nil, false, nil
	}
	stream := streams[outer.sevenStream.Stream]
	if ranges[0].Size != int64(stream.UncompressedSize) || outer.sevenStream.Size != int64(stream.UncompressedSize) {
		return nil, false, nil
	}
	start := ranges[0].Offset + outer.sevenStream.Offset
	if start < ranges[0].Offset || start > ranges[0].Offset+ranges[0].Size || int64(outer.size) > ranges[0].Size-(start-ranges[0].Offset) {
		return nil, false, nil
	}
	return nestedOffsetReader{r: reader, start: start, size: int64(outer.size)}, true, nil
}

// NestedArchivePolicy keeps recursive archive expansion deliberately bounded.
// The policy is consulted by the explicit nested-entry path; ordinary
// directory enumeration must never call it.
type nestedArchivePolicy struct {
	MaxDepth      int
	MaxBytes      int64
	MaxMembers    int
	MaxIndexBytes int64
}

var defaultNestedArchivePolicy = nestedArchivePolicy{
	MaxDepth: 1, MaxBytes: 64 << 30, MaxMembers: 100000, MaxIndexBytes: 64 << 20,
}

func (p nestedArchivePolicy) allow(depth int, size int64) error {
	if depth > p.MaxDepth {
		return syscall.ELOOP
	}
	if size < 0 || size > p.MaxBytes {
		return syscall.EFBIG
	}
	return nil
}

// boundedReaderAt limits a nested archive view to the member's advertised
// size. It is useful when a decompressed member is backed by a shared cache
// handle: archive parsers cannot accidentally read past the member boundary.
type boundedReaderAt struct {
	r    io.ReaderAt
	size int64
}

func (r boundedReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, syscall.EINVAL
	}
	if off >= r.size {
		return 0, io.EOF
	}
	if int64(len(p)) > r.size-off {
		p = p[:int(r.size-off)]
	}
	n, err := r.r.ReadAt(p, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, err
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// indexNested7z is the narrow seam used by the explicit nested-entry path.
// It performs only header/index work against the supplied ReaderAt; callers
// must obtain that reader from the outer member on an explicit lookup. Keeping
// this operation separate prevents ordinary Readdir from accidentally opening
// an inner archive.
func indexNested7z(ctx context.Context, reader io.ReaderAt, size int64, password []byte, policy nestedArchivePolicy) ([]archiveMember, error) {
	if err := policy.allow(1, size); err != nil {
		return nil, err
	}
	result := make([]archiveMember, 0, 128)
	err := scanArchive(ctx, ".7z", boundedReaderAt{r: reader, size: size}, size, password, func(m archiveMember) error {
		if len(result) >= policy.MaxMembers {
			return syscall.EFBIG
		}
		result = append(result, m)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// indexNestedRAR performs the same bounded header scan for an inner RAR.
// The reader is normally a lazy view over a compressed outer 7z member, so
// scanning headers does not materialize the entire outer archive.
func indexNestedRAR(ctx context.Context, reader io.ReaderAt, size int64, password []byte, policy nestedArchivePolicy) ([]archiveMember, error) {
	if err := policy.allow(1, size); err != nil {
		return nil, err
	}
	result := make([]archiveMember, 0, 128)
	err := scanArchive(ctx, ".rar", boundedReaderAt{r: reader, size: size}, size, password, func(m archiveMember) error {
		if len(result) >= policy.MaxMembers {
			return syscall.EFBIG
		}
		result = append(result, m)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func indexNestedZIP(reader io.ReaderAt, size int64, policy nestedArchivePolicy) ([]archiveMember, error) {
	if err := policy.allow(1, size); err != nil {
		return nil, err
	}
	zr, err := zip.NewReader(reader, size)
	if err != nil {
		return nil, err
	}
	if len(zr.File) > policy.MaxMembers {
		return nil, syscall.EFBIG
	}
	result := make([]archiveMember, 0, len(zr.File))
	for i, f := range zr.File {
		name, err := zipName(f)
		if err != nil {
			return nil, err
		}
		name = strings.TrimSuffix(name, "/")
		if name == "" {
			continue
		}
		result = append(result, archiveMember{
			name: name, size: f.UncompressedSize64, crc: f.CRC32,
			directory: f.FileInfo().IsDir(), ordinal: i, zipFile: f,
			encrypted: f.Flags&1 != 0,
		})
	}
	return result, nil
}

func nestedIndexFromMembers(members []archiveMember, policy nestedArchivePolicy) (*zipIndex, error) {
	idx := &zipIndex{root: &zipDir{dirs: map[string]*zipDir{}, files: map[string]*member{}}, members: map[string]*member{}, bytes: 256, complete: true, changed: make(chan struct{})}
	for _, f := range members {
		name := strings.TrimSuffix(f.name, "/")
		if name == "" {
			return nil, syscall.EIO
		}
		parts := strings.Split(name, "/")
		idx.bytes += int64(256 + len(name))
		if idx.bytes > policy.MaxIndexBytes {
			return nil, syscall.EFBIG
		}
		dir := idx.root
		for i, part := range parts {
			if !validName(part) {
				return nil, syscall.EIO
			}
			if i < len(parts)-1 || f.directory {
				if dir.files[part] != nil {
					return nil, syscall.EIO
				}
				if dir.dirs[part] == nil {
					dir.order = append(dir.order, part)
					dir.dirs[part] = &zipDir{dirs: map[string]*zipDir{}, files: map[string]*member{}}
				}
				dir = dir.dirs[part]
				continue
			}
			if dir.dirs[part] != nil || dir.files[part] != nil {
				return nil, syscall.EIO
			}
			format := ".7z"
			if f.rarLocator != nil {
				format = ".rar"
			}
			m := &member{name: f.name, size: f.size, crc: f.crc, ordinal: f.ordinal, format: format, rarLocator: f.rarLocator, sevenStream: f.sevenStream}
			if f.zipFile != nil {
				m.format = ".zip"
				m.file = f.zipFile
				m.flags = f.zipFile.Flags
				m.method = f.zipFile.Method
				m.encrypted = f.encrypted
			}
			dir.order = append(dir.order, part)
			dir.files[part] = m
			idx.members[f.name] = m
		}
	}
	close(idx.changed)
	return idx, nil
}

func (n *Node) expandNested(ctx context.Context, e *entry) (*entry, error) {
	if e == nil || e.member == nil || e.nested != nil {
		return e, nil
	}
	kind := archiveKind(e.member.name)
	if kind != ".7z" && kind != ".rar" {
		return e, nil
	}
	if n.item == nil {
		return e, nil
	}
	archive := n.item.archive
	if archive == nil {
		archive = e.archive
	}
	if archive == nil {
		return e, nil
	}
	source := n.item.source
	if source == nil {
		source = e.source
	}
	if source == nil || n.tree.cache == nil {
		return e, nil
	}
	na, err := n.tree.buildNestedArchive(ctx, source, archive, nil, e.member.name, e.member)
	if err != nil {
		return nil, err
	}
	// Nodes are owned by the FUSE inode tree and may not receive a Go-level
	// release callback. Keep the readers alive until the archive becomes
	// unreachable during unmount or inode reclamation.
	runtime.SetFinalizer(na, func(a *nestedArchive) { _ = a.Close() })
	copy := *e
	copy.directory = true
	copy.member = nil
	copy.nested = na
	copy.archiveSize = na.size
	return &copy, nil
}

func (n *Node) openNestedMember(ctx context.Context) (fs.FileHandle, uint32, syscall.Errno) {
	na, m := n.item.nested, n.item.member
	if na == nil || m == nil || na.reader == nil {
		return nil, 0, syscall.EIO
	}
	if n.tree.cache == nil {
		return nil, 0, syscall.EIO
	}
	key := n.item.nested.cachePrefix + ":" + m.name + fmt.Sprintf(":%d:%08x", m.size, m.crc)
	cached, err := n.tree.cache.Acquire(ctx, key, int64(m.size), func(fillCtx context.Context, w io.Writer) error {
		return archiveReadError(fillCtx, extractArchiveMember(fillCtx, na.reader, na.size, m, na.password, w), na.password)
	})
	if err != nil {
		return nil, 0, toErrno(err)
	}
	return &handle{reader: cached, closer: cached, size: m.size}, n.tree.archiveCacheOpenFlags(), 0
}
