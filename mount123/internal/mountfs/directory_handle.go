package mountfs

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"syscall"

	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// directoryHandle keeps the names returned by one OpendirHandle call. The
// resolver points at an immutable snapshot or a completed shared index. A
// progressive RAR freezes only its current directory's discovered entries.
type directoryHandle struct {
	mu      sync.Mutex
	parent  *Node
	names   []string
	resolve func(string) *entry
	release func()
	cursor  int
	closed  bool
}

type frozenDirectorySnapshot struct {
	entries map[string]*entry
}

func (t *Tree) pinDirectorySnapshot(snapshot any, bytes int64) (func(), error) {
	return t.pinDirectorySnapshotWithHandle(snapshot, bytes, 0)
}

func (t *Tree) pinDirectorySnapshotWithHandle(snapshot any, bytes, handleBytes int64) (func(), error) {
	if bytes < 0 || handleBytes < 0 || bytes+handleBytes > t.opts.MetadataBytes {
		return nil, fmt.Errorf("directory snapshot exceeds metadata budget")
	}
	t.directoryPinMu.Lock()
	if t.directoryPins == nil {
		t.directoryPins = make(map[any]directoryPin)
	}
	pin, exists := t.directoryPins[snapshot]
	newBytes := handleBytes
	if !exists {
		newBytes += bytes
	}
	if t.directoryPinBytes+newBytes > t.opts.MetadataBytes {
		t.directoryPinMu.Unlock()
		return nil, fmt.Errorf("open directory snapshots exceed metadata budget")
	}
	if !exists {
		pin.bytes = bytes
	}
	pin.refs++
	pin.handleBytes += handleBytes
	t.directoryPinBytes += newBytes
	t.directoryPins[snapshot] = pin
	t.directoryPinMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			t.directoryPinMu.Lock()
			pin := t.directoryPins[snapshot]
			pin.refs--
			pin.handleBytes -= handleBytes
			t.directoryPinBytes -= handleBytes
			if pin.refs == 0 {
				delete(t.directoryPins, snapshot)
				t.directoryPinBytes -= pin.bytes
			} else {
				t.directoryPins[snapshot] = pin
			}
			t.directoryPinMu.Unlock()
		})
	}, nil
}

func newDirectoryHandle(parent *Node, entries map[string]*entry, names []string, release func()) *directoryHandle {
	if names == nil {
		names = make([]string, 0, len(entries))
		for name := range entries {
			names = append(names, name)
		}
		sort.Strings(names)
	}
	snapshot := &frozenDirectorySnapshot{entries: entries}
	return newFrozenDirectoryHandle(parent, snapshot, names, release)
}

func newFrozenDirectoryHandle(parent *Node, snapshot *frozenDirectorySnapshot, names []string, release func()) *directoryHandle {
	return &directoryHandle{
		parent: parent,
		names:  names,
		resolve: func(name string) *entry {
			return snapshot.entries[name]
		},
		release: release,
	}
}

func newIndexedDirectoryHandle(parent *Node, idx *zipIndex, path string, source *storage.Remote, archive *archiveDescriptor, names []string, release func()) *directoryHandle {
	return &directoryHandle{
		parent: parent,
		names:  names,
		resolve: func(name string) *entry {
			idx.mu.RLock()
			defer idx.mu.RUnlock()
			return indexEntry(idx.dirLocked(path), path, name, source, archive)
		},
		release: release,
	}
}

func (h *directoryHandle) Readdirent(context.Context) (*fuse.DirEntry, syscall.Errno) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, syscall.EBADF
	}
	if h.cursor >= len(h.names) {
		return nil, 0
	}
	h.cursor++
	name := h.names[h.cursor-1]
	mode := uint32(fuse.S_IFREG)
	if item := h.resolve(name); item != nil && item.directory {
		mode = fuse.S_IFDIR
	}
	return &fuse.DirEntry{Name: name, Mode: mode, Off: uint64(h.cursor)}, 0
}

func (h *directoryHandle) Seekdir(_ context.Context, off uint64) syscall.Errno {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return syscall.EBADF
	}
	if off > uint64(len(h.names)) {
		return syscall.EINVAL
	}
	h.cursor = int(off)
	return 0
}

func (h *directoryHandle) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, syscall.EBADF
	}
	parent, resolve := h.parent, h.resolve
	if parent == nil || resolve == nil {
		return nil, syscall.ENOENT
	}
	e := resolve(name)
	if e == nil {
		return nil, syscall.ENOENT
	}
	return parent.inodeForEntry(ctx, e, out, false), 0
}

func (h *directoryHandle) Releasedir(context.Context, uint32) {
	h.mu.Lock()
	h.closed = true
	h.names = nil
	h.resolve = nil
	h.parent = nil
	release := h.release
	h.release = nil
	h.mu.Unlock()
	if release != nil {
		release()
	}
}

var _ fs.FileReaddirenter = (*directoryHandle)(nil)
var _ fs.FileLookuper = (*directoryHandle)(nil)
var _ fs.FileSeekdirer = (*directoryHandle)(nil)
var _ fs.FileReleasedirer = (*directoryHandle)(nil)
