package mountfs

import (
	"context"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/hanwen/go-fuse/v2/fs"
)

func (n *Node) progressiveRAR(ctx context.Context) (*zipIndex, *storage.Remote, *archiveDescriptor, bool, error) {
	a := n.item.archive
	if a == nil && n.item.cloud != nil && !n.item.cloud.IsDir {
		f := n.item.cloud
		a = &archiveDescriptor{id: f.ID, parentID: f.ParentID, name: f.Name, version: f.Version, size: f.Size}
	}
	if a == nil || a.kind() != ".rar" || !n.item.directory {
		return nil, nil, nil, false, nil
	}
	source := n.item.source
	var err error
	if source == nil {
		source, err = n.source(ctx)
		if err != nil {
			return nil, nil, nil, true, err
		}
	}
	password, err := n.tree.otherPassword(ctx, a)
	if err != nil {
		return nil, nil, nil, true, err
	}
	defer clear(password)
	idx, err := n.tree.otherIndexMode(ctx, source, a, password, true)
	return idx, source, a, true, err
}

// dirLocked requires the index read lock. Paths and append order remain stable.
func (z *zipIndex) dirLocked(path string) *zipDir {
	d := z.root
	if path != "" {
		for _, part := range strings.Split(path, "/") {
			if d == nil {
				return nil
			}
			d = d.dirs[part]
		}
	}
	return d
}
func indexEntry(d *zipDir, path, name string, source *storage.Remote, a *archiveDescriptor) *entry {
	if d == nil {
		return nil
	}
	if d.dirs[name] != nil {
		childPath := name
		if path != "" {
			childPath = path + "/" + name
		}
		return &entry{name: name, directory: true, zipPath: childPath, source: source, archiveSize: a.size, archive: a}
	}
	if m := d.files[name]; m != nil {
		copied := *m
		copied.file = nil
		copied.reader = nil
		return &entry{name: name, member: &copied, source: source, archiveSize: a.size, archive: a}
	}
	return nil
}
func (n *Node) lookupEntries(ctx context.Context, name string) (map[string]*entry, bool, error) {
	if !n.item.directory {
		return nil, false, syscall.ENOTDIR
	}
	if n.isDisc() {
		entries, err := n.lookupDisc(ctx, name)
		return entries, false, err
	}
	idx, source, a, handled, err := n.progressiveRAR(ctx)
	if handled {
		if err != nil {
			return nil, false, err
		}
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		for {
			idx.mu.RLock()
			e := indexEntry(idx.dirLocked(n.item.zipPath), n.item.zipPath, name, source, a)
			done, scanErr, changed := idx.complete, idx.scanErr, idx.changed
			started := len(idx.root.order) > 0
			idx.mu.RUnlock()
			if e != nil {
				return map[string]*entry{name: e}, false, nil
			}
			if done {
				return nil, false, scanErr
			}
			if started {
				return nil, true, nil
			}
			select {
			case <-changed:
			case <-ctx.Done():
				return nil, false, ctx.Err()
			case <-n.tree.ctx.Done():
				return nil, false, n.tree.ctx.Err()
			case <-timer.C:
				return nil, true, nil
			}
		}
	}
	if n.item.directory && n.item.cloud != nil && n.item.cloud.IsDir && n.item.source == nil {
		entries, err := n.lookupCloud(ctx, name)
		return entries, false, err
	}
	// A lookup in a packed directory only needs the named member. Readdir
	// retains list()'s complete child map and ordering behavior.
	archive := n.item.archive
	if archive == nil && n.item.cloud != nil {
		f := n.item.cloud
		archive = &archiveDescriptor{id: f.ID, parentID: f.ParentID, name: f.Name, version: f.Version, size: f.Size}
	}
	if archive == nil {
		return nil, false, syscall.ENOTDIR
	}
	if archive.kind() == "" {
		return nil, false, syscall.ENOTDIR
	}
	packedSource := n.item.source
	if packedSource == nil {
		packedSource, err = n.source(ctx)
		if err != nil {
			return nil, false, err
		}
	}
	var index *zipIndex
	if archive.kind() == ".zip" {
		index, err = n.tree.getZIP(ctx, packedSource, archive.size, archive)
	} else {
		password, passwordErr := n.tree.otherPassword(ctx, archive)
		if passwordErr != nil {
			return nil, false, passwordErr
		}
		index, err = n.tree.otherIndex(ctx, packedSource, archive, password)
		clear(password)
	}
	if err != nil {
		return nil, false, err
	}
	index.mu.RLock()
	e := indexEntry(index.dirLocked(n.item.zipPath), n.item.zipPath, name, packedSource, archive)
	index.mu.RUnlock()
	if e == nil {
		return nil, false, nil
	}
	return map[string]*entry{name: e}, false, nil
}

// OpendirHandle freezes the currently discovered entries for this listing.
// Reopening observes newer entries; an ordinary ls can always reach this
// snapshot's EOF without waiting for a large archive to finish scanning.
func (n *Node) OpendirHandle(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if n.isDisc() {
		return n.opendirDisc(ctx)
	}
	idx, source, archive, handled, err := n.progressiveRAR(ctx)
	if !handled {
		if n.item.directory && n.item.cloud != nil && n.item.cloud.IsDir && n.item.source == nil {
			directory, err := n.tree.cloudDirectory(ctx, n.item.cloud.ID)
			if err != nil {
				return nil, 0, toErrno(err)
			}
			release, err := n.tree.pinDirectorySnapshot(directory, directory.bytes)
			if err != nil {
				return nil, 0, syscall.ENOMEM
			}
			return newDirectoryHandle(n, directory.entries, directory.names, release), 0, 0
		}
		if n.item.directory && (n.item.source != nil || (n.item.cloud != nil && !n.item.cloud.IsDir)) {
			idx, source, archive, err := n.archiveDirectoryIndex(ctx)
			if err != nil {
				return nil, 0, toErrno(err)
			}
			return n.indexedDirectoryHandle(idx, source, archive)
		}
		entries, err := n.list(ctx)
		if err != nil {
			return nil, 0, toErrno(err)
		}
		return newDirectoryHandle(n, entries, nil, nil), 0, 0
	}
	if err != nil {
		return nil, 0, toErrno(err)
	}
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		idx.mu.RLock()
		dir := idx.dirLocked(n.item.zipPath)
		if idx.complete && idx.scanErr != nil {
			err := idx.scanErr
			idx.mu.RUnlock()
			return nil, 0, toErrno(err)
		}
		if idx.complete || (dir != nil && len(dir.order) > 0) {
			var names []string
			entries := make(map[string]*entry)
			bytes := int64(128)
			if dir != nil {
				names = append([]string(nil), dir.order...)
				bytes += int64(16 * len(names))
				for _, name := range names {
					if e := indexEntry(dir, n.item.zipPath, name, source, archive); e != nil {
						entries[name] = e
						if e.directory {
							bytes += int64(128 + len(name))
						} else {
							bytes += int64(384 + len(name))
						}
					}
				}
			}
			snapshot := &frozenDirectorySnapshot{entries: entries}
			idx.mu.RUnlock()
			release, err := n.tree.pinDirectorySnapshot(snapshot, bytes)
			if err != nil {
				return nil, 0, syscall.ENOMEM
			}
			return newFrozenDirectoryHandle(n, snapshot, names, release), 0, 0
		}
		changed := idx.changed
		idx.mu.RUnlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, 0, toErrno(ctx.Err())
		case <-n.tree.ctx.Done():
			return nil, 0, toErrno(n.tree.ctx.Err())
		case <-timer.C:
			return nil, 0, syscall.EAGAIN
		}
	}
}

func (n *Node) archiveDirectoryIndex(ctx context.Context) (*zipIndex, *storage.Remote, *archiveDescriptor, error) {
	archive := n.item.archive
	if archive == nil && n.item.cloud != nil {
		f := n.item.cloud
		archive = &archiveDescriptor{id: f.ID, parentID: f.ParentID, name: f.Name, version: f.Version, size: f.Size}
	}
	if archive == nil || archive.kind() == "" {
		return nil, nil, nil, syscall.ENOTDIR
	}
	source := n.item.source
	var err error
	if source == nil {
		source, err = n.source(ctx)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	var idx *zipIndex
	if archive.kind() == ".zip" {
		idx, err = n.tree.getZIP(ctx, source, archive.size, archive)
	} else {
		password, passwordErr := n.tree.otherPassword(ctx, archive)
		if passwordErr != nil {
			return nil, nil, nil, passwordErr
		}
		idx, err = n.tree.otherIndex(ctx, source, archive, password)
		clear(password)
	}
	return idx, source, archive, err
}

func (n *Node) indexedDirectoryHandle(idx *zipIndex, source *storage.Remote, archive *archiveDescriptor) (fs.FileHandle, uint32, syscall.Errno) {
	idx.mu.RLock()
	dir := idx.dirLocked(n.item.zipPath)
	var names []string
	if dir != nil {
		names = make([]string, 0, len(dir.dirs)+len(dir.files))
		for name := range dir.dirs {
			names = append(names, name)
		}
		for name := range dir.files {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	pinBytes := idx.bytes
	idx.mu.RUnlock()
	release, err := n.tree.pinDirectorySnapshotWithHandle(idx, pinBytes, int64(16*len(names)))
	if err != nil {
		return nil, 0, syscall.ENOMEM
	}
	return newIndexedDirectoryHandle(n, idx, n.item.zipPath, source, archive, names, release), 0, 0
}
