package mountfs

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
)

func TestServiceISOThroughArchiveAPI(t *testing.T) {
	data, err := os.ReadFile("testdata/disc/images/iso9660-base.iso")
	if err != nil {
		t.Fatal(err)
	}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "disc.iso", time.Time{}, bytes.NewReader(data))
	}))
	defer remote.Close()
	file := panapi.File{ID: 77, Name: "disc.ISO", Size: int64(len(data)), Version: "v1"}
	api := &serviceNestedAPI{url: remote.URL, files: []panapi.File{file}, detail: file}
	cache, err := storage.NewCache(t.TempDir(), 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	svc, err := NewService(context.Background(), api, cache, 0, true, Options{})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := svc.ListArchive(context.Background(), 77, "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if row.Name != "README.TXT" {
			continue
		}
		found = true
		reader, size, err := svc.OpenArchiveMember(context.Background(), 77, row.Name)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(io.NewSectionReader(reader, 0, size))
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile("testdata/disc/payload/base/README.TXT")
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("member mismatch: %q", got)
		}
	}
	if !found {
		t.Fatal("README.TXT missing")
	}
}
