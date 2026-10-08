package mountfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"syscall"
	"time"

	"github.com/binghezhouke/123/mount123/internal/workqueue"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

const refreshControlName = ".mount123-refresh"
const passwordRefreshCooldown = 30 * time.Second
const maxDirectoryRefreshStates = 1024

type directoryRefreshState struct {
	active *directoryRefreshCall
	until  time.Time
	result RefreshResult
	err    error
}

type directoryRefreshCall struct {
	done   chan struct{}
	result RefreshResult
	err    error
}

// requestDirectoryRefresh joins active explicit/automatic refreshes before
// taking a build slot. Only automatic requests reuse the cooldown result.
// Failed refreshes also cool down; canceled leaders let live waiters retry.
func (t *Tree) requestDirectoryRefresh(ctx context.Context, parentID int64, root *Node, path string, automatic bool) (RefreshResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	stop := context.AfterFunc(t.ctx, cancel)
	defer func() { stop(); cancel() }()
	for {
		if err := ctx.Err(); err != nil {
			return RefreshResult{}, err
		}
		if err := t.ctx.Err(); err != nil {
			return RefreshResult{}, err
		}
		t.refreshMu.Lock()
		if t.directoryRefreshes == nil {
			t.directoryRefreshes = make(map[int64]*directoryRefreshState)
		}
		state := t.directoryRefreshes[parentID]
		if state == nil {
			if len(t.directoryRefreshes) >= maxDirectoryRefreshStates {
				for id, old := range t.directoryRefreshes {
					if old.active == nil && !time.Now().Before(old.until) {
						delete(t.directoryRefreshes, id)
					}
				}
			}
			if len(t.directoryRefreshes) >= maxDirectoryRefreshStates {
				t.refreshMu.Unlock()
				return RefreshResult{}, syscall.EAGAIN
			}
			state = &directoryRefreshState{}
			t.directoryRefreshes[parentID] = state
		}
		if call := state.active; call != nil {
			t.refreshMu.Unlock()
			select {
			case <-ctx.Done():
				return RefreshResult{}, ctx.Err()
			case <-call.done:
				if errors.Is(call.err, context.Canceled) {
					continue
				}
				return call.result, call.err
			}
		}
		if automatic && time.Now().Before(state.until) {
			result, err := state.result, state.err
			t.refreshMu.Unlock()
			return result, err
		}
		call := &directoryRefreshCall{done: make(chan struct{})}
		state.active = call
		t.refreshMu.Unlock()

		call.result, call.err = t.refreshCloudDirectory(ctx, parentID, root, path)
		t.refreshMu.Lock()
		state.active = nil
		state.result, state.err = call.result, call.err
		if !errors.Is(call.err, context.Canceled) {
			state.until = time.Now().Add(passwordRefreshCooldown)
		}
		close(call.done)
		t.refreshMu.Unlock()
		return call.result, call.err
	}
}

func (n *Node) isCloudDirectory() bool {
	return n != nil && n.item != nil && n.item.directory && n.item.cloud != nil && n.item.cloud.IsDir && n.item.source == nil && n.item.archive == nil && n.item.disc == nil && n.item.member == nil
}

// passwordForMember runs only on an encrypted ZIP's actual Open path, outside
// metadata/index build slots. Lookup, index scans and prefetch stay passive.
func (n *Node) passwordForMember(ctx context.Context) ([]byte, error) {
	archive := n.item.archive
	password, err := n.tree.archivePassword(ctx, archive)
	if err != nil || len(password) > 0 {
		return password, err
	}
	if !workqueue.IsBackground(ctx) {
		for parent := n.parent; parent != nil; parent = parent.parent {
			if parent.isCloudDirectory() && parent.item.cloud.ID == archive.parentID {
				if _, err := n.tree.requestDirectoryRefresh(ctx, archive.parentID, parent, ".", true); err != nil {
					return nil, err
				}
				password, err = n.tree.archivePassword(ctx, archive)
				if err != nil || len(password) > 0 {
					return password, err
				}
				break
			}
		}
	}
	return nil, syscall.EACCES
}

// refreshControlNode never enters a directory snapshot. Its name is reserved
// in cloud listings (real same-named files receive the usual ID alias).
type refreshControlNode struct {
	fs.Inode
	directory *Node
}

func (n *Node) refreshControl(ctx context.Context, out *fuse.EntryOut) *fs.Inode {
	control := &refreshControlNode{directory: n}
	var attr fuse.AttrOut
	control.Getattr(ctx, nil, &attr)
	out.Attr = attr.Attr
	return n.NewInode(ctx, control, stableAttrForEntry(n.StableAttr().Ino, &entry{name: refreshControlName}))
}

func (*refreshControlNode) Getattr(_ context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = fuse.S_IFREG | 0444
	out.Nlink = 1
	return 0
}

func (n *refreshControlNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if flags&(syscall.O_ACCMODE|syscall.O_TRUNC|syscall.O_APPEND|syscall.O_CREAT) != syscall.O_RDONLY {
		return nil, 0, syscall.EROFS
	}
	parent := n.directory
	result, err := parent.tree.requestDirectoryRefresh(ctx, parent.item.cloud.ID, parent, ".", false)
	if err != nil {
		return nil, 0, toErrno(err)
	}
	data := []byte(fmt.Sprintf("entries=%d\n", result.Entries))
	return &handle{reader: bytes.NewReader(data), size: uint64(len(data))}, fuse.FOPEN_DIRECT_IO, 0
}

func (*refreshControlNode) Setattr(context.Context, fs.FileHandle, *fuse.SetAttrIn, *fuse.AttrOut) syscall.Errno {
	return syscall.EROFS
}
