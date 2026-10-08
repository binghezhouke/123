package mountfs

import (
	"context"
	"strings"
	"syscall"
	"time"

	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func (n *Node) progressiveRAR(ctx context.Context) (*zipIndex, *storage.Remote, *archiveDescriptor, bool, error) {
	a := n.item.archive
	if a == nil && n.item.cloud != nil && !n.item.cloud.IsDir {
		f := n.item.cloud
		a = &archiveDescriptor{id: f.ID, parentID: f.ParentID, name: f.Name, version: f.Version, size: f.Size}
	}
	if a == nil || archiveKind(a.name) != ".rar" || !n.item.directory {
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
	if archiveKind(archive.name) == "" {
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
	if archiveKind(archive.name) == ".zip" {
		index, err = n.tree.getZIP(ctx, packedSource, archive.size)
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
	idx, _, _, handled, err := n.progressiveRAR(ctx)
	if !handled {
		ds, errno := n.Readdir(ctx)
		return ds, 0, errno
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
			entries := []fuse.DirEntry{}
			if dir != nil {
				entries = make([]fuse.DirEntry, 0, len(dir.order))
				for _, name := range dir.order {
					mode := uint32(fuse.S_IFREG)
					if dir.dirs[name] != nil {
						mode = fuse.S_IFDIR
					}
					entries = append(entries, fuse.DirEntry{Name: name, Mode: mode})
				}
			}
			idx.mu.RUnlock()
			return fs.NewListDirStream(entries), 0, 0
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
