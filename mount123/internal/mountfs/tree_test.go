package mountfs

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type fakeAPI struct {
	url     string
	archive []byte
}

func (a *fakeAPI) List(_ context.Context, id int64) ([]panapi.File, error) {
	if id == 2 {
		return []panapi.File{{ID: 3, Name: "plain.txt", Size: 11, Version: "v1"}}, nil
	}
	return []panapi.File{{ID: 1, Name: "photos.zip", Size: int64(len(a.archive)), Version: "v1"}, {ID: 2, Name: "folder", IsDir: true}}, nil
}
func (a *fakeAPI) DownloadURL(_ context.Context, id int64) (string, error) {
	return fmt.Sprintf("%s/%d", a.url, id), nil
}
func fixture(t *testing.T) (*Node, []byte) {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	content := bytes.Repeat([]byte("sample content\n"), 10000)
	for _, method := range []uint16{zip.Store, zip.Deflate} {
		name := fmt.Sprintf("images/%d.txt", method)
		m, err := w.CreateHeader(&zip.FileHeader{Name: name, Method: method})
		if err != nil {
			t.Fatal(err)
		}
		m.Write(content)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{archive: b.Bytes()}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data := api.archive
		if r.URL.Path == "/3" {
			data = []byte("hello cloud")
		}
		w.Header().Set("ETag", `"v1"`)
		http.ServeContent(w, r, "file", time.Unix(100, 0), bytes.NewReader(data))
	}))
	t.Cleanup(server.Close)
	api.url = server.URL
	cache, err := storage.NewCache(t.TempDir(), 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cache.Close() })
	return New(context.Background(), api, cache, 0, true), content
}
func lookup(t *testing.T, n *Node, name string) *Node {
	t.Helper()
	inode, errno := n.Lookup(context.Background(), name, &fuse.EntryOut{})
	if errno != 0 {
		t.Fatalf("lookup %s: %v", name, errno)
	}
	return inode.Operations().(*Node)
}
func readNode(t *testing.T, n *Node, offset int64, size int) []byte {
	t.Helper()
	h, _, errno := n.Open(context.Background(), syscall.O_RDONLY)
	if errno != 0 {
		t.Fatal(errno)
	}
	defer h.(fs.FileReleaser).Release(context.Background())
	result, errno := h.(fs.FileReader).Read(context.Background(), make([]byte, size), offset)
	if errno != 0 {
		t.Fatal(errno)
	}
	data, status := result.Bytes(make([]byte, size))
	if status != fuse.OK {
		t.Fatal(status)
	}
	return data
}
func TestPublicTreeReadsAndReadOnly(t *testing.T) {
	root, content := fixture(t)
	fs.NewNodeFS(root, &fs.Options{})
	archive := lookup(t, root, "photos.zip")
	images := lookup(t, archive, "images")
	for _, name := range []string{"0.txt", "8.txt"} {
		member := lookup(t, images, name)
		if got := readNode(t, member, 12345, 900); !bytes.Equal(got, content[12345:13245]) {
			t.Fatal("ZIP seek read mismatch")
		}
		if _, _, errno := member.Open(context.Background(), syscall.O_WRONLY); errno != syscall.EROFS {
			t.Fatalf("write allowed: %v", errno)
		}
		if errno := member.Setattr(context.Background(), nil, &fuse.SetAttrIn{}, &fuse.AttrOut{}); errno != syscall.EROFS {
			t.Fatal(errno)
		}
	}
	plain := lookup(t, lookup(t, root, "folder"), "plain.txt")
	if got := string(readNode(t, plain, 6, 20)); got != "cloud" {
		t.Fatalf("plain read = %q", got)
	}
}
func TestUnsafeZIPPaths(t *testing.T) {
	for _, names := range [][]string{{"../escape"}, {"/absolute"}, {"a\\b"}, {"duplicate", "duplicate"}, {"file", "file/child"}} {
		t.Run(strings.Join(names, "_"), func(t *testing.T) {
			var b bytes.Buffer
			w := zip.NewWriter(&b)
			for _, name := range names {
				m, _ := w.Create(name)
				io.WriteString(m, "data")
			}
			w.Close()
			z, err := zip.NewReader(bytes.NewReader(b.Bytes()), int64(b.Len()))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = zipEntries(z, nil); err == nil {
				t.Fatal("unsafe ZIP accepted")
			}
		})
	}
}
func TestActualFUSEMount(t *testing.T) {
	if os.Getenv("MOUNT123_FUSE_TEST") != "1" {
		t.Skip("set MOUNT123_FUSE_TEST=1 to test actual Linux FUSE mounting")
	}
	root, content := fixture(t)
	mountpoint := t.TempDir()
	server, err := fs.Mount(mountpoint, root, &fs.Options{MountOptions: fuse.MountOptions{Options: []string{"ro"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := server.Unmount(); err != nil {
			t.Error(err)
		}
		server.Wait()
	}()
	for _, name := range []string{"0.txt", "8.txt"} {
		data, err := os.ReadFile(filepath.Join(mountpoint, "photos.zip", "images", name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, content) {
			t.Fatal("mounted content mismatch")
		}
	}
	plain := filepath.Join(mountpoint, "folder", "plain.txt")
	data, err := os.ReadFile(plain)
	if err != nil || string(data) != "hello cloud" {
		t.Fatalf("plain: %q %v", data, err)
	}
	if err = os.WriteFile(plain, []byte("mutate"), 0600); !os.IsPermission(err) && err != syscall.EROFS && !strings.Contains(fmt.Sprint(err), "read-only") {
		t.Fatalf("write not rejected: %v", err)
	}
	if err = os.Mkdir(filepath.Join(mountpoint, "new"), 0700); err == nil {
		t.Fatal("mkdir succeeded")
	}
	if err = os.Remove(plain); err == nil {
		t.Fatal("unlink succeeded")
	}
	if err = os.Rename(plain, plain+".new"); err == nil {
		t.Fatal("rename succeeded")
	}
}
