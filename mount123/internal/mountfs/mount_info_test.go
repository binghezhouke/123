package mountfs

import (
	"context"
	"syscall"
	"testing"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	goFS "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type mountInfoTestAPI struct{}

func (mountInfoTestAPI) List(context.Context, int64) ([]panapi.File, error) { return nil, nil }
func (mountInfoTestAPI) DownloadURL(context.Context, int64) (string, error) { return "", nil }

func TestMountInfoControlIsRootOnlyAndReadOnly(t *testing.T) {
	root := NewWithOptions(context.Background(), mountInfoTestAPI{}, nil, 0, true, Options{MountInfo: "version=test\ncache_dir=/cache\n"})
	goFS.NewNodeFS(root, &goFS.Options{})
	inode, errno := root.Lookup(context.Background(), mountInfoControlName, &fuse.EntryOut{})
	if errno != 0 {
		t.Fatalf("lookup info control: %v", errno)
	}
	control := inode.Operations()
	if errno := control.(goFS.NodeGetattrer).Getattr(context.Background(), nil, &fuse.AttrOut{}); errno != 0 {
		t.Fatal(errno)
	}
	if _, _, errno := control.(goFS.NodeOpener).Open(context.Background(), syscall.O_WRONLY); errno != syscall.EROFS {
		t.Fatalf("write errno=%v", errno)
	}
	h, flags, errno := control.(goFS.NodeOpener).Open(context.Background(), syscall.O_RDONLY)
	if errno != 0 || flags&fuse.FOPEN_DIRECT_IO == 0 {
		t.Fatalf("open info: flags=%x errno=%v", flags, errno)
	}
	defer h.(goFS.FileReleaser).Release(context.Background())
	result, errno := h.(goFS.FileReader).Read(context.Background(), make([]byte, 128), 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	data, status := result.Bytes(make([]byte, 128))
	if status != fuse.OK || string(data) != "version=test\ncache_dir=/cache\n" {
		t.Fatalf("info=%q status=%v", data, status)
	}
	if _, errno := root.Lookup(context.Background(), mountInfoControlName, &fuse.EntryOut{}); errno != 0 {
		t.Fatal(errno)
	}
}
