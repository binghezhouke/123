package mountfs

import (
	"bytes"
	"context"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// mountInfoControl is a root-only, read-only diagnostic file. Its contents
// are captured at mount startup, so reading it never performs a cloud call.
type mountInfoControl struct {
	fs.Inode
	info string
}

func (n *Node) mountInfoControl(ctx context.Context, out *fuse.EntryOut) *fs.Inode {
	control := &mountInfoControl{info: n.tree.opts.MountInfo}
	var attr fuse.AttrOut
	control.Getattr(ctx, nil, &attr)
	out.Attr = attr.Attr
	return n.NewInode(ctx, control, stableAttrForEntry(n.StableAttr().Ino, &entry{name: mountInfoControlName}))
}

func (n *mountInfoControl) Getattr(_ context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = fuse.S_IFREG | 0444
	out.Nlink = 1
	out.Size = uint64(len(n.info))
	return 0
}

func (n *mountInfoControl) Open(_ context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if flags&(syscall.O_ACCMODE|syscall.O_TRUNC|syscall.O_APPEND|syscall.O_CREAT) != syscall.O_RDONLY {
		return nil, 0, syscall.EROFS
	}
	data := []byte(n.info)
	return &handle{reader: bytes.NewReader(data), size: uint64(len(data))}, fuse.FOPEN_DIRECT_IO, 0
}

func (*mountInfoControl) Setattr(context.Context, fs.FileHandle, *fuse.SetAttrIn, *fuse.AttrOut) syscall.Errno {
	return syscall.EROFS
}
