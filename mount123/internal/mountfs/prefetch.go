package mountfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/binghezhouke/123/mount123/internal/workqueue"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func imageName(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp", ".avif", ".heic", ".tif", ".tiff":
		return true
	}
	return false
}

// Numeric runs are compared without integer conversion (arbitrarily long IDs).
func naturalLess(a, b string) bool {
	x, y := strings.ToLower(a), strings.ToLower(b)
	for len(x) > 0 && len(y) > 0 {
		if x[0] >= '0' && x[0] <= '9' && y[0] >= '0' && y[0] <= '9' {
			i, j := 0, 0
			for i < len(x) && x[i] >= '0' && x[i] <= '9' {
				i++
			}
			for j < len(y) && y[j] >= '0' && y[j] <= '9' {
				j++
			}
			p, q := strings.TrimLeft(x[:i], "0"), strings.TrimLeft(y[:j], "0")
			if len(p) != len(q) {
				return len(p) < len(q)
			}
			if p != q {
				return p < q
			}
			x, y = x[i:], y[j:]
		} else {
			if x[0] != y[0] {
				return x[0] < y[0]
			}
			x, y = x[1:], y[1:]
		}
	}
	if len(x) != len(y) {
		return len(x) < len(y)
	}
	return a < b
}

type imageHandle struct {
	fs.FileHandle
	node *Node
	once sync.Once
}

func (h *imageHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	result, errno := h.FileHandle.(fs.FileReader).Read(ctx, dest, off)
	if errno == 0 && result != nil && result.Size() > 0 {
		h.once.Do(func() { h.node.tree.prefetch.observe(h.node) })
	}
	return result, errno
}
func (h *imageHandle) Release(ctx context.Context) syscall.Errno {
	return h.FileHandle.(fs.FileReleaser).Release(ctx)
}

// A prefetch target is keyed by immutable directory and content identities.
// It deliberately contains no Node pointers, so rebuilt nodes can reuse work.
type prefetchTarget struct {
	directory string
	name      string
	content   string
}

type prefetchJob struct {
	cancel  context.CancelFunc
	ctx     context.Context
	started bool
	finish  sync.Once
}

type imagePrefetch struct {
	running           map[prefetchTarget]*prefetchJob
	completed         map[prefetchTarget]struct{}
	completedOrder    []prefetchTarget
	tree              *Tree
	active            int
	planCancel        context.CancelFunc
	mu                sync.Mutex
	sequence          uint64
	parentKey         string
	last              string
	lastRead          time.Time
	direction, streak int
	slots             chan struct{}
	statsMu           sync.Mutex
	stats             ImagePrefetchStatsSnapshot
}

func newImagePrefetch(t *Tree) *imagePrefetch {
	workers := t.opts.PrefetchWorkers
	if workers <= 0 {
		workers = 2
	}
	if t.opts.PrefetchBytes <= 0 {
		t.opts.PrefetchBytes = 256 << 20
	}
	return &imagePrefetch{tree: t, running: map[prefetchTarget]*prefetchJob{}, completed: map[prefetchTarget]struct{}{}, slots: make(chan struct{}, workers)}
}

func (p *imagePrefetch) addStat(fn func(*ImagePrefetchStatsSnapshot)) {
	p.statsMu.Lock()
	fn(&p.stats)
	p.statsMu.Unlock()
}

func (p *imagePrefetch) interrupt() {
	p.mu.Lock()
	p.sequence++
	if p.planCancel != nil {
		p.planCancel()
		p.planCancel = nil
	}
	for target, job := range p.running {
		job.cancel()
		delete(p.running, target)
	}
	p.mu.Unlock()
}

// foreground pauses planning while Open is in progress. Same-directory jobs
// remain alive so opening the next image cannot kill work that the next window
// will continue to need. Jobs in a different directory are cancelled promptly.
func (p *imagePrefetch) foreground(n *Node) func() {
	directory := p.directoryIdentity(n.parent)
	target := p.targetIdentity(n)
	p.mu.Lock()
	_, inFlight := p.running[target]
	_, ready := p.completed[target]
	if inFlight || ready {
		p.addStat(func(s *ImagePrefetchStatsSnapshot) {
			if inFlight {
				s.ForegroundInFlight++
			} else if ready {
				s.ForegroundReady++
			}
		})
	}
	p.active++
	p.sequence++
	if p.planCancel != nil {
		p.planCancel()
		p.planCancel = nil
	}
	for key, job := range p.running {
		if key.directory != directory && key != target {
			job.cancel()
			delete(p.running, key)
		}
	}
	p.mu.Unlock()
	return func() {
		p.mu.Lock()
		if p.active > 0 {
			p.active--
		}
		p.mu.Unlock()
	}
}

func (p *imagePrefetch) observe(n *Node) {
	// A bulk metadata scan keeps the read path busy with work the user asked
	// for; speculative image windows wait until traffic returns to normal.
	if p.tree.scanShedding() {
		return
	}
	p.mu.Lock()
	if p.active > 0 {
		p.mu.Unlock()
		return
	}
	p.sequence++
	sequence := p.sequence
	if p.planCancel != nil {
		p.planCancel()
	}
	ctx, cancel := context.WithTimeout(workqueue.Background(p.tree.ctx), 30*time.Second)
	p.planCancel = cancel
	p.mu.Unlock()
	p.addStat(func(s *ImagePrefetchStatsSnapshot) { s.Planned++ })
	go func() {
		defer cancel()
		p.run(ctx, n, sequence)
	}()
}

func (p *imagePrefetch) plan(n *Node, entries map[string]*entry) []*entry {
	names := make([]string, 0, len(entries))
	for name, e := range entries {
		if !e.directory && imageName(name) {
			names = append(names, name)
		}
	}
	sort.Slice(names, func(i, j int) bool { return naturalLess(names[i], names[j]) })
	index, previous := -1, -1
	for i, name := range names {
		if name == n.item.name {
			index = i
		}
		if name == p.last {
			previous = i
		}
	}
	parentKey := p.directoryIdentity(n.parent)
	if index < 0 {
		return nil
	}
	direction, streak := 1, 0
	if p.parentKey == parentKey && previous >= 0 && time.Since(p.lastRead) < 30*time.Second {
		delta := index - previous
		if delta == 1 || delta == -1 {
			direction = delta
			if direction == p.direction {
				streak = p.streak + 1
			}
		}
		if delta == 0 {
			direction, streak = p.direction, p.streak
			if direction == 0 {
				direction = 1
			}
		}
	}
	p.parentKey, p.last, p.lastRead, p.direction, p.streak = parentKey, n.item.name, time.Now(), direction, streak
	count := 2
	if streak == 1 {
		count = 4
	}
	if streak >= 2 {
		count = p.tree.opts.PrefetchFiles
	}
	if count <= 0 {
		count = 2
	}
	if count > p.tree.opts.PrefetchFiles && p.tree.opts.PrefetchFiles > 0 {
		count = p.tree.opts.PrefetchFiles
	}
	solid7z := n.item.member != nil && p.tree.cache != nil && p.tree.solid7zCacheable(n.item.member)
	if n.item.member != nil && n.item.member.format != "" && !solid7z && count > 1 {
		count = 1
	}
	budget := p.tree.opts.PrefetchBytes
	result := make([]*entry, 0, count)
	for i, examined := index+direction, 0; i >= 0 && i < len(names) && examined < count; i, examined = i+direction, examined+1 {
		e := entries[names[i]]
		// A solid group's foreground fill already retains all of its bytes.
		// Warm nearby member views from that group, without starting unrelated
		// groups whose decompressed size can dwarf the image window budget.
		if solid7z && (e.member == nil || e.member.sevenStream == nil || e.member.sevenStream.Stream != n.item.member.sevenStream.Stream) {
			continue
		}
		var size uint64
		if e.disc != nil {
			size = e.disc.size
		} else if e.member != nil {
			size = e.member.size
		} else if e.cloud != nil && e.cloud.Size >= 0 {
			size = uint64(e.cloud.Size)
		} else {
			continue
		}
		if size > uint64(max(int64(0), budget)) {
			continue
		}
		budget -= int64(size)
		result = append(result, e)
	}
	if solid7z {
		sort.SliceStable(result, func(i, j int) bool {
			return result[i].member.sevenStream.Offset < result[j].member.sevenStream.Offset
		})
	}
	return result
}

func (p *imagePrefetch) run(ctx context.Context, n *Node, sequence uint64) {
	defer func() {
		p.mu.Lock()
		if p.sequence == sequence {
			p.planCancel = nil
		}
		p.mu.Unlock()
	}()
	if n == nil || n.parent == nil {
		return
	}
	entries, err := n.parent.list(ctx)
	if err != nil {
		return
	}
	p.mu.Lock()
	if p.sequence != sequence || p.active > 0 || ctx.Err() != nil {
		p.mu.Unlock()
		return
	}
	plan := p.plan(n, entries)
	desired := make(map[prefetchTarget]*entry, len(plan))
	ordered := make([]prefetchTarget, 0, len(plan))
	for _, e := range plan {
		child := &Node{tree: p.tree, item: e, parent: n.parent}
		target := p.targetIdentity(child)
		desired[target] = e
		ordered = append(ordered, target)
	}
	for target, job := range p.running {
		if _, keep := desired[target]; !keep {
			job.cancel()
			delete(p.running, target)
		}
	}
	pending := make([]pendingPrefetch, 0, len(ordered))
	for _, target := range ordered {
		e := desired[target]
		if existing := p.running[target]; existing != nil {
			if existing.started {
				p.addStat(func(s *ImagePrefetchStatsSnapshot) { s.Reused++ })
				continue
			}
			pending = append(pending, pendingPrefetch{target: target, job: existing, node: &Node{tree: p.tree, item: e, parent: n.parent}})
			continue
		}
		jobCtx, cancel := context.WithTimeout(workqueue.Background(p.tree.ctx), 30*time.Second)
		job := &prefetchJob{cancel: cancel, ctx: jobCtx}
		p.running[target] = job
		context.AfterFunc(jobCtx, func() { p.finishUnstarted(target, job, jobCtx.Err()) })
		p.addStat(func(s *ImagePrefetchStatsSnapshot) { s.Started++ })
		pending = append(pending, pendingPrefetch{target: target, job: job, node: &Node{tree: p.tree, item: e, parent: n.parent}})
	}
	p.mu.Unlock()
	p.dispatch(ctx, sequence, pending)
}

func (p *imagePrefetch) dispatch(ctx context.Context, sequence uint64, pending []pendingPrefetch) {
	// Acquire worker slots in plan order. This makes nearby images enter the
	// worker pool first; launching waiters in goroutines would race on slots.
	for _, task := range pending {
		select {
		case p.slots <- struct{}{}:
		case <-ctx.Done():
			return
		case <-task.job.ctx.Done():
			p.finishTarget(task.target, task.job, false, task.job.ctx.Err())
			continue
		case <-p.tree.ctx.Done():
			task.job.cancel()
			p.finishTarget(task.target, task.job, false, task.job.ctx.Err())
			continue
		}
		p.mu.Lock()
		start := p.sequence == sequence && p.running[task.target] == task.job && !task.job.started && task.job.ctx.Err() == nil && ctx.Err() == nil
		if start {
			task.job.started = true
		}
		p.mu.Unlock()
		if start {
			go p.runTargetWithSlot(task.job.ctx, task.target, task.job, task.node)
		} else {
			<-p.slots
			if ctx.Err() != nil || p.currentSequence() != sequence {
				return
			}
			if task.job.ctx.Err() != nil {
				p.finishTarget(task.target, task.job, false, task.job.ctx.Err())
			}
		}
	}
}

func (p *imagePrefetch) currentSequence() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sequence
}

func (p *imagePrefetch) finishUnstarted(target prefetchTarget, job *prefetchJob, err error) {
	p.mu.Lock()
	started := job.started
	if !started && p.running[target] == job {
		delete(p.running, target)
	}
	p.mu.Unlock()
	if !started {
		p.finishTarget(target, job, false, err)
	}
}

type pendingPrefetch struct {
	target prefetchTarget
	job    *prefetchJob
	node   *Node
}

func (p *imagePrefetch) runTarget(ctx context.Context, target prefetchTarget, job *prefetchJob, n *Node) {
	acquired := false
	select {
	case p.slots <- struct{}{}:
		acquired = true
	case <-ctx.Done():
	}
	if acquired {
		p.runTargetWithSlot(ctx, target, job, n)
		return
	}
	p.finishTarget(target, job, false, ctx.Err())
}

func (p *imagePrefetch) runTargetWithSlot(ctx context.Context, target prefetchTarget, job *prefetchJob, n *Node) {
	defer func() { <-p.slots }()
	completed := ctx.Err() == nil && prefetchImage(ctx, n)
	p.finishTarget(target, job, completed, ctx.Err())
}

func (p *imagePrefetch) finishTarget(target prefetchTarget, job *prefetchJob, completed bool, ctxErr error) {
	job.finish.Do(func() {
		job.cancel()
		p.mu.Lock()
		if p.running[target] == job {
			delete(p.running, target)
		}
		if completed {
			p.rememberCompletedLocked(target)
		}
		p.mu.Unlock()
		if completed {
			p.addStat(func(s *ImagePrefetchStatsSnapshot) { s.Completed++ })
		} else if errors.Is(ctxErr, context.Canceled) {
			p.addStat(func(s *ImagePrefetchStatsSnapshot) { s.Cancelled++ })
		} else {
			p.addStat(func(s *ImagePrefetchStatsSnapshot) { s.Failed++ })
		}
	})
}

// rememberCompletedLocked keeps each identity at most once in the bounded
// recency order so an old duplicate cannot evict a newer completion marker.
func (p *imagePrefetch) rememberCompletedLocked(target prefetchTarget) {
	if _, exists := p.completed[target]; exists {
		kept := p.completedOrder[:0]
		for _, previous := range p.completedOrder {
			if previous != target {
				kept = append(kept, previous)
			}
		}
		p.completedOrder = kept
	}
	p.completed[target] = struct{}{}
	p.completedOrder = append(p.completedOrder, target)
	if len(p.completedOrder) > 64 {
		oldest := p.completedOrder[0]
		p.completedOrder = p.completedOrder[1:]
		delete(p.completed, oldest)
	}
}

func (p *imagePrefetch) directoryIdentity(n *Node) string {
	if n == nil || n.item == nil {
		return "unknown"
	}
	e := n.item
	if e.cloud != nil && e.cloud.IsDir {
		generation := uint64(0)
		p.tree.mu.Lock()
		if meta := p.tree.meta[fmt.Sprintf("dir:%d", e.cloud.ID)]; meta != nil {
			if directory, ok := meta.value.(*cloudDirectory); ok {
				generation = directory.generation
			}
		}
		p.tree.mu.Unlock()
		return fmt.Sprintf("cloud:%s:%d:%d", p.tree.diskCacheScope(), e.cloud.ID, generation)
	}
	if e.disc != nil {
		base := p.archiveEntryIdentity(e)
		return "disc:" + base + ":" + path.Clean(e.disc.path)
	}
	if e.archive != nil || e.zipPath != "" || (e.cloud != nil && !e.cloud.IsDir && p.tree.zipDirs && archiveKind(e.cloud.Name) != "") {
		return "archive:" + p.archiveEntryIdentity(e) + ":" + e.zipPath
	}
	if e.source != nil {
		return "source:" + e.source.Key()
	}
	if e.cloud != nil {
		return "cloud-parent:" + p.tree.cloudCacheKey(e.cloud)
	}
	return "unknown"
}

func (p *imagePrefetch) archiveEntryIdentity(e *entry) string {
	a := e.archive
	if a == nil && e.cloud != nil {
		a = &archiveDescriptor{id: e.cloud.ID, parentID: e.cloud.ParentID, name: e.cloud.Name, version: e.cloud.Version, size: e.cloud.Size}
	}
	if a != nil {
		if a.version != "" {
			return fmt.Sprintf("cloud:%d:%s:%d:%s", a.id, a.version, a.size, a.kind())
		}
		if e.source != nil {
			return e.source.Key() + ":" + a.kind()
		}
		if e.cloud != nil {
			return p.tree.cloudCacheKey(e.cloud) + ":" + a.kind()
		}
	}
	if e.source != nil {
		return e.source.Key()
	}
	return "unknown"
}

func (p *imagePrefetch) targetIdentity(n *Node) prefetchTarget {
	if n == nil || n.item == nil {
		return prefetchTarget{directory: "unknown"}
	}
	e := n.item
	content := "unknown"
	switch {
	case e.cloud != nil:
		content = fmt.Sprintf("cloud:%d:%s:%d", e.cloud.ID, e.cloud.Version, e.cloud.Size)
	case e.member != nil:
		content = fmt.Sprintf("member:%s:%08x:%d:%d:%d:%d:%t", e.member.name, e.member.crc, e.member.size, e.member.compressed, e.member.method, e.member.flags, e.member.encrypted)
	case e.disc != nil:
		content = fmt.Sprintf("disc:%s:%d:%d", e.disc.path, e.disc.size, e.disc.modified.UnixNano())
	}
	return prefetchTarget{directory: p.directoryIdentity(n.parent), name: e.name, content: content}
}

func prefetchImage(ctx context.Context, n *Node) bool {
	if n.item.disc != nil && !n.item.directory {
		source, imageSize, _, err := n.discSource(ctx)
		if err != nil {
			return false
		}
		prefetch := &discPrefetchSource{source: source, imageSize: imageSize, path: n.item.disc.path, size: n.item.disc.size}
		return prefetch.PrefetchRangeAtContext(ctx, 0, int64(n.item.disc.size)) == nil
	}
	h, _, errno := n.openRaw(ctx, syscall.O_RDONLY)
	if errno != 0 {
		return false
	}
	defer h.(fs.FileReleaser).Release(context.Background())
	handle, ok := h.(*handle)
	if !ok {
		return false
	}
	if growing := handle.growing; growing != nil {
		return growing.Wait(ctx) == nil
	}
	if handle.remote == nil {
		return true
	}
	buffer := make([]byte, 1<<20)
	for off := uint64(0); off < handle.size; {
		if err := ctx.Err(); err != nil {
			return false
		}
		length := uint64(len(buffer))
		if length > handle.size-off {
			length = handle.size - off
		}
		count, err := handle.remote.ReadAtContext(ctx, buffer[:length], handle.base+int64(off))
		off += uint64(count)
		if err != nil && err != io.EOF {
			return false
		}
		if count == 0 {
			return false
		}
	}
	return true
}
