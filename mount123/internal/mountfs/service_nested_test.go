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
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
)

type serviceNestedAPI struct {
	url    string
	files  []panapi.File
	detail panapi.File
}

func (a *serviceNestedAPI) List(context.Context, int64) ([]panapi.File, error) {
	return append([]panapi.File(nil), a.files...), nil
}
func (a *serviceNestedAPI) DownloadURL(_ context.Context, id int64) (string, error) {
	return fmt.Sprintf("%s/%d", a.url, id), nil
}
func (a *serviceNestedAPI) Infos(context.Context, []int64) ([]panapi.File, error) {
	return []panapi.File{a.detail}, nil
}
func (a *serviceNestedAPI) Detail(context.Context, int64) (panapi.File, error) {
	return a.detail, nil
}

func newServiceNested(t *testing.T, data []byte) *Service {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"fixture"`)
		http.ServeContent(w, r, "outer.7z", time.Unix(100, 0), bytes.NewReader(data))
	}))
	t.Cleanup(server.Close)
	api := &serviceNestedAPI{
		url:    server.URL,
		files:  []panapi.File{{ID: 77, Name: "outer.7z", Size: int64(len(data)), Version: "v1"}},
		detail: panapi.File{ID: 77, Name: "outer.7z", Size: int64(len(data)), Version: "v1"},
	}
	cache, err := storage.NewCache(t.TempDir(), 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	service, err := NewService(context.Background(), api, cache, 0, true, Options{})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestServiceListsNested7zVolume(t *testing.T) {
	data, err := os.ReadFile("testdata/encrypted/nested-vol-outer.7z")
	if err != nil {
		t.Fatal(err)
	}
	service := newServiceNested(t, data)
	ctx := context.Background()

	outer, err := service.ListArchive(ctx, 77, "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range outer {
		if entry.Name == "inner.7z.001" {
			found = true
		}
	}
	if !found {
		t.Fatalf("outer listing does not contain the volume entry: %#v", outer)
	}

	inner, err := service.ListArchive(ctx, 77, "inner.7z.001")
	if err != nil {
		t.Fatalf("nested listing: %v", err)
	}
	if len(inner) == 0 {
		t.Fatalf("nested listing is empty")
	}
	for _, entry := range inner {
		if entry.Name == "plain.7z" {
			return
		}
	}
	t.Fatalf("nested listing does not contain plain.7z: %#v", inner)
}

func TestServiceReadsNestedMember(t *testing.T) {
	data, err := os.ReadFile("testdata/encrypted/nested-vol-outer.7z")
	if err != nil {
		t.Fatal(err)
	}
	service := newServiceNested(t, data)
	ctx := context.Background()

	reader, size, err := service.OpenArchiveMember(ctx, 77, "inner.7z.001/plain.7z")
	if err != nil {
		t.Fatalf("open nested member: %v", err)
	}
	if size <= 0 {
		t.Fatalf("nested member size = %d", size)
	}
	head := make([]byte, 8)
	if _, err := reader.ReadAt(head, 0); err != nil && err != io.EOF {
		t.Fatalf("read nested member: %v", err)
	}
	if len(head) == 0 {
		t.Fatal("nested member head is empty")
	}
}

func TestServiceReadsNestedRARMember(t *testing.T) {
	sevenzip, err := exec.LookPath("7z")
	if err != nil {
		t.Skip("7z fixture generator unavailable")
	}
	dir := t.TempDir()
	rardata, err := os.ReadFile("../../../tests/fixtures/rar/rar5-crc.rar")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "inner.rar"), rardata, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(sevenzip, "a", "-t7z", "-m0=lzma2", "-mx=5", "outer.7z", "inner.rar")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate: %v %s", err, out)
	}
	data, err := os.ReadFile(filepath.Join(dir, "outer.7z"))
	if err != nil {
		t.Fatal(err)
	}
	service := newServiceNested(t, data)
	ctx := context.Background()
	inner, err := service.ListArchive(ctx, 77, "inner.rar")
	if err != nil {
		t.Fatalf("nested RAR listing: %v", err)
	}
	if len(inner) != 2 || inner[0].Name != "stest1.txt" {
		t.Fatalf("nested RAR entries: %#v", inner)
	}
	reader, size, err := service.OpenArchiveMember(ctx, 77, "inner.rar/stest1.txt")
	if err != nil {
		t.Fatalf("open nested RAR member: %v", err)
	}
	if c, ok := reader.(io.Closer); ok {
		defer c.Close()
	}
	got := make([]byte, size)
	if _, err = reader.ReadAt(got, 0); err != nil && err != io.EOF {
		t.Fatalf("read nested RAR member: %v", err)
	}
	if len(got) != 2048 {
		t.Fatalf("nested RAR member size = %d", len(got))
	}
}

func TestServiceNestedMissingMemberReturnsNotFound(t *testing.T) {
	data, err := os.ReadFile("testdata/encrypted/nested-vol-outer.7z")
	if err != nil {
		t.Fatal(err)
	}
	service := newServiceNested(t, data)
	if _, _, err := service.OpenArchiveMember(context.Background(), 77, "inner.7z.001/nope.txt"); err != syscall.ENOENT {
		t.Fatalf("missing nested member err = %v, want ENOENT", err)
	}
}

// The real compressed path must remain readable after listing has closed its
// decoder, and after the lookup request/password buffers have gone away.
func TestCompressedNestedSharedReader(t *testing.T) {
	sevenzip, err := exec.LookPath("7z")
	if err != nil {
		t.Skip("7z fixture generator unavailable")
	}
	dir := t.TempDir()
	inner, err := os.ReadFile("testdata/encrypted/plain.7z")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "inner.7z"), inner, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(sevenzip, "a", "-t7z", "-m0=lzma2", "-mx=5", "outer.7z", "inner.7z")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate: %v %s", err, out)
	}
	data, err := os.ReadFile(filepath.Join(dir, "outer.7z"))
	if err != nil {
		t.Fatal(err)
	}
	service := newServiceNested(t, data)
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := service.ListArchive(ctx, 77, "inner.7z"); err != nil {
		t.Fatal(err)
	}
	cancel()
	reader, size, err := service.OpenArchiveMember(context.Background(), 77, "inner.7z/folder/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := reader.(io.Closer); ok {
		defer c.Close()
	}
	got := make([]byte, size)
	if _, err = reader.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(got, []byte("mount 7z fixture contents\n")) {
		t.Fatalf("wrong bytes: %q", got)
	}
	root, _ := otherArchiveFixture(t, "outer.7z", data, nil, false)
	nested := lookup(t, lookup(t, root, "outer.7z"), "inner.7z")
	file := lookup(t, lookup(t, nested, "folder"), "hello.txt")
	if got := readNode(t, file, 0, 100); !bytes.HasPrefix(got, []byte("mount 7z fixture contents\n")) {
		t.Fatalf("FUSE bytes: %q", got)
	}
	if nested.item.nested != nil {
		_ = nested.item.nested.Close()
	}
}

func TestServiceZIPDecodedMemberPath(t *testing.T) {
	var payload bytes.Buffer
	zw := zip.NewWriter(&payload)
	// GBK name; both the directory and leaf differ from the raw index key.
	raw := "\xd6\xd0\xce\xc4/  \xcd\xbc.jpg"
	w, err := zw.CreateHeader(&zip.FileHeader{Name: raw, NonUTF8: true, Method: zip.Deflate})
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("fixture image bytes")
	if _, err = w.Write(want); err != nil {
		t.Fatal(err)
	}
	if err = zw.Close(); err != nil {
		t.Fatal(err)
	}
	service := newServiceNested(t, payload.Bytes())
	api := service.tree.api.(*serviceNestedAPI)
	api.detail.Name = "fixture.zip"
	api.files[0].Name = "fixture.zip"
	rows, err := service.ListArchive(context.Background(), 77, "中文")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Name != "  图.jpg" {
		t.Fatalf("rows: %#v", rows)
	}
	reader, _, err := service.OpenArchiveMember(context.Background(), 77, "中文/"+rows[0].Name)
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := reader.(io.Closer); ok {
		defer c.Close()
	}
	got := make([]byte, len(want))
	if _, err = reader.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("wrong member bytes")
	}
}
