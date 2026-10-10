package mountfs

import (
	"context"
	"errors"
	"fmt"
	"github.com/binghezhouke/123/mount123/internal/workqueue"
	"hash/crc32"
	"io"
	iofs "io/fs"
	"log"
	"math"
	"strings"
	"syscall"
	"time"

	"github.com/binghezhouke/123/mount123/internal/iostats"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/bodgit/sevenzip"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/nwaples/rardecode/v2"
)

// 7z does not expose whether an uncompressed member uses encryption. A CRC
// mismatch can therefore mean a missing/wrong password or damaged ciphertext.
var errArchiveIntegrity = errors.New("archive password or integrity check failed")

func archiveKind(name string) string {
	name = strings.ToLower(name)
	for _, kind := range []string{".zip", ".7z", ".rar"} {
		if strings.HasSuffix(name, kind) {
			return kind
		}
	}
	if strings.HasSuffix(name, ".7zz") || strings.HasSuffix(name, ".7z.001") {
		return ".7z"
	}
	return ""
}

// kind separates a detected format from the original cloud name used for
// password discovery and split-volume naming.
func (a *archiveDescriptor) kind() string {
	if a == nil {
		return ""
	}
	if a.format != "" {
		return a.format
	}
	return archiveKind(a.name)
}

// Only immutable member metadata enters the shared index. Readers, passwords,
// and decompressor state are scoped to one fill operation.
type archiveMember struct {
	encrypted   bool
	name        string
	size        uint64
	crc         uint32
	directory   bool
	ordinal     int
	rarLocator  *rardecode.MemberLocator
	sevenStream *sevenStreamLocation
}

type budgetReaderAt struct {
	r    io.ReaderAt
	left int64
}

func (r *budgetReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if int64(len(p)) > r.left {
		return 0, syscall.EFBIG
	}
	r.left -= int64(len(p))
	return r.r.ReadAt(p, off)
}

func archiveReadError(ctx context.Context, err error, password []byte) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno
	}
	var se *sevenzip.ReadError
	if errors.Is(err, errArchiveIntegrity) || (errors.As(err, &se) && se.Encrypted) || errors.Is(err, rardecode.ErrBadPassword) || errors.Is(err, rardecode.ErrArchiveEncrypted) || errors.Is(err, rardecode.ErrArchivedFileEncrypted) || len(password) > 0 {
		return syscall.EACCES
	}
	// Decoder errors can include archive-controlled text. Do not log it.
	return syscall.EIO
}

func (t *Tree) otherPassword(ctx context.Context, a *archiveDescriptor) ([]byte, error) {
	if _, ok := t.api.(PasswordAPI); !ok {
		return nil, nil
	}
	return t.archivePassword(ctx, a)
}

func rarOptions(password []byte) []rardecode.Option {
	options := []rardecode.Option{rardecode.MaxDictionarySize(64 << 20)}
	if len(password) > 0 {
		options = append(options, rardecode.Password(string(password)))
	}
	return options
}

func scanArchive(ctx context.Context, kind string, reader io.ReaderAt, size int64, password []byte, visit func(archiveMember) error) error {
	if kind == ".7z" {
		zr, err := sevenzip.NewReaderWithPassword(reader, size, string(password))
		if err != nil {
			return err
		}
		streams := zr.Streams()
		for i, f := range zr.File {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !f.FileInfo().IsDir() && !f.FileInfo().Mode().IsRegular() {
				return syscall.EOPNOTSUPP
			}
			var location *sevenStreamLocation
			if offset, ok := f.StreamOffset(); ok {
				if f.Stream < 0 || f.Stream >= len(streams) || streams[f.Stream].UncompressedSize > math.MaxInt64 {
					return syscall.EFBIG
				}
				location = &sevenStreamLocation{Stream: f.Stream, Offset: offset, Size: int64(streams[f.Stream].UncompressedSize)}
				if !location.valid(f.UncompressedSize) {
					return syscall.EIO
				}
			}
			if err := visit(archiveMember{name: f.Name, size: f.UncompressedSize, crc: f.CRC32, directory: f.FileInfo().IsDir(), ordinal: i, sevenStream: location}); err != nil {
				return err
			}
		}
		return nil
	}
	options := append(rarOptions(password), rardecode.FileSystem(singleArchiveFS{reader, size}), rardecode.BufferSize(512))
	i := 0
	return rardecode.WalkMembers("archive.rar", func(f *rardecode.FileHeader, locator rardecode.MemberLocator) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if f.UnKnownSize || f.UnPackedSize < 0 || (!f.IsDir && !f.Mode().IsRegular()) {
			return syscall.EOPNOTSUPP
		}
		err := visit(archiveMember{encrypted: f.Encrypted, name: f.Name, size: uint64(f.UnPackedSize), directory: f.IsDir, ordinal: i, rarLocator: &locator})
		i++
		return err
	}, options...)
}

// List walks packed headers without constructing a decoder. Reader.Next would
// decompress prior solid members even when the caller only wants their names.
type singleArchiveFS struct {
	reader io.ReaderAt
	size   int64
}

func (s singleArchiveFS) Open(name string) (iofs.File, error) {
	if name != "archive.rar" {
		return nil, iofs.ErrNotExist
	}
	return &archiveSectionFile{SectionReader: io.NewSectionReader(s.reader, 0, s.size)}, nil
}

type archiveSectionFile struct{ *io.SectionReader }

func (f *archiveSectionFile) Close() error { return nil }
func (f *archiveSectionFile) Stat() (iofs.FileInfo, error) {
	return archiveFileInfo{size: f.Size()}, nil
}

type archiveFileInfo struct{ size int64 }

func (f archiveFileInfo) Name() string        { return "archive.rar" }
func (f archiveFileInfo) Size() int64         { return f.size }
func (f archiveFileInfo) Mode() iofs.FileMode { return 0400 }
func (f archiveFileInfo) ModTime() time.Time  { return time.Time{} }
func (f archiveFileInfo) IsDir() bool         { return false }
func (f archiveFileInfo) Sys() any            { return nil }

type metadataRemote struct {
	reader  io.ReaderAt
	fileID  int64
	size    int64
	nextLog time.Time
}

func (r *metadataRemote) ReadAt(p []byte, off int64) (int, error) {
	if time.Now().After(r.nextLog) {
		log.Printf("archive header scan: file_id=%d offset=%d size=%d", r.fileID, off, r.size)
		r.nextLog = time.Now().Add(30 * time.Second)
	}
	return r.reader.ReadAt(p, off)
}

func (t *Tree) otherIndex(ctx context.Context, source *storage.Remote, a *archiveDescriptor, password []byte) (*zipIndex, error) {
	return t.otherIndexMode(ctx, source, a, password, false)
}
func (t *Tree) otherIndexMode(ctx context.Context, source *storage.Remote, a *archiveDescriptor, password []byte, progressive bool) (*zipIndex, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Completed single-volume indexes need neither a worker nor a CDN probe.
	if !strings.HasSuffix(strings.ToLower(a.name), ".7z.001") {
		cacheKey := "archive-index:" + a.kind() + ":" + archiveIdentity(source, a) + ":" + t.passwordTag(a, password)
		t.mu.Lock()
		if item := t.meta[cacheKey]; item != nil && time.Now().Before(item.expires) {
			t.seq++
			item.seq = t.seq
			idx := item.value.(*zipIndex)
			t.mu.Unlock()
			return idx, nil
		}
		t.mu.Unlock()
	}
	key := t.archiveTaskKey(source, a, password)
	password = append([]byte(nil), password...) // caller clears its buffer when the foreground request ends
	value, err := t.waitArchiveIndex(ctx, key, 5*time.Second, func(ctx context.Context) (any, error) {
		defer clear(password)
		return t.buildOtherIndex(ctx, source, a, password)
	}, progressive)
	if err != nil {
		return nil, err
	}
	return value.(*zipIndex), nil
}

// Slow header scans survive a foreground timeout, but stop with the mount.
// A bounded task map coalesces retries without creating a goroutine per waiter.
func (t *Tree) waitArchiveIndex(ctx context.Context, key string, wait time.Duration, build func(context.Context) (any, error), progressive ...bool) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	t.mu.Lock()
	if t.archiveTasks == nil {
		t.archiveTasks = make(map[string]*sourceCall)
	}
	call := t.archiveTasks[key]
	if call == nil {
		if len(t.archiveTasks) >= t.opts.MaxConcurrentBuilds {
			t.mu.Unlock()
			return nil, syscall.EAGAIN
		}
		call = &sourceCall{done: make(chan struct{}), updated: make(chan struct{})}
		t.archiveTasks[key] = call
		go func() {
			ctx, cancel := context.WithTimeout(workqueue.Background(t.ctx), 30*time.Minute)
			defer cancel()
			call.value, call.err = build(ctx)
			t.mu.Lock()
			delete(t.archiveTasks, key)
			close(call.done)
			t.mu.Unlock()
		}()
	}
	t.mu.Unlock()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		t.mu.Lock()
		partial, updated := call.partial, call.updated
		t.mu.Unlock()
		if len(progressive) > 0 && progressive[0] && partial != nil {
			return partial, nil
		}
		select {
		case <-call.done:
			return call.value, call.err
		case <-updated:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.ctx.Done():
			return nil, t.ctx.Err()
		case <-timer.C:
			return nil, syscall.EAGAIN
		}
	}
}

func (t *Tree) buildOtherIndex(ctx context.Context, source *storage.Remote, a *archiveDescriptor, password []byte) (*zipIndex, error) {
	started := time.Now()
	defer t.observeStage(iostats.StageArchiveIndex, started)
	statusKey := t.archiveTaskKey(source, a, password)
	t.indexStatuses.update(statusKey, func(s *ArchiveIndexStatus) {
		if s.State == "" {
			s.State = "queued"
		}
		s.ArchiveSize = a.size
	})
	reader, size, identity, err := t.archiveSource(ctx, source, a)
	if err != nil {
		t.indexStatuses.set(statusKey, failedArchiveStatus(a.size, err))
		return nil, err
	}
	if a.kind() == ".rar" {
		reader = &metadataRemote{reader: source.NewMetadataReader(ctx), fileID: a.id, size: size, nextLog: time.Now().Add(30 * time.Second)}
	}
	reader = &indexProgressReaderAt{r: reader, status: t.indexStatuses, key: statusKey, size: size}
	t.indexStatuses.update(statusKey, func(s *ArchiveIndexStatus) {
		s.State = "scanning"
		s.ArchiveSize = size
	})
	key := "archive-index:" + a.kind() + ":" + identity + ":" + t.passwordTag(a, password)
	kind := a.kind()
	persistKey := t.archiveIndexCacheKey(kind, identity, a, password)
	identityDigest := ""
	if t.cache != nil {
		identityDigest = t.cache.StableDigest("archive-index-identity-v1", identity)
	}
	value, err := t.loadMeta(ctx, key, 365*24*time.Hour, func(ctx context.Context) (any, int64, error) {
		if idx, ok := t.loadPersistentArchiveIndex(ctx, persistKey, kind, size, identityDigest); ok {
			return idx, idx.bytes, nil
		}
		started := time.Now()
		log.Printf("archive index started: file_id=%d format=%s", a.id, a.kind())
		idx := &zipIndex{root: &zipDir{dirs: map[string]*zipDir{}, files: map[string]*member{}}, members: map[string]*member{}, bytes: 256, changed: make(chan struct{})}
		t.mu.Lock()
		if call := t.archiveTasks[t.archiveTaskKey(source, a, password)]; call != nil {
			call.partial = idx
			close(call.updated)
			call.updated = nil
		}
		t.mu.Unlock()
		seen := map[string]bool{}
		// A checkpoint contains only the verified prefix. Reuse it as the
		// starting directory snapshot; the decoder continues to validate the
		// remaining headers and completion is still decided by this scan.
		if a.kind() == ".rar" {
			if partial, ok := t.loadArchiveCheckpoint(ctx, persistKey, kind, size, identityDigest); ok {
				idx = partial
				for name := range idx.members { seen[name] = true }
			}
		}
		nodes, names, entries := 0, 0, 0
		// Indexing has a separate read budget, so recursive find cannot decompress
		// an arbitrarily large solid RAR merely to discover names.
		readBudget := max(int64(64<<20), int64(t.opts.MaxZIPEntries)*512)
		bounded := &budgetReaderAt{r: reader, left: readBudget}
		err := scanArchive(ctx, a.kind(), bounded, size, password, func(f archiveMember) error {
			idx.mu.Lock()
			defer idx.mu.Unlock()
			entries++
			if entries > t.opts.MaxZIPEntries || f.size >= math.MaxInt64 {
				return syscall.EFBIG
			}
			name := strings.TrimSuffix(f.name, "/")
			if name == "" || seen[name] {
				return syscall.EIO
			}
			seen[name] = true
			parts := strings.Split(name, "/")
			if len(parts) > t.opts.MaxDepth {
				return syscall.EFBIG
			}
			dir := idx.root
			idx.bytes += int64(384 + len(name))
			if f.rarLocator != nil {
				idx.bytes += 32
			}
			for i, part := range parts {
				if !validName(part) {
					return syscall.EIO
				}
				names += len(part)
				if i < len(parts)-1 || f.directory {
					if dir.files[part] != nil {
						return syscall.EIO
					}
					if dir.dirs[part] == nil {
						dir.order = append(dir.order, part)
						dir.dirs[part] = &zipDir{dirs: map[string]*zipDir{}, files: map[string]*member{}}
						nodes++
						idx.bytes += int64(96 + len(part))
					}
					dir = dir.dirs[part]
				} else {
					if dir.dirs[part] != nil {
						return syscall.EIO
					}
					m := &member{name: f.name, size: f.size, crc: f.crc, ordinal: f.ordinal, rarLocator: f.rarLocator, sevenStream: f.sevenStream, format: a.kind()}
					dir.order = append(dir.order, part)
					dir.files[part] = m
					idx.members[f.name] = m
					nodes++
					idx.bytes += int64(256 + len(part))
				}
				if nodes > t.opts.MaxExpandedNodes || names > t.opts.MaxNameBytes || idx.bytes > t.opts.MetadataBytes {
					return syscall.EFBIG
				}
			}
			t.indexStatuses.update(statusKey, func(s *ArchiveIndexStatus) {
				s.Members = entries
				s.ArchiveSize = size
			})
			close(idx.changed)
			idx.changed = make(chan struct{})
			return nil
		})
		idx.mu.Lock()
		idx.complete = true
		idx.scanErr = archiveReadError(ctx, err, password)
		close(idx.changed)
		idx.mu.Unlock()
		if err != nil {
			if a.kind() == ".rar" {
				t.persistArchiveCheckpoint(ctx, persistKey, kind, size, identityDigest, idx, 0)
			}
			failure := failedArchiveStatus(size, archiveReadError(ctx, err, password))
			failure.Members = entries
			t.indexStatuses.set(statusKey, failure)
		} else {
			t.indexStatuses.set(statusKey, ArchiveIndexStatus{State: "complete", Members: entries, ScanOffset: size, ArchiveSize: size})
		}
		log.Printf("archive index finished: file_id=%d entries=%d elapsed=%s success=%t", a.id, entries, time.Since(started).Round(time.Millisecond), err == nil)
		if err != nil {
			return nil, 0, archiveReadError(ctx, err, password)
		}
		t.persistArchiveIndex(ctx, persistKey, kind, size, identityDigest, idx)
		return idx, idx.bytes, nil
	})
	if err != nil {
		return nil, err
	}
	return value.(*zipIndex), nil
}

func extractArchiveMember(ctx context.Context, reader io.ReaderAt, size int64, m *member, password []byte, w io.Writer) error {
	var stream io.Reader
	if m.format == ".7z" {
		zr, err := sevenzip.NewReaderWithPassword(reader, size, string(password))
		if err != nil {
			return err
		}
		if m.ordinal >= len(zr.File) {
			return syscall.ESTALE
		}
		f := zr.File[m.ordinal]
		if f.Name != m.name || f.UncompressedSize != m.size || f.CRC32 != m.crc {
			return syscall.ESTALE
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		defer rc.Close()
		stream = rc
	} else if m.rarLocator != nil && m.rarLocator.Direct() {
		options := append(rarOptions(password), rardecode.FileSystem(singleArchiveFS{reader, size}))
		h, rc, err := rardecode.OpenMember("archive.rar", *m.rarLocator, options...)
		if err != nil {
			return err
		}
		defer rc.Close()
		if h.Name != m.name || h.UnPackedSize < 0 || uint64(h.UnPackedSize) != m.size {
			return syscall.ESTALE
		}
		stream = rc
	} else {
		rr, err := rardecode.NewReader(io.NewSectionReader(reader, 0, size), rarOptions(password)...)
		if err != nil {
			return err
		}
		for i := 0; i <= m.ordinal; i++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			f, err := rr.Next()
			if err != nil {
				return err
			}
			if i == m.ordinal && (f.Name != m.name || f.UnPackedSize < 0 || uint64(f.UnPackedSize) != m.size) {
				return syscall.ESTALE
			}
		}
		stream = rr
	}
	hash := crc32.NewIEEE()
	count, err := io.CopyBuffer(io.MultiWriter(w, hash), io.LimitReader(&contextReader{ctx: ctx, r: stream}, int64(m.size)+1), make([]byte, 64<<10))
	if err != nil {
		return err
	}
	if count != int64(m.size) {
		return syscall.EIO
	}
	if m.format == ".7z" && hash.Sum32() != m.crc {
		return errArchiveIntegrity
	}
	return nil
}

func (n *Node) openOtherArchive(ctx context.Context) (fs.FileHandle, uint32, syscall.Errno) {
	t, a, m := n.tree, n.item.archive, n.item.member
	password, err := t.otherPassword(ctx, a)
	if err != nil {
		return nil, 0, toErrno(err)
	}
	defer clear(password)
	_, _, identity, err := t.archiveSource(ctx, n.item.source, a)
	if err != nil {
		return nil, 0, toErrno(err)
	}
	key := t.diskCacheScope() + ":" + identity + ":" + a.kind() + ":archive-member:" + m.name + fmt.Sprintf(":%d:%08x:", m.size, m.crc) + t.passwordTag(a, password)
	protected, err := t.encryptPassword(key, password)
	if err != nil {
		return nil, 0, toErrno(err)
	}
	if growing, err := n.acquireGrowingMember(ctx, key, m.size, func(fillCtx context.Context, w io.Writer) error {
		streamPassword, err := t.decryptPassword(key, protected)
		if err != nil {
			return err
		}
		defer clear(streamPassword)
		err = t.extractOtherMember(fillCtx, n.item.source, a, m, streamPassword, w)
		return archiveReadError(fillCtx, err, streamPassword)
	}); err != nil {
		return nil, 0, toErrno(err)
	} else if growing != nil {
		return &handle{growing: growing, closer: growing, size: m.size}, n.tree.growingCacheOpenFlags(growing), 0
	}
	cached, err := t.cache.Acquire(ctx, key, int64(m.size), func(ctx context.Context, w io.Writer) error {
		err := t.extractOtherMember(ctx, n.item.source, a, m, password, w)
		return archiveReadError(ctx, err, password)
	})
	if err != nil {
		return nil, 0, toErrno(err)
	}
	return &handle{reader: cached, closer: cached, size: m.size}, t.archiveCacheOpenFlags(), 0
}
