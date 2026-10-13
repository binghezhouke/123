package mountfs

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type nestedPasswordAPI struct {
	*archiveAPI
	lists         atomic.Int64
	passwordReads atomic.Int64
}

func (a *nestedPasswordAPI) List(ctx context.Context, id int64) ([]panapi.File, error) {
	a.lists.Add(1)
	return a.archiveAPI.List(ctx, id)
}

func (a *nestedPasswordAPI) Detail(ctx context.Context, id int64) (panapi.File, error) {
	if err := ctx.Err(); err != nil {
		return panapi.File{}, err
	}
	for _, file := range a.files {
		if file.ID == id {
			return file, nil
		}
	}
	return panapi.File{}, syscall.ENOENT
}

func (a *nestedPasswordAPI) Infos(ctx context.Context, ids []int64) ([]panapi.File, error) {
	files := make([]panapi.File, 0, len(ids))
	for _, id := range ids {
		file, err := a.Detail(ctx, id)
		if err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	return files, nil
}

func (a *nestedPasswordAPI) ReadSmallFile(ctx context.Context, id, limit int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// There is exactly one password file, beside the root cloud archive.
	// An inner-specific lookup cannot accidentally return its contents.
	if id != 9999 || int64(len(a.password)) > limit {
		return nil, syscall.EACCES
	}
	a.passwordReads.Add(1)
	return append([]byte(nil), a.password...), nil
}

func nestedPasswordFixture(t *testing.T, format, innerName, fixture string) (*Node, *nestedPasswordAPI) {
	t.Helper()
	inner, err := os.ReadFile(filepath.Join("testdata/encrypted", fixture))
	if err != nil {
		t.Fatal(err)
	}
	outerName := "outer." + format
	data := multilevelFixture(t, format, map[string][]byte{innerName: inner})
	root, base := otherArchiveFixture(t, outerName, data, []byte("mount-test-password"), false)
	api := &nestedPasswordAPI{archiveAPI: base}
	root.tree.api = api
	root.tree.builds = make(chan struct{}, 1)
	root.tree.opts.MaxConcurrentBuilds = 1
	if len(api.files) != 2 || api.files[1].Name != outerName+".pwd" || api.lists.Load() != 0 || len(root.tree.meta) != 0 {
		t.Fatal("fixture must start with cold metadata and only the root archive's sidecar")
	}
	return root, api
}

func nestedPasswordLookup(t *testing.T, ctx context.Context, node *Node, name string) *Node {
	t.Helper()
	for {
		inode, errno := node.Lookup(ctx, name, &fuse.EntryOut{})
		if errno == 0 {
			return inode.Operations().(*Node)
		}
		if errno != syscall.EAGAIN {
			t.Fatalf("FUSE nested lookup %q: %v", name, errno)
		}
		// A cold progressive RAR may report an incomplete safe prefix. Retry
		// its explicit lookup under the same bounded request deadline.
		select {
		case <-ctx.Done():
			t.Fatalf("FUSE nested lookup %q: %v", name, ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
}

func TestCrossFormatNestedPasswordInheritance(t *testing.T) {
	cases := []struct {
		name, innerName, fixture, memberPath string
		want                                 []byte
	}{
		{
			name: "aes-zip", innerName: "inner.zip", fixture: "aes256-ae2-deflate.zip", memberPath: "hello.txt",
			want: append(bytes.Repeat([]byte("mount123 encrypted fixture\n"), 256), byteSliceRange()...),
		},
		{
			name: "encrypted-7z-headers", innerName: "inner.7z", fixture: "headers.7z", memberPath: "folder/hello.txt",
			want: bytes.Repeat([]byte("mount 7z fixture contents\n"), 200),
		},
	}
	for _, format := range []string{"zip", "rar"} {
		for _, tc := range cases {
			for _, frontend := range []string{"service", "fuse"} {
				t.Run(format+"/"+tc.name+"/"+frontend, func(t *testing.T) {
					root, api := nestedPasswordFixture(t, format, tc.innerName, tc.fixture)
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					memberPath := tc.innerName + "/" + tc.memberPath
					if frontend == "service" {
						service := NewServiceFromNode(root)
						rows, err := service.ListArchive(ctx, 77, tc.innerName)
						if err != nil || len(rows) != 1 {
							t.Fatalf("public nested listing: rows=%v error=%v", rows, err)
						}
						reader, size, err := service.OpenArchiveMember(ctx, 77, memberPath)
						if err != nil {
							t.Fatalf("public nested open: %v", err)
						}
						if closer, ok := reader.(io.Closer); ok {
							defer closer.Close()
						}
						got := make([]byte, len(tc.want))
						if n, err := reader.ReadAt(got, 0); size != int64(len(tc.want)) || n != len(got) || (err != nil && err != io.EOF) || !bytes.Equal(got, tc.want) {
							t.Fatalf("public nested payload: size=%d read=%d error=%v equal=%v", size, n, err, bytes.Equal(got, tc.want))
						}
					} else {
						node := root
						for _, name := range strings.Split("outer."+format+"/"+memberPath, "/") {
							node = nestedPasswordLookup(t, ctx, node, name)
						}
						h, _, errno := node.Open(ctx, syscall.O_RDONLY)
						if errno != 0 {
							t.Fatalf("FUSE nested open: %v", errno)
						}
						defer h.(fs.FileReleaser).Release(context.Background())
						result, errno := h.(fs.FileReader).Read(ctx, make([]byte, len(tc.want)), 0)
						if errno != 0 {
							t.Fatalf("FUSE nested read: %v", errno)
						}
						got, status := result.Bytes(nil)
						if status != fuse.OK || !bytes.Equal(got, tc.want) {
							t.Fatalf("FUSE nested payload: size=%d status=%v equal=%v", len(got), status, bytes.Equal(got, tc.want))
						}
					}
					if api.passwordReads.Load() == 0 {
						t.Fatal("inner archive did not discover the root password sidecar")
					}
				})
			}
		}
	}
}
