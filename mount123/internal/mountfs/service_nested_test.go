package mountfs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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
