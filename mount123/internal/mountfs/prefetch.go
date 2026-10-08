package mountfs

import (
	"context"
	"io"
	"path"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

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

type prefetchTarget struct {
	parent *Node
	name   string
}

type imagePrefetch struct {
	running           map[prefetchTarget]int
	tree              *Tree
	active            int
	lastRead          time.Time
	mu                sync.Mutex
	cancel            context.CancelFunc
	generation        uint64
	parent            *Node
	last              string
	direction, streak int
	slots             chan struct{}
}

func newImagePrefetch(t *Tree) *imagePrefetch {
	workers := t.opts.PrefetchWorkers
	if workers <= 0 {
		workers = 2
	}
	if t.opts.PrefetchBytes <= 0 {
		t.opts.PrefetchBytes = 256 << 20
	}
	return &imagePrefetch{tree: t, running: map[prefetchTarget]int{}, slots: make(chan struct{}, workers)}
}
func (p *imagePrefetch) interrupt() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.generation++
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
}
func (p *imagePrefetch) foreground(n *Node) func() {
	p.mu.Lock()
	p.active++
	p.generation++
	if p.cancel != nil && p.running[prefetchTarget{n.parent, n.item.name}] == 0 {
		p.cancel()
		p.cancel = nil
	}
	p.mu.Unlock()
	return func() { p.mu.Lock(); p.active--; p.mu.Unlock() }
}

func (p *imagePrefetch) observe(n *Node) {
	p.mu.Lock()
	if p.active > 0 {
		p.mu.Unlock()
		return
	}
	if p.cancel != nil {
		p.cancel()
	}
	ctx, cancel := context.WithTimeout(p.tree.ctx, 30*time.Second)
	p.cancel = cancel
	p.generation++
	generation := p.generation
	p.mu.Unlock()
	go func() { defer cancel(); p.run(ctx, n, generation) }()
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
	if index < 0 {
		return nil
	}
	direction, streak := 1, 0
	if p.parent == n.parent && previous >= 0 && time.Since(p.lastRead) < 30*time.Second {
		delta := index - previous
		if delta == 1 || delta == -1 {
			direction = delta
			streak = 0
			if direction == p.direction {
				streak = p.streak + 1
			}
		}
		if delta == 0 {
			direction = p.direction
			streak = p.streak
			if direction == 0 {
				direction = 1
			}
		}
	}
	p.lastRead = time.Now()
	p.parent, p.last, p.direction, p.streak = n.parent, n.item.name, direction, streak
	count := 2
	if streak == 1 {
		count = 4
	}
	if streak >= 2 {
		count = p.tree.opts.PrefetchFiles
	}
	// Reversals start conservatively; do not fetch a full new window immediately.
	if count > p.tree.opts.PrefetchFiles {
		count = p.tree.opts.PrefetchFiles
	}
	if n.item.member != nil && n.item.member.format != "" && count > 1 {
		count = 1
	}
	budget := p.tree.opts.PrefetchBytes
	result := make([]*entry, 0, count)
	for i, examined := index+direction, 0; i >= 0 && i < len(names) && examined < count; i, examined = i+direction, examined+1 {
		e := entries[names[i]]
		var size uint64
		if e.member != nil {
			size = e.member.size
		} else if e.cloud != nil && e.cloud.Size >= 0 {
			size = uint64(e.cloud.Size)
		} else {
			continue
		}
		if size > uint64(budget) {
			continue
		}
		budget -= int64(size)
		result = append(result, e)
	}
	return result
}
func (p *imagePrefetch) run(ctx context.Context, n *Node, generation uint64) {
	entries, err := n.parent.list(ctx)
	if err != nil {
		return
	}
	p.mu.Lock()
	if p.generation != generation || ctx.Err() != nil {
		p.mu.Unlock()
		return
	}
	plan := p.plan(n, entries)
	p.mu.Unlock()
	var wg sync.WaitGroup
	for _, e := range plan {
		select {
		case p.slots <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return
		}
		if ctx.Err() != nil {
			<-p.slots
			break
		}
		target := prefetchTarget{n.parent, e.name}
		p.mu.Lock()
		if p.generation != generation || p.active > 0 {
			p.mu.Unlock()
			<-p.slots
			break
		}
		p.running[target]++
		p.mu.Unlock()
		wg.Add(1)
		go func(e *entry) {
			defer func() {
				p.mu.Lock()
				p.running[target]--
				if p.running[target] == 0 {
					delete(p.running, target)
				}
				p.mu.Unlock()
			}()
			defer wg.Done()
			defer func() { <-p.slots }()
			prefetchImage(ctx, &Node{tree: p.tree, item: e, parent: n.parent})
		}(e)
	}
	wg.Wait()
}
func prefetchImage(ctx context.Context, n *Node) {
	h, _, errno := n.openRaw(ctx, syscall.O_RDONLY)
	if errno != 0 {
		return
	}
	defer h.(fs.FileReleaser).Release(context.Background())
	// Compressed/decrypted members are already materialized by Open. Direct
	// cloud/Store handles need reads to populate the shared 1 MiB block cache.
	handle, ok := h.(*handle)
	if !ok || handle.remote == nil {
		return
	}
	buffer := make([]byte, 1<<20)
	for off := uint64(0); off < handle.size; {
		if ctx.Err() != nil {
			return
		}
		length := uint64(len(buffer))
		if length > handle.size-off {
			length = handle.size - off
		}
		count, err := handle.remote.ReadAtContext(ctx, buffer[:length], handle.base+int64(off))
		off += uint64(count)
		if err != nil && err != io.EOF {
			return
		}
		if count == 0 {
			return
		}
	}
}
