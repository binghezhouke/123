package mountfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/binghezhouke/123/mount123/internal/workqueue"
)

// ArchiveIndexStatus is a point-in-time view of a mount-local archive scan.
// DownloadBytes is nil when the storage reader does not expose an exact count.
type ArchiveIndexStatus struct {
	State             string `json:"state"`
	Members           int    `json:"members"`
	ScanOffset        int64  `json:"scan_offset"`
	ArchiveSize       int64  `json:"archive_size"`
	DownloadBytes     *int64 `json:"download_bytes"`
	FailureKind       string `json:"failure_kind,omitempty"`
	FailureReason     string `json:"failure_reason,omitempty"`
	RecommendedAction string `json:"recommended_action,omitempty"`
}

type indexStatusTracker struct {
	mu     sync.Mutex
	states map[string]*indexStatusState
	starts map[string]bool
}

type indexStatusState struct {
	status ArchiveIndexStatus
	change chan struct{}
}

func newIndexStatusTracker() *indexStatusTracker {
	return &indexStatusTracker{states: make(map[string]*indexStatusState), starts: make(map[string]bool)}
}

func (t *indexStatusTracker) start(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.starts[key] {
		return false
	}
	t.starts[key] = true
	return true
}

// A historical completion is not evidence that an evicted index is resident.
// Requeue on a cache miss; a persisted snapshot can satisfy the new request.
func (t *indexStatusTracker) requeueCompleted(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if state := t.states[key]; state != nil && state.status.State == "complete" && !t.starts[key] {
		state.status.State = "queued"
		close(state.change)
		state.change = make(chan struct{})
	}
}

func (t *indexStatusTracker) resetFailed(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	state := t.states[key]
	if state == nil || state.status.State != "failed" || t.starts[key] {
		return false
	}
	delete(t.states, key)
	return true
}

func (t *indexStatusTracker) done(key string) {
	t.mu.Lock()
	delete(t.starts, key)
	t.mu.Unlock()
}

func (t *indexStatusTracker) set(key string, status ArchiveIndexStatus) {
	t.mu.Lock()
	defer t.mu.Unlock()
	state := t.states[key]
	if state == nil {
		state = &indexStatusState{change: make(chan struct{})}
		t.states[key] = state
	}
	if state.status.State == "complete" || state.status.State == "failed" {
		if status.State == "queued" || status.State == "scanning" {
			return
		}
	}
	state.status = status
	close(state.change)
	state.change = make(chan struct{})
}

func (t *indexStatusTracker) update(key string, update func(*ArchiveIndexStatus)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	state := t.states[key]
	if state == nil {
		state = &indexStatusState{status: ArchiveIndexStatus{State: "queued"}, change: make(chan struct{})}
		t.states[key] = state
	}
	update(&state.status)
	close(state.change)
	state.change = make(chan struct{})
}

func (t *indexStatusTracker) snapshot(key string) (ArchiveIndexStatus, <-chan struct{}) {
	t.mu.Lock()
	defer t.mu.Unlock()
	state := t.states[key]
	if state == nil {
		return ArchiveIndexStatus{}, nil
	}
	return state.status, state.change
}

// StatusArchiveIndex resolves path inside this mount and starts indexing when
// needed. The path is relative to the mounted cloud root; a leading slash is
// accepted as a mount-relative path.
func (n *Node) StatusArchiveIndex(ctx context.Context, archivePath string) (ArchiveIndexStatus, error) {
	return n.statusArchiveIndex(ctx, archivePath, false)
}

// RetryArchiveIndex retries only a previously failed, inactive index build.
// Existing work remains coalesced and completed indexes remain reusable.
func (n *Node) RetryArchiveIndex(ctx context.Context, archivePath string) (ArchiveIndexStatus, error) {
	return n.statusArchiveIndex(ctx, archivePath, true)
}

func (n *Node) statusArchiveIndex(ctx context.Context, archivePath string, retry bool) (ArchiveIndexStatus, error) {
	for _, component := range strings.Split(archivePath, "/") {
		if component == ".." {
			return ArchiveIndexStatus{}, syscall.EINVAL
		}
	}
	clean := path.Clean("/" + strings.TrimPrefix(archivePath, "/"))
	if clean == "/" || clean == "/." || strings.Contains(clean, "\x00") {
		return ArchiveIndexStatus{}, syscall.EINVAL
	}
	parts := strings.Split(strings.TrimPrefix(clean, "/"), "/")
	parent := n
	for _, component := range parts[:len(parts)-1] {
		entries, pending, err := parent.lookupEntries(ctx, component)
		if err != nil {
			return ArchiveIndexStatus{}, err
		}
		if pending {
			return ArchiveIndexStatus{State: "scanning"}, nil
		}
		item := entries[component]
		if item == nil {
			return ArchiveIndexStatus{}, syscall.ENOENT
		}
		if !item.directory {
			return ArchiveIndexStatus{}, syscall.ENOTDIR
		}
		parent = &Node{tree: n.tree, item: item, parent: parent}
	}
	name := parts[len(parts)-1]
	entries, pending, err := parent.lookupEntries(ctx, name)
	if err != nil {
		return ArchiveIndexStatus{}, err
	}
	if pending {
		return ArchiveIndexStatus{State: "scanning"}, nil
	}
	item := entries[name]
	if item == nil {
		return ArchiveIndexStatus{}, syscall.ENOENT
	}
	archive := item.archive
	if archive == nil && item.cloud != nil {
		f := item.cloud
		archive = &archiveDescriptor{id: f.ID, parentID: f.ParentID, name: f.Name, version: f.Version, size: f.Size}
	}
	if !item.directory || archive == nil || archive.kind() == "" {
		return ArchiveIndexStatus{}, syscall.EINVAL
	}
	if item.source != nil && item.zipPath != "" {
		return ArchiveIndexStatus{}, syscall.EINVAL
	}
	target := &Node{tree: n.tree, item: item, parent: parent}
	if archive.kind() == ".zip" {
		return target.startZIPIndexStatus(ctx, archive, retry)
	}
	return target.startOtherIndexStatus(ctx, archive, retry)
}

func (n *Node) startZIPIndexStatus(ctx context.Context, archive *archiveDescriptor, retry bool) (ArchiveIndexStatus, error) {
	source, err := n.source(ctx)
	if err != nil {
		return failedArchiveStatus(archive.size, err), nil
	}
	// Queueing must not read the archive tail. The cached parent generation
	// changes when the ordered manifest can change, even with the same final ID.
	var generation uint64
	n.tree.mu.Lock()
	if item := n.tree.meta[fmt.Sprintf("dir:%d", archive.parentID)]; item != nil {
		if directory, ok := item.value.(*cloudDirectory); ok {
			generation = directory.generation
		}
	}
	n.tree.mu.Unlock()
	idx, prepared, err := n.cachedZIP(ctx, source, archive)
	if err != nil {
		return failedArchiveStatus(archive.size, err), nil
	}
	key := fmt.Sprintf("status:zip:%s:g%d", archiveIdentity(source, archive), generation)
	archiveSize := archive.size
	if idx != nil {
		archiveSize = prepared.size
		status := ArchiveIndexStatus{State: "complete", Members: len(idx.members), ScanOffset: archiveSize, ArchiveSize: archiveSize}
		n.tree.indexStatuses.set(key, status)
		return status, nil
	}
	if retry {
		n.tree.indexStatuses.resetFailed(key)
	}
	n.tree.indexStatuses.requeueCompleted(key)
	status, _ := n.tree.indexStatuses.snapshot(key)
	if status.State == "failed" {
		return status, nil
	}
	if status.State == "" {
		n.tree.indexStatuses.set(key, ArchiveIndexStatus{State: "queued", ArchiveSize: archiveSize})
		status.State = "queued"
	}
	if n.tree.indexStatuses.start(key) {
		go func() {
			defer n.tree.indexStatuses.done(key)
			n.tree.indexStatuses.update(key, func(s *ArchiveIndexStatus) { s.State = "scanning" })
			idx, err := n.tree.getZIPPreparedWithProgress(workqueue.Background(n.tree.ctx), source, archive.size, archive, nil, func(offset int64) {
				n.tree.indexStatuses.update(key, func(s *ArchiveIndexStatus) {
					s.State = "scanning"
					s.ArchiveSize = archiveSize
					if offset > s.ScanOffset {
						s.ScanOffset = min(offset, archiveSize)
					}
				})
			}, func(rawSize int64) {
				archiveSize = rawSize
				n.tree.indexStatuses.update(key, func(s *ArchiveIndexStatus) { s.ArchiveSize = rawSize })
			})
			if err != nil {
				n.tree.indexStatuses.set(key, failedArchiveStatus(archiveSize, err))
				return
			}
			n.tree.indexStatuses.set(key, ArchiveIndexStatus{State: "complete", Members: len(idx.members), ScanOffset: archiveSize, ArchiveSize: archiveSize})
		}()
	}
	status, _ = n.tree.indexStatuses.snapshot(key)
	return status, nil
}

func (n *Node) cachedZIP(ctx context.Context, source *storage.Remote, archive *archiveDescriptor) (*zipIndex, *preparedArchiveInput, error) {
	// Match getZIPPreparedWithProgress: listing/status never needs a body
	// password, and polling must not start unrelated cloud password I/O.
	identity := archiveIdentity(source, archive)
	scope, passwordIdentity := n.tree.diskCacheScope(), n.tree.passwordTag(archive, nil)
	key := "zip:" + scope + ":" + identity + ":" + passwordIdentity
	lookup := func(key string) *zipIndex {
		n.tree.mu.Lock()
		defer n.tree.mu.Unlock()
		item := n.tree.meta[key]
		if item == nil || (!item.expires.IsZero() && time.Now().After(item.expires)) {
			return nil
		}
		index, _ := item.value.(*zipIndex)
		return index
	}
	if index := lookup(key); index != nil && index.prepared != nil {
		return index, index.prepared, nil
	}
	// Validate a resident split index against the immutable cached directory.
	// This is a metadata operation; a cold tail is prepared by the worker.
	n.tree.mu.Lock()
	defer n.tree.mu.Unlock()
	item := n.tree.meta[fmt.Sprintf("dir:%d", archive.parentID)]
	if item == nil {
		return nil, nil, nil
	}
	directory, ok := item.value.(*cloudDirectory)
	if !ok {
		return nil, nil, nil
	}
	var candidates []archiveVolumeCandidate
	for metaKey, item := range n.tree.meta {
		if !strings.HasPrefix(metaKey, "zip:") || (!item.expires.IsZero() && time.Now().After(item.expires)) {
			continue
		}
		index, ok := item.value.(*zipIndex)
		if !ok || index.prepared == nil || index.prepared.layout.kind == archiveSingle {
			continue
		}
		prepared := index.prepared
		if metaKey != "zip:"+scope+":"+prepared.identity+":"+passwordIdentity {
			continue
		}
		volumes, ok := prepared.input.(*volumeReaderAt)
		if !ok || len(volumes.parts) == 0 {
			continue
		}
		pinned := 0
		if prepared.layout.kind == archiveZIPDisks {
			pinned = len(volumes.parts) - 1
		}
		final := volumes.parts[pinned]
		if final.ID != archive.id || final.Version != archive.version || final.Size != archive.size || final.Name != archive.name {
			continue
		}
		if candidates == nil {
			candidates = make([]archiveVolumeCandidate, len(directory.files))
			for i, f := range directory.files {
				candidates[i] = archiveVolumeCandidate{f.Name, f.Size, f.IsDir}
			}
		}
		indices, _, _, err := discoverArchiveVolumes(archive.name, prepared.layout.kind, uint32(len(volumes.parts)-1), candidates)
		if err != nil || len(indices) != len(volumes.parts) {
			continue
		}
		matches := true
		for i, j := range indices {
			f, old := directory.files[j], volumes.parts[i]
			if f.ID != old.ID || f.Version != old.Version || f.Size != old.Size || f.Name != old.Name {
				matches = false
				break
			}
		}
		if matches {
			return index, prepared, nil
		}
	}
	return nil, nil, nil
}

func (n *Node) startOtherIndexStatus(ctx context.Context, archive *archiveDescriptor, retry bool) (ArchiveIndexStatus, error) {
	source, err := n.source(ctx)
	if err != nil {
		return failedArchiveStatus(archive.size, err), nil
	}
	password, err := n.tree.otherPassword(ctx, archive)
	if err != nil {
		return failedArchiveStatus(archive.size, err), nil
	}
	defer clear(password)
	key := n.tree.archiveTaskKey(source, archive, password)
	_, archiveSize, identity, err := n.tree.archiveSource(ctx, source, archive)
	if err != nil {
		return failedArchiveStatus(archive.size, err), nil
	}
	cacheKey := "archive-index:" + archive.kind() + ":" + identity + ":" + n.tree.passwordTag(archive, password)
	n.tree.mu.Lock()
	item := n.tree.meta[cacheKey]
	var cached *zipIndex
	if item != nil && (item.expires.IsZero() || time.Now().Before(item.expires)) {
		cached, _ = item.value.(*zipIndex)
	}
	n.tree.mu.Unlock()
	if cached != nil {
		status := ArchiveIndexStatus{State: "complete", Members: len(cached.members), ScanOffset: archiveSize, ArchiveSize: archiveSize}
		n.tree.indexStatuses.set(key, status)
		return status, nil
	}
	if retry {
		n.tree.indexStatuses.resetFailed(key)
	}
	n.tree.indexStatuses.requeueCompleted(key)
	status, _ := n.tree.indexStatuses.snapshot(key)
	if status.State == "" {
		n.tree.indexStatuses.set(key, ArchiveIndexStatus{State: "queued", ArchiveSize: archive.size})
		status.State = "queued"
	}
	if status.State == "complete" || status.State == "failed" {
		return status, nil
	}
	if status.State == "queued" && n.tree.indexStatuses.start(key) {
		passwordCopy := append([]byte(nil), password...)
		go func() {
			defer n.tree.indexStatuses.done(key)
			defer clear(passwordCopy)
			statusCtx, cancel := context.WithCancel(n.tree.ctx)
			defer cancel()
			idx, err := n.tree.otherIndex(statusCtx, source, archive, passwordCopy)
			if errors.Is(err, syscall.EAGAIN) {
				return
			}
			if err != nil {
				n.tree.indexStatuses.set(key, failedArchiveStatus(archive.size, err))
				return
			}
			n.tree.indexStatuses.set(key, ArchiveIndexStatus{State: "complete", Members: len(idx.members), ScanOffset: archiveSize, ArchiveSize: archiveSize})
		}()
	}
	status, _ = n.tree.indexStatuses.snapshot(key)
	return status, nil
}

type indexProgressReaderAt struct {
	r      io.ReaderAt
	status *indexStatusTracker
	key    string
	size   int64
}

func (r *indexProgressReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := r.r.ReadAt(p, off)
	if n > 0 {
		high := off + int64(n)
		r.status.update(r.key, func(s *ArchiveIndexStatus) {
			s.State = "scanning"
			s.ArchiveSize = r.size
			if high > s.ScanOffset {
				s.ScanOffset = high
			}
		})
	}
	return n, err
}
