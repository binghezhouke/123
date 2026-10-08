// Package mountfs exposes a read-only cloud tree and ZIP virtual directories.
package mountfs

import (
	"archive/zip"
	"compress/flate"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/binghezhouke/123/mount123/internal/workqueue"
	"hash/crc32"
	"hash/fnv"
	"io"
	"log"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/nwaples/rardecode/v2"
)

type API interface {
	List(context.Context, int64) ([]panapi.File, error)
	DownloadURL(context.Context, int64) (string, error)
}

// MetadataAPI is optional to keep small adapters compatible. The production
// client implements both methods for root validation and active inode refresh.
type MetadataAPI interface {
	Infos(context.Context, []int64) ([]panapi.File, error)
	Detail(context.Context, int64) (panapi.File, error)
}

type PasswordAPI interface {
	ReadSmallFile(context.Context, int64, int64) ([]byte, error)
}

// Options sets mount-local metadata and source reuse lifetimes. Zero values
// select documented defaults.
type Options struct {
	PrefetchFiles       int
	PrefetchWorkers     int
	PrefetchBytes       int64
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
	RefreshFileMetadata bool
}

func defaults(o Options) Options {
	if o.DirectoryTTL <= 0 {
		o.DirectoryTTL = 30 * time.Second
	}
	if o.SourceTTL <= 0 {
		o.SourceTTL = 30 * time.Second
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
	prefetch         *imagePrefetch
	ctx              context.Context
	api              API
	cache            *storage.Cache
	zipDirs          bool
	opts             Options
	mu               sync.Mutex
	meta             map[string]*metaItem
	metaBytes        int64
	seq              uint64
	builds           chan struct{}
	buildGate        *workqueue.Gate
	buildOnce        sync.Once
	sources          map[string]*sourceCall
	archiveTasks     map[string]*sourceCall
	indexStatuses    *indexStatusTracker
	refreshing       map[string]bool
	infoMu           sync.Mutex
	infoPending      []*infoRequest
	infoRunning      bool
	passwordKey      [32]byte
	passwordKeyValid bool
	cacheScope       string
}
type entry struct {
	name        string
	cloud       *panapi.File
	member      *member
	source      *storage.Remote
	zipPath     string
	archiveSize int64
	archive     *archiveDescriptor
	directory   bool
}
type member struct {
	rarLocator       *rardecode.MemberLocator
	format           string
	ordinal          int
	file             *zip.File
	reader           *contextZIPReaderAt
	name             string
	headerOffset     int64
	modifiedTime     uint16
	method           uint16
	flags            uint16
	crc              uint32
	compressed, size uint64
	encrypted        bool
	aes              *aesMemberInfo
}
type archiveDescriptor struct {
	id, parentID  int64
	name, version string
	size          int64
}
type protectedPassword struct{ nonce, ciphertext []byte }
type Node struct {
	parent *Node
	fs.Inode
	tree *Tree
	item *entry
	root bool
}
type metaItem struct {
	key     string
	value   any
	bytes   int64
	expires time.Time
	seq     uint64
}
type sourceCall struct {
	partial *zipIndex
	updated chan struct{}
	done    chan struct{}
	value   any
	err     error
}
type infoResult struct {
	file panapi.File
	err  error
}
type infoRequest struct {
	ctx    context.Context
	id     int64
	result chan infoResult
}

var errInfoNotFound = errors.New("file metadata not found or trashed")
var errDuplicateCloudName = errors.New("cloud directory contains duplicate names")

const infoBatchMax = 100
const infoQueueMax = 400
const infoBatchWindow = 5 * time.Millisecond

func New(ctx context.Context, api API, cache *storage.Cache, rootID int64, zipDirs bool) *Node {
	return NewWithOptions(ctx, api, cache, rootID, zipDirs, Options{})
}
func NewWithOptions(ctx context.Context, api API, cache *storage.Cache, rootID int64, zipDirs bool, opts Options) *Node {
	if ctx == nil {
		ctx = context.Background()
	}
	t := &Tree{ctx: ctx, api: api, cache: cache, zipDirs: zipDirs, opts: defaults(opts), meta: map[string]*metaItem{}, builds: make(chan struct{}, defaults(opts).MaxConcurrentBuilds), sources: map[string]*sourceCall{}, indexStatuses: newIndexStatusTracker()}
	if identity, ok := api.(interface{ CacheIdentity() string }); ok {
		t.cacheScope = identity.CacheIdentity()
	} else {
		// Test and adapter APIs can opt in to CacheIdentity. This fallback keeps
		// unrelated adapter types from sharing plaintext cache entries.
		t.cacheScope = fmt.Sprintf("%T", api)
	}
	_, secretErr := rand.Read(t.passwordKey[:])
	t.passwordKeyValid = secretErr == nil
	if opts.PrefetchFiles > 0 {
		t.prefetch = newImagePrefetch(t)
	}
	return &Node{tree: t, item: &entry{directory: true, cloud: &panapi.File{ID: rootID, IsDir: true}}, root: true}
}

// Prepare validates a nonzero root and caches its listing before mounting.
func (n *Node) Prepare(ctx context.Context) error {
	if n.item.cloud.ID != 0 {
		if api, ok := n.tree.api.(MetadataAPI); ok {
			id := n.item.cloud.ID
			value, err := n.tree.loadMeta(ctx, fmt.Sprintf("detail:%d", id), n.tree.opts.DirectoryTTL, func(ctx context.Context) (any, int64, error) {
				f, err := api.Detail(ctx, id)
				if err != nil {
					return nil, 0, err
				}
				return f, int64(512 + len(f.Name) + len(f.Version)), nil
			})
			if err != nil {
				return fmt.Errorf("root metadata: %w", err)
			}
			f := value.(panapi.File)
			if f.ID != id || !f.IsDir || f.Trashed {
				return errors.New("root ID is missing, trashed or not a directory")
			}
			n.item.cloud = &f
		}
	}
	_, err := n.list(ctx)
	return err
}

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
			if item.expires.IsZero() || time.Now().Before(item.expires) {
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
		release, err := t.acquireBuild(ctx)
		if err == nil {
			value, size, err = build(ctx)
			release()
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
			expires := time.Time{}
			if ttl > 0 {
				expires = time.Now().Add(ttl)
			}
			t.meta[key] = &metaItem{key: key, value: value, bytes: size, expires: expires, seq: t.seq}
			t.metaBytes += size
		}
		flight.value, flight.err = value, err
		delete(t.sources, key)
		close(flight.done)
		t.mu.Unlock()
		return value, err
	}
}

func (t *Tree) lookupCloudMetadata(ctx context.Context, snapshot *panapi.File) (*panapi.File, error) {
	api, ok := t.api.(MetadataAPI)
	if !ok {
		return snapshot, nil
	}
	key := fmt.Sprintf("info:%d:%s:%d:%t", snapshot.ID, snapshot.Version, snapshot.Size, snapshot.IsDir)
	value, err := t.loadMeta(ctx, key, t.opts.DirectoryTTL, func(ctx context.Context) (any, int64, error) {
		f, err := t.requestInfo(ctx, api, snapshot.ID)
		if err != nil {
			return nil, 0, err
		}
		if f.Trashed {
			return nil, 0, errInfoNotFound
		}
		if f.ID != snapshot.ID || f.Name != snapshot.Name || f.IsDir != snapshot.IsDir {
			return nil, 0, syscall.ESTALE
		}
		// Directory aggregates may vary between list and Infos. Keep the
		// immutable directory snapshot while accepting refreshed timestamps.
		if !snapshot.IsDir && (f.Size != snapshot.Size || f.Version != snapshot.Version) {
			return nil, 0, syscall.ESTALE
		}
		copy := *snapshot
		if !f.CreatedAt.IsZero() {
			copy.CreatedAt = f.CreatedAt
		}
		if !f.UpdatedAt.IsZero() {
			copy.UpdatedAt = f.UpdatedAt
		}
		return &copy, int64(512 + len(copy.Name) + len(copy.Version)), nil
	})
	if err != nil {
		return nil, err
	}
	return value.(*panapi.File), nil
}

func (t *Tree) cachedCloudMetadata(snapshot *panapi.File) *panapi.File {
	key := fmt.Sprintf("info:%d:%s:%d:%t", snapshot.ID, snapshot.Version, snapshot.Size, snapshot.IsDir)
	t.mu.Lock()
	defer t.mu.Unlock()
	item := t.meta[key]
	if item == nil || time.Now().After(item.expires) {
		return snapshot
	}
	t.seq++
	item.seq = t.seq
	f, ok := item.value.(*panapi.File)
	if !ok {
		return snapshot
	}
	return f
}

func (t *Tree) requestInfo(ctx context.Context, api MetadataAPI, id int64) (panapi.File, error) {
	req := &infoRequest{ctx: ctx, id: id, result: make(chan infoResult, 1)}
	t.infoMu.Lock()
	if len(t.infoPending) >= infoQueueMax {
		t.infoMu.Unlock()
		return panapi.File{}, errors.New("metadata refresh queue is full")
	}
	t.infoPending = append(t.infoPending, req)
	if !t.infoRunning {
		t.infoRunning = true
		go t.runInfoBatches(api)
	}
	t.infoMu.Unlock()
	select {
	case result := <-req.result:
		return result.file, result.err
	case <-ctx.Done():
		return panapi.File{}, ctx.Err()
	case <-t.ctx.Done():
		return panapi.File{}, t.ctx.Err()
	}
}

func (t *Tree) runInfoBatches(api MetadataAPI) {
	for {
		timer := time.NewTimer(infoBatchWindow)
		select {
		case <-timer.C:
		case <-t.ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			t.finishInfoQueue(t.ctx.Err())
			return
		}
		t.infoMu.Lock()
		if len(t.infoPending) == 0 {
			t.infoRunning = false
			t.infoMu.Unlock()
			return
		}
		n := len(t.infoPending)
		if n > infoBatchMax {
			n = infoBatchMax
		}
		batch := append([]*infoRequest(nil), t.infoPending[:n]...)
		t.infoPending = append([]*infoRequest(nil), t.infoPending[n:]...)
		t.infoMu.Unlock()
		active := batch[:0]
		idSet := map[int64]bool{}
		ids := make([]int64, 0, len(batch))
		for _, req := range batch {
			if req.ctx.Err() != nil {
				req.result <- infoResult{err: req.ctx.Err()}
				continue
			}
			active = append(active, req)
			if !idSet[req.id] {
				idSet[req.id] = true
				ids = append(ids, req.id)
			}
		}
		if len(ids) == 0 {
			continue
		}
		batchCtx, cancel := context.WithCancel(t.ctx)
		var remaining atomic.Int32
		remaining.Store(int32(len(active)))
		stops := make([]func() bool, 0, len(active))
		for _, req := range active {
			stops = append(stops, context.AfterFunc(req.ctx, func() {
				if remaining.Add(-1) == 0 {
					cancel()
				}
			}))
		}
		files, err := api.Infos(batchCtx, ids)
		for _, stop := range stops {
			stop()
		}
		cancel()
		byID := make(map[int64]panapi.File, len(files))
		for _, f := range files {
			byID[f.ID] = f
		}
		for _, req := range active {
			if err := req.ctx.Err(); err != nil {
				req.result <- infoResult{err: err}
				continue
			}
			if err != nil {
				req.result <- infoResult{err: err}
				continue
			}
			f, ok := byID[req.id]
			if !ok {
				req.result <- infoResult{err: errInfoNotFound}
				continue
			}
			req.result <- infoResult{file: f}
		}
	}
}

func (t *Tree) finishInfoQueue(err error) {
	t.infoMu.Lock()
	pending := t.infoPending
	t.infoPending = nil
	t.infoRunning = false
	t.infoMu.Unlock()
	for _, req := range pending {
		req.result <- infoResult{err: err}
	}
}

func cloudKey(f *panapi.File) string { return fmt.Sprintf("cloud:%d:%s:%d", f.ID, f.Version, f.Size) }
func (t *Tree) cloudCacheKey(f *panapi.File) string {
	return t.diskCacheScope() + ":" + cloudKey(f)
}
func (n *Node) source(ctx context.Context) (*storage.Remote, error) {
	f := n.item.cloud
	t := n.tree
	key := t.cloudCacheKey(f)
	ttl := t.opts.SourceTTL
	if t.zipDirs && f.Version != "" && (archiveKind(f.Name) == ".rar" || archiveKind(f.Name) == ".7z") {
		ttl = max(ttl, 6*24*time.Hour)
	}
	value, err := t.loadMeta(ctx, "source:"+key, ttl, func(ctx context.Context) (any, int64, error) {
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
		archive := n.item.archive
		if archive == nil && n.item.cloud != nil {
			archive = &archiveDescriptor{id: n.item.cloud.ID, parentID: n.item.cloud.ParentID, name: n.item.cloud.Name, version: n.item.cloud.Version, size: n.item.cloud.Size}
		}
		var idx *zipIndex
		if archive != nil && archiveKind(archive.name) != ".zip" {
			password, e := n.tree.otherPassword(ctx, archive)
			if e != nil {
				return nil, e
			}
			defer clear(password)
			idx, err = n.tree.otherIndex(ctx, source, archive, password)
		} else {
			idx, err = n.tree.getZIP(ctx, source, size, archive)
		}
		if err != nil {
			return nil, err
		}
		return idx.children(n.item.zipPath, source, size, archive), nil
	}
	return n.listCloud(ctx)
}

func (t *Tree) getZIP(ctx context.Context, source *storage.Remote, size int64, archive *archiveDescriptor) (*zipIndex, error) {
	return t.getZIPWithProgress(ctx, source, size, archive, nil)
}

func (t *Tree) getZIPWithProgress(ctx context.Context, source *storage.Remote, size int64, archive *archiveDescriptor, progress func(int64)) (*zipIndex, error) {
	identity := source.Key()
	if archive != nil {
		identity = archiveIdentity(source, archive)
	}
	var password []byte
	if archive != nil {
		if _, ok := t.api.(PasswordAPI); ok {
			if value, err := t.archivePassword(ctx, archive); err == nil {
				password = value
			}
		}
	}
	defer clear(password)
	passwordIdentity := t.passwordTag(archive, password)
	cacheKey := t.archiveIndexCacheKey(".zip", identity, archive, password)
	identityDigest := ""
	if t.cache != nil {
		identityDigest = t.cache.StableDigest("archive-index-identity-v1", identity)
	}
	value, err := t.loadMeta(ctx, "zip:"+t.diskCacheScope()+":"+identity+":"+passwordIdentity, t.opts.ZIPIndexTTL, func(ctx context.Context) (any, int64, error) {
		if idx, ok := t.loadPersistentArchiveIndex(ctx, cacheKey, ".zip", size, identityDigest); ok {
			idx.attachZIPReader(source)
			return idx, idx.bytes, nil
		}
		idx, err := buildZIPWithProgress(ctx, t, source, size, progress)
		if err != nil {
			return nil, 0, err
		}
		identityDigest := ""
		if t.cache != nil {
			identityDigest = t.cache.StableDigest("archive-index-identity-v1", identity)
		}
		t.persistArchiveIndex(ctx, cacheKey, ".zip", size, identityDigest, idx)
		return idx, idx.bytes, nil
	})
	if err != nil {
		return nil, err
	}
	return value.(*zipIndex), nil
}

// ZIP index paths are represented by slash-separated components on the Node.
type zipIndex struct {
	mu       sync.RWMutex
	changed  chan struct{}
	complete bool
	scanErr  error
	root     *zipDir
	members  map[string]*member
	bytes    int64
}
type zipDir struct {
	order []string
	dirs  map[string]*zipDir
	files map[string]*member
}

func (z *zipIndex) children(path string, source *storage.Remote, archiveSize int64, archive *archiveDescriptor) map[string]*entry {
	z.mu.RLock()
	defer z.mu.RUnlock()
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
		out[k] = &entry{name: k, directory: true, zipPath: childPath, source: source, archiveSize: archiveSize, archive: archive}
	}
	for k, m := range dir.files {
		copy := *m
		copy.file = nil
		copy.reader = nil
		out[k] = &entry{name: k, member: &copy, source: source, archiveSize: archiveSize, archive: archive}
	}
	return out
}

type contextZIPReaderAt struct {
	source   *storage.Remote
	gate     chan struct{}
	ctx      context.Context
	bounded  bool
	left     int64
	progress func(int64)
}

func (r *contextZIPReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if r.bounded {
		r.left -= int64(len(p))
		if r.left < 0 {
			return 0, errors.New("ZIP index exceeds 64 MiB read budget")
		}
	}
	n, err := r.source.ReadAtContext(r.ctx, p, off)
	if n > 0 && r.progress != nil {
		r.progress(off + int64(n))
	}
	return n, err
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
func (m *member) dataOffset(ctx context.Context) (int64, error) {
	if m.reader == nil || m.headerOffset < 0 {
		return 0, errors.New("ZIP local header offset is unavailable")
	}
	var offset int64
	err := m.reader.withContext(ctx, func() error {
		var header [30]byte
		if err := zipReadAt(m.reader, header[:], m.headerOffset); err != nil {
			return err
		}
		if binary.LittleEndian.Uint32(header[:]) != 0x04034b50 {
			return errors.New("ZIP local file header is invalid")
		}
		nameLen, extraLen := int64(binary.LittleEndian.Uint16(header[26:])), int64(binary.LittleEndian.Uint16(header[28:]))
		if m.headerOffset > math.MaxInt64-30-nameLen-extraLen {
			return syscall.EFBIG
		}
		offset = m.headerOffset + 30 + nameLen + extraLen
		return nil
	})
	return offset, err
}

func buildZIP(ctx context.Context, t *Tree, source *storage.Remote, size int64) (*zipIndex, error) {
	return buildZIPWithProgress(ctx, t, source, size, nil)
}

func buildZIPWithProgress(ctx context.Context, t *Tree, source *storage.Remote, size int64, progress func(int64)) (*zipIndex, error) {
	adapter := &contextZIPReaderAt{source: source, ctx: ctx, gate: make(chan struct{}, 1), progress: progress}
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
	var localOffsets []int64
	err = adapter.withIndexContext(ctx, func() error {
		var e error
		localOffsets, e = zipLocalHeaderOffsets(adapter, size, t.opts.MaxZIPEntries)
		return e
	})
	if err != nil {
		return nil, err
	}
	if len(localOffsets) != len(zr.File) {
		return nil, errors.New("ZIP local header index does not match central directory")
	}
	root := &zipDir{dirs: map[string]*zipDir{}, files: map[string]*member{}}
	index := &zipIndex{root: root, members: map[string]*member{}}
	bytes := int64(256)
	nodes, names := 0, 0
	seen := map[string]bool{}
	for fileIndex, f := range zr.File {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		aesInfo, hasAES, aesErr := parseAESExtra(f.Extra)
		if aesErr != nil {
			return nil, aesErr
		}
		if hasAES && f.Flags&1 == 0 {
			return nil, errors.New("WinZip AES entry is not marked encrypted")
		}
		if hasAES && f.Method != 99 {
			return nil, errors.New("WinZip AES entry has an inconsistent compression method")
		}
		if f.Method == 99 && !hasAES {
			return nil, syscall.EOPNOTSUPP
		}
		if f.Flags&0x40 != 0 {
			return nil, syscall.EOPNOTSUPP
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
				m := &member{file: f, reader: adapter, name: f.Name, headerOffset: localOffsets[fileIndex], modifiedTime: f.ModifiedTime, method: f.Method, flags: f.Flags, crc: f.CRC32, compressed: f.CompressedSize64, size: f.UncompressedSize64, encrypted: f.Flags&1 != 0}
				if hasAES {
					m.aes = &aesInfo
					m.method = aesInfo.method
				}
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
	index.complete = true
	index.changed = make(chan struct{})
	close(index.changed)
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
	if n.item.cloud != nil {
		file := n.item.cloud
		if !n.root && n.tree.opts.RefreshFileMetadata {
			file = n.tree.cachedCloudMetadata(file)
		}
		setFileTimes(&out.Attr, *file)
	}
	out.Blksize = 4096
	out.Blocks = (out.Size + 511) / 512
	return 0
}
func (n *Node) Setattr(context.Context, fs.FileHandle, *fuse.SetAttrIn, *fuse.AttrOut) syscall.Errno {
	return syscall.EROFS
}
func (n *Node) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	entries, pending, err := n.lookupEntries(ctx, name)
	if err != nil {
		return nil, toErrno(err)
	}
	e, ok := entries[name]
	if !ok && pending {
		return nil, syscall.EAGAIN
	}
	if !ok {
		return nil, syscall.ENOENT
	}
	if e.cloud != nil && n.tree.opts.RefreshFileMetadata {
		fresh, err := n.tree.lookupCloudMetadata(ctx, e.cloud)
		if errors.Is(err, errInfoNotFound) {
			return nil, syscall.ENOENT
		}
		if err != nil {
			return nil, toErrno(err)
		}
		copy := *e
		copy.cloud = fresh
		e = &copy
	}
	child := &Node{tree: n.tree, item: e, parent: n}
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

func setFileTimes(attr *fuse.Attr, file panapi.File) {
	updated := file.UpdatedAt
	if updated.IsZero() {
		updated = file.CreatedAt
	}
	setFuseTime(&attr.Mtime, &attr.Mtimensec, updated)
	setFuseTime(&attr.Ctime, &attr.Ctimensec, updated)
}
func setFuseTime(seconds *uint64, nanos *uint32, value time.Time) {
	if value.IsZero() {
		return
	}
	unix := value.Unix()
	if unix < 0 {
		return
	}
	*seconds = uint64(unix)
	*nanos = uint32(value.Nanosecond())
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
	if n.tree.prefetch != nil {
		done := n.tree.prefetch.foreground(n)
		defer done()
	}
	h, flagsOut, errno := n.openRaw(ctx, flags)
	if errno == 0 && n.tree.prefetch != nil && n.parent != nil && imageName(n.item.name) {
		return &imageHandle{FileHandle: h, node: n}, flagsOut, errno
	}
	return h, flagsOut, errno
}

func (n *Node) openRaw(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
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
	if n.item.member.format != "" {
		return n.openOtherArchive(ctx)
	}
	m := n.item.member
	src := n.item.source
	idx, err := n.tree.getZIP(ctx, src, n.item.archiveSize, n.item.archive)
	if err != nil {
		return nil, 0, toErrno(err)
	}
	full := idx.members[m.name]
	if full == nil {
		return nil, 0, syscall.EIO
	}
	if m.flags&1 != 0 {
		password, err := n.tree.passwordForArchive(ctx, n.item.archive)
		if err != nil {
			return nil, 0, toErrno(err)
		}
		defer clear(password)
		tag := n.tree.passwordTag(n.item.archive, password)
		key := n.tree.diskCacheScope() + ":" + src.Key() + ":.zip:encrypted-member:" + m.name + fmt.Sprintf(":%08x:%d:%s", m.crc, m.size, tag)
		cached, err := n.tree.cache.Acquire(ctx, key, int64(m.size), func(ctx context.Context, w io.Writer) error {
			release, err := n.tree.acquireBuild(ctx)
			if err != nil {
				return err
			}
			defer release()
			return encryptedMember(ctx, src, full, password, w)
		})
		if err != nil {
			if isPasswordError(err) {
				return nil, 0, syscall.EACCES
			}
			return nil, 0, toErrno(err)
		}
		return &handle{reader: cached, closer: cached, size: m.size}, fuse.FOPEN_DIRECT_IO, 0
	}
	if m.method != zip.Store && m.method != zip.Deflate {
		return nil, 0, syscall.EOPNOTSUPP
	}
	if m.method == zip.Store {
		if m.compressed != m.size {
			return nil, 0, syscall.EIO
		}
		offset, err := full.dataOffset(ctx)
		if err != nil {
			return nil, 0, toErrno(err)
		}
		return &handle{remote: src, base: offset, size: m.size}, fuse.FOPEN_DIRECT_IO, 0
	}
	cached, err := n.tree.cache.Acquire(ctx, n.tree.diskCacheScope()+":"+src.Key()+":.zip:member:"+m.name+fmt.Sprintf(":%08x:%d", m.crc, m.size), int64(m.size), func(ctx context.Context, w io.Writer) error {
		release, err := n.tree.acquireBuild(ctx)
		if err != nil {
			return err
		}
		defer release()
		return inflateMember(ctx, src, full, w)
	})
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
	offset, err := m.dataOffset(ctx)
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
