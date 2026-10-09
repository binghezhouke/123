package mountfs

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/binghezhouke/123/mount123/internal/workqueue"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/nwaples/rardecode/v2"
)

const (
	probeControlName       = ".mount123-probe"
	probeStatusControlName = ".mount123-probe-status"
	maxProbeFiles          = 1000
	probeReadBudget        = 1 << 20
)

func isDirectoryControlName(name string) bool {
	return name == refreshControlName || name == probeControlName || name == probeStatusControlName
}

// Results contain no source URL, password or archive-controlled error text.
type archiveProbeResult struct {
	ID      int64     `json:"id"`
	Version string    `json:"content_version"`
	Size    int64     `json:"size"`
	Updated time.Time `json:"updated_at"`
	Kind    string    `json:"format,omitempty"`
	State   string    `json:"state"`
}

func (r archiveProbeResult) matches(f panapi.File) bool {
	return r.ID == f.ID && r.Version == f.Version && r.Size == f.Size && r.Updated.Equal(f.UpdatedAt)
}

type archiveProbeRecord struct {
	Version  int                  `json:"version"`
	ParentID int64                `json:"parent_id"`
	Created  time.Time            `json:"created_at"`
	Results  []archiveProbeResult `json:"results"`
}

// ArchiveProbeStatus is a snapshot of an explicitly requested directory scan.
type ArchiveProbeStatus struct {
	State       string `json:"state"`
	Total       int    `json:"total"`
	Checked     int    `json:"checked"`
	Detected    int    `json:"detected"`
	Cached      int    `json:"cached"`
	Unknown     int    `json:"unknown"`
	Failed      int    `json:"failed"`
	Skipped     int    `json:"skipped"`
	Persistence string `json:"persistence"`
}

type archiveProbeJob struct{ status ArchiveProbeStatus }

func probeCandidate(f panapi.File) bool {
	return !f.IsDir && !f.Trashed && f.Size >= 8 && archiveKind(f.Name) == "" && !strings.HasSuffix(strings.ToLower(f.Name), ".pwd") && !strings.HasSuffix(strings.ToLower(f.Name), ".iso")
}

func (t *Tree) probeRecordKey(parentID int64) string {
	return fmt.Sprintf("archive-probes:%s:%d", t.diskCacheScope(), parentID)
}

// Serialize record replacement with disk restoration; an old load cannot
// publish over a just-finished scan. Records remain in the bounded meta cache.
func (t *Tree) loadProbeRecord(ctx context.Context, parentID int64) (*archiveProbeRecord, error) {
	t.probeRecordMu.Lock()
	defer t.probeRecordMu.Unlock()
	key := t.probeRecordKey(parentID)
	value, err := t.loadMeta(ctx, key, 365*24*time.Hour, func(ctx context.Context) (any, int64, error) {
		empty := &archiveProbeRecord{Version: 1, ParentID: parentID}
		if t.cache == nil || !t.cacheScopeStable || t.cache.IsEphemeral() {
			return empty, 128, nil
		}
		h, err := t.cache.OpenArchiveIndex(key)
		if err != nil {
			return empty, 128, nil
		}
		defer h.Close()
		if h.Size() <= 0 || h.Size() > min(t.opts.MetadataBytes/8, 1<<20) {
			return empty, 128, nil
		}
		data, err := io.ReadAll(io.NewSectionReader(h, 0, h.Size()))
		var record archiveProbeRecord
		if err != nil || json.Unmarshal(data, &record) != nil || record.Version != 1 || record.ParentID != parentID || len(record.Results) > maxProbeFiles || record.Created.IsZero() {
			return empty, 128, nil
		}
		for _, result := range record.Results {
			if result.Version == "" || (result.Kind != "" && result.Kind != ".zip" && result.Kind != ".7z" && result.Kind != ".rar") {
				return empty, 128, nil
			}
		}
		return &record, int64(len(data))*2 + 128, nil
	})
	if err != nil {
		return nil, err
	}
	return value.(*archiveProbeRecord), nil
}

func (t *Tree) saveProbeRecord(ctx context.Context, record *archiveProbeRecord) string {
	t.probeRecordMu.Lock()
	defer t.probeRecordMu.Unlock()
	key := t.probeRecordKey(record.ParentID)
	data, err := json.Marshal(record)
	if err != nil || int64(len(data)) > min(t.opts.MetadataBytes/8, 1<<20) {
		return "failed"
	}
	persistence := "memory"
	if t.cache != nil && t.cacheScopeStable && !t.cache.IsEphemeral() {
		durable := *record
		durable.Results = nil
		for _, result := range record.Results {
			// Without an API content version, do not trust a durable detection
			// just because an ID and size still match after a restart.
			if result.Version != "" {
				durable.Results = append(durable.Results, result)
			}
		}
		encoded, _ := json.Marshal(durable)
		if err := t.cache.ReplaceArchiveIndex(ctx, key, encoded); err != nil {
			persistence = "failed"
		} else {
			persistence = "disk"
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	oldBytes := int64(0)
	if old := t.meta[key]; old != nil {
		oldBytes = old.bytes
	}
	size := int64(len(data))*2 + 128
	if !t.makeMetadataRoomLocked(key, oldBytes, size) {
		return "failed"
	}
	t.seq++
	t.meta[key] = &metaItem{key: key, value: record, bytes: size, expires: time.Now().Add(365 * 24 * time.Hour), seq: t.seq}
	t.metaBytes += size - oldBytes
	return persistence
}

// Overlay immutable aliases onto a directory snapshot. No network probing is
// performed by this path, including on restart, Lookup or directory traversal.
func (t *Tree) withProbedArchives(ctx context.Context, parentID int64, base *cloudDirectory) (*cloudDirectory, error) {
	if !t.zipDirs {
		return base, nil
	}
	record, err := t.loadProbeRecord(ctx, parentID)
	if err != nil {
		return nil, err
	}
	if len(record.Results) == 0 {
		return base, nil
	}
	key := fmt.Sprintf("probe-directory:%d:%d:%d", parentID, base.generation, record.Created.UnixNano())
	value, err := t.loadMeta(ctx, key, t.opts.DirectoryTTL, func(context.Context) (any, int64, error) {
		copy := *base
		copy.entries = make(map[string]*entry, len(base.entries)+len(record.Results))
		copy.byName = make(map[string]panapi.File, len(base.byName)+len(record.Results))
		copy.names = append([]string(nil), base.names...)
		reserved := make(map[string]bool, len(base.entries)+len(record.Results))
		for name, item := range base.entries {
			copy.entries[name] = item
			copy.byName[name] = base.byName[name]
			reserved[name] = true
		}
		byID := make(map[int64]archiveProbeResult, len(record.Results))
		for _, result := range record.Results {
			if result.Kind != "" && result.State == "detected" {
				byID[result.ID] = result
			}
		}
		for _, f := range base.files {
			result, ok := byID[f.ID]
			if !ok || !result.matches(f) || !probeCandidate(f) {
				continue
			}
			name := f.Name + result.Kind
			if reserved[name] || isDirectoryControlName(name) {
				alias := f
				alias.Name = name
				name = cloudDuplicateName(alias, reserved)
			}
			reserved[name] = true
			file := f
			copy.entries[name] = &entry{name: name, cloud: &file, directory: true, archive: &archiveDescriptor{id: f.ID, parentID: parentID, name: f.Name, version: f.Version, size: f.Size, format: result.Kind}}
			copy.byName[name] = f
			copy.names = append(copy.names, name)
			copy.bytes += int64(600 + len(name) + len(f.Name) + len(f.Version))
		}
		sort.Strings(copy.names)
		copy.generation = cloudDirectoryGeneration.Add(1)
		return &copy, copy.bytes, nil
	})
	if err != nil {
		return nil, err
	}
	return value.(*cloudDirectory), nil
}

func (n *Node) probeStatus() ArchiveProbeStatus {
	n.tree.probeMu.Lock()
	defer n.tree.probeMu.Unlock()
	if job := n.tree.probeJobs[n.item.cloud.ID]; job != nil {
		return job.status
	}
	return ArchiveProbeStatus{State: "idle"}
}

func (n *Node) startArchiveProbe(ctx context.Context) (ArchiveProbeStatus, error) {
	if err := ctx.Err(); err != nil {
		return ArchiveProbeStatus{}, err
	}
	if err := n.tree.ctx.Err(); err != nil {
		return ArchiveProbeStatus{}, err
	}
	if !n.tree.zipDirs || n.tree.cache == nil {
		return ArchiveProbeStatus{}, syscall.EOPNOTSUPP
	}
	t := n.tree
	parentID := n.item.cloud.ID
	t.probeMu.Lock()
	if job := t.probeJobs[parentID]; job != nil && job.status.State == "running" {
		status := job.status
		t.probeMu.Unlock()
		return status, nil
	}
	if t.probeActive >= 2 {
		t.probeMu.Unlock()
		return ArchiveProbeStatus{}, syscall.EAGAIN
	}
	if t.probeJobs == nil {
		t.probeJobs = make(map[int64]*archiveProbeJob)
		t.probeGate = make(chan struct{}, 2)
	}
	if len(t.probeJobs) >= 8 {
		for id, old := range t.probeJobs {
			if old.status.State != "running" {
				delete(t.probeJobs, id)
			}
		}
	}
	job := &archiveProbeJob{status: ArchiveProbeStatus{State: "running", Persistence: "pending"}}
	t.probeJobs[parentID] = job
	t.probeActive++
	t.probeMu.Unlock()
	go n.runArchiveProbe(job)
	return n.probeStatus(), nil
}

func (n *Node) runArchiveProbe(job *archiveProbeJob) {
	t := n.tree
	ctx, cancel := context.WithTimeout(workqueue.Background(t.ctx), 10*time.Minute)
	defer cancel()
	finish := func(state string) { t.probeMu.Lock(); job.status.State = state; t.probeActive--; t.probeMu.Unlock() }
	directory, err := t.cloudDirectory(ctx, n.item.cloud.ID)
	if err != nil {
		finish("failed")
		return
	}
	old, err := t.loadProbeRecord(ctx, n.item.cloud.ID)
	if err != nil {
		finish("failed")
		return
	}
	known := make(map[int64]archiveProbeResult, len(old.Results))
	for _, result := range old.Results {
		known[result.ID] = result
	}
	var candidates []panapi.File
	for _, f := range directory.files {
		if probeCandidate(f) {
			candidates = append(candidates, f)
		}
	}
	if len(candidates) > maxProbeFiles {
		t.probeMu.Lock()
		job.status.Skipped = len(candidates) - maxProbeFiles
		t.probeMu.Unlock()
		candidates = candidates[:maxProbeFiles]
	}
	t.probeMu.Lock()
	job.status.Total = len(candidates)
	t.probeMu.Unlock()
	results := make([]archiveProbeResult, len(candidates))
	var workers sync.WaitGroup
	items := make(chan int)
	for worker := 0; worker < 2; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := range items {
				if ctx.Err() != nil {
					return
				}
				f := candidates[i]
				result, cached := known[f.ID]
				cached = cached && result.matches(f) && (result.State == "detected" || result.State == "not_archive")
				if !cached {
					select {
					case t.probeGate <- struct{}{}:
					case <-ctx.Done():
						return
					}
					fileCtx, stop := context.WithTimeout(ctx, 25*time.Second)
					result = n.probeFile(fileCtx, f)
					stop()
					<-t.probeGate
				}
				results[i] = result
				t.probeMu.Lock()
				job.status.Checked++
				if cached {
					job.status.Cached++
				}
				switch result.State {
				case "detected":
					job.status.Detected++
				case "failed":
					job.status.Failed++
				default:
					job.status.Unknown++
				}
				t.probeMu.Unlock()
			}
		}()
	}
send:
	for i := range candidates {
		select {
		case items <- i:
		case <-ctx.Done():
			break send
		}
	}
	close(items)
	workers.Wait()
	if ctx.Err() != nil {
		finish("cancelled")
		return
	}
	record := &archiveProbeRecord{Version: 1, ParentID: n.item.cloud.ID, Created: time.Now(), Results: results}
	persistence := t.saveProbeRecord(ctx, record)
	// Drop the old overlay's negative dentry cache without changing any raw
	// inode's type. Existing open directory handles keep their old snapshot.
	if fresh, err := t.cloudDirectory(ctx, n.item.cloud.ID); err == nil {
		n.notifyCloudDirectoryChange(".", &metaItem{value: directory}, fresh)
	}
	t.probeMu.Lock()
	job.status.Persistence = persistence
	t.probeMu.Unlock()
	finish("complete")
}

func (n *Node) probeFile(ctx context.Context, f panapi.File) archiveProbeResult {
	result := archiveProbeResult{ID: f.ID, Version: f.Version, Size: f.Size, Updated: f.UpdatedAt, State: "failed"}
	file := f
	target := &Node{tree: n.tree, item: &entry{cloud: &file}}
	source, err := target.source(ctx)
	if err != nil {
		return result
	}
	reader := &probeReader{ctx: ctx, source: source, left: probeReadBudget}
	result.Kind, result.State = detectArchiveFormat(reader, f.Size)
	return result
}

type probeReader struct {
	ctx    context.Context
	source *storage.Remote
	left   int64
}

func (r *probeReader) ReadAt(p []byte, off int64) (int, error) {
	if int64(len(p)) > r.left {
		return 0, syscall.EFBIG
	}
	r.left -= int64(len(p))
	return r.source.ReadRangeAtContext(r.ctx, p, off)
}

// Recognition checks structure, not the filename. An incomplete/split 7z
// whose NextHeader is outside this file remains unknown; entering aliases
// later performs normal full index/password validation.
func detectArchiveFormat(r io.ReaderAt, size int64) (string, string) {
	var header [32]byte
	n, err := r.ReadAt(header[:min(int64(len(header)), size)], 0)
	if err != nil {
		return "", "failed"
	}
	h := header[:n]
	if bytes.HasPrefix(h, []byte{'7', 'z', 0xbc, 0xaf, 0x27, 0x1c}) {
		if len(h) < 32 || h[6] != 0 || crc32.ChecksumIEEE(h[12:32]) != binary.LittleEndian.Uint32(h[8:12]) {
			return "", "invalid"
		}
		off, length := binary.LittleEndian.Uint64(h[12:20]), binary.LittleEndian.Uint64(h[20:28])
		if off > uint64(size-32) || length > uint64(size-32)-off {
			return "", "incomplete"
		}
		if length == 0 || length > probeReadBudget-32 {
			return "", "unknown"
		}
		next := make([]byte, int(length))
		if _, err := r.ReadAt(next, 32+int64(off)); err != nil {
			return "", "failed"
		}
		if crc32.ChecksumIEEE(next) != binary.LittleEndian.Uint32(h[28:32]) || (next[0] != 1 && next[0] != 0x17) {
			return "", "invalid"
		}
		return ".7z", "detected"
	}
	if bytes.HasPrefix(h, []byte("PK\x03\x04")) || bytes.HasPrefix(h, []byte("PK\x05\x06")) || bytes.HasPrefix(h, []byte("PK\x06\x06")) {
		zr, err := zip.NewReader(r, size)
		if err != nil {
			return "", "unknown"
		}
		if len(zr.File) > 100000 {
			return "", "unknown"
		}
		return ".zip", "detected"
	}
	if bytes.HasPrefix(h, []byte("Rar!\x1a\x07\x00")) || bytes.HasPrefix(h, []byte("Rar!\x1a\x07\x01\x00")) {
		if rarProbeVolume(h) {
			return "", "incomplete"
		}
		// Header-only Walk validates RAR CRCs and does not decompress payloads.
		stop := errors.New("probe found a valid member header")
		err := scanArchive(context.Background(), ".rar", r, size, nil, func(archiveMember) error { return stop })
		if err == nil || errors.Is(err, stop) || errors.Is(err, rardecode.ErrArchiveEncrypted) {
			return ".rar", "detected"
		}
		return "", "unknown"
	}
	return "", "not_archive"
}

// Reject visible volume flags even when the first file fits this volume.
// Encrypted RAR5 main headers are verified by the decoder instead; complete
// archive and password validation still belongs to the normal index path.
func rarProbeVolume(h []byte) bool {
	if bytes.HasPrefix(h, []byte("Rar!\x1a\x07\x00")) {
		return len(h) >= 12 && h[9] == 0x73 && binary.LittleEndian.Uint16(h[10:12])&1 != 0
	}
	if len(h) < 13 {
		return false
	}
	p := h[12:]
	read := func() (uint64, bool) {
		value, n := binary.Uvarint(p)
		if n <= 0 {
			return 0, false
		}
		p = p[n:]
		return value, true
	}
	if _, ok := read(); !ok {
		return false
	} // header size
	kind, ok := read()
	if !ok || kind != 1 {
		return false
	}
	flags, ok := read()
	if !ok {
		return false
	}
	if flags&1 != 0 {
		if _, ok := read(); !ok {
			return false
		}
	}
	if flags&2 != 0 {
		if _, ok := read(); !ok {
			return false
		}
	}
	flags, ok = read()
	return ok && flags&1 != 0
}

type probeControlNode struct {
	fs.Inode
	directory *Node
	start     bool
}

func (n *Node) probeControl(ctx context.Context, name string, out *fuse.EntryOut) *fs.Inode {
	control := &probeControlNode{directory: n, start: name == probeControlName}
	var attr fuse.AttrOut
	control.Getattr(ctx, nil, &attr)
	out.Attr = attr.Attr
	return n.NewInode(ctx, control, stableAttrForEntry(n.StableAttr().Ino, &entry{name: name}))
}

func (*probeControlNode) Getattr(_ context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = fuse.S_IFREG | 0444
	out.Nlink = 1
	return 0
}
func (*probeControlNode) Setattr(context.Context, fs.FileHandle, *fuse.SetAttrIn, *fuse.AttrOut) syscall.Errno {
	return syscall.EROFS
}
func (n *probeControlNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if flags&(syscall.O_ACCMODE|syscall.O_TRUNC|syscall.O_APPEND|syscall.O_CREAT) != syscall.O_RDONLY {
		return nil, 0, syscall.EROFS
	}
	status := n.directory.probeStatus()
	if n.start {
		var err error
		status, err = n.directory.startArchiveProbe(ctx)
		if err != nil {
			return nil, 0, toErrno(err)
		}
	}
	data, _ := json.Marshal(status)
	data = append(data, '\n')
	return &handle{reader: bytes.NewReader(data), size: uint64(len(data))}, fuse.FOPEN_DIRECT_IO, 0
}
