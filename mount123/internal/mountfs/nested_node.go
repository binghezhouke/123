package mountfs

import (
	"context"
	"runtime"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
)

func isExpandableArchiveMember(e *entry) bool {
	if e == nil || e.member == nil {
		return false
	}
	kind := archiveKind(e.member.name)
	return kind == ".zip" || kind == ".7z" || kind == ".rar"
}

func (n *Node) expandNested(ctx context.Context, e *entry) (*entry, error) {
	if !isExpandableArchiveMember(e) {
		return e, nil
	}
	var na *nestedArchive
	var err error
	if e.nested != nil {
		na, err = n.tree.enterNestedArchive(ctx, e.nested, e.member.name, e.member)
	} else {
		archive, source := e.archive, e.source
		if n.item != nil {
			if archive == nil {
				archive = n.item.archive
			}
			if source == nil {
				source = n.item.source
			}
		}
		if archive == nil || source == nil || n.tree.cache == nil {
			return e, nil
		}
		na, err = n.tree.buildNestedArchive(ctx, source, archive, nil, e.member.name, e.member)
	}
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
	copy.zipPath = ""
	copy.source = nil
	copy.archive = nil
	copy.cloud = nil
	copy.nested = na
	copy.archiveSize = na.size
	return &copy, nil
}

func (n *Node) openNestedMember(ctx context.Context) (fs.FileHandle, uint32, syscall.Errno) {
	na, m := n.item.nested, n.item.member
	if na == nil || m == nil {
		return nil, 0, syscall.EIO
	}
	reader, size, err := n.tree.openNestedArchiveMember(ctx, na, m)
	if err != nil {
		return nil, 0, toErrno(err)
	}
	return &handle{reader: reader, closer: reader, size: uint64(size)}, n.tree.archiveCacheOpenFlags(), 0
}
