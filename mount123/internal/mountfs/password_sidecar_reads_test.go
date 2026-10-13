package mountfs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
)

type passwordSidecarReadsAPI struct {
	*warmArchiveRestartAPI
	files    []panapi.File
	password []byte
	mu       sync.Mutex
	readIDs  []int64
	urlIDs   []int64
}

func (*passwordSidecarReadsAPI) CacheIdentity() string { return "synthetic-sidecar-read-account" }
func (a *passwordSidecarReadsAPI) List(context.Context, int64) ([]panapi.File, error) {
	return append([]panapi.File(nil), a.files...), nil
}
func (a *passwordSidecarReadsAPI) DownloadURL(_ context.Context, id int64) (string, error) {
	a.mu.Lock()
	a.urlIDs = append(a.urlIDs, id)
	a.mu.Unlock()
	if id == 9999 {
		return "synthetic-sidecar-source", nil
	}
	if id != a.file.ID {
		return "", syscall.ENOENT
	}
	return a.url, nil
}
func (a *passwordSidecarReadsAPI) ReadSmallFile(ctx context.Context, id, limit int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if id != 9999 || int64(len(a.password)) > limit {
		return nil, syscall.EACCES
	}
	a.mu.Lock()
	a.readIDs = append(a.readIDs, id)
	a.mu.Unlock()
	// Mirror panapi.Client.ReadSmallFile's one DownloadURL call without
	// networking or persisting the synthetic password response.
	if _, err := a.DownloadURL(ctx, id); err != nil {
		return nil, err
	}
	return append([]byte(nil), a.password...), nil
}

func TestWarmNestedPasswordReadsSelectedSidecarOnce(t *testing.T) {
	for _, sidecar := range []string{"outer.zip.pwd", ".mount123.pwd"} {
		t.Run(sidecar, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			inner, err := os.ReadFile("testdata/encrypted/headers.7z")
			if err != nil {
				t.Fatal(err)
			}
			data := multilevelFixture(t, "zip", map[string][]byte{"inner.7z": inner})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.ServeContent(w, r, "outer.zip", time.Time{}, bytes.NewReader(data))
			}))
			defer server.Close()
			file := panapi.File{ID: 77, Name: "outer.zip", Size: int64(len(data)), Version: "v1"}
			password := []byte("mount-test-password")
			api := &passwordSidecarReadsAPI{
				warmArchiveRestartAPI: &warmArchiveRestartAPI{file: file, url: server.URL},
				files:                 []panapi.File{file, {ID: 9999, Name: sidecar, Size: int64(len(password)), Version: "p1"}},
				password:              password,
			}
			dir := t.TempDir()
			cache, err := storage.NewCache(dir, 32<<20)
			if err != nil {
				t.Fatal(err)
			}
			opts := Options{DirectoryTTL: time.Hour, DisableReadAhead: true}
			seed, err := NewService(ctx, api, cache, 0, true, opts)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = seed.ListDirectory(ctx, 0); err != nil {
				t.Fatal(err)
			}
			if _, err = seed.ListArchive(ctx, file.ID, "inner.7z"); err != nil {
				t.Fatal(err)
			}
			if err = cache.Close(); err != nil {
				t.Fatal(err)
			}
			cache, err = storage.NewCache(dir, 32<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer cache.Close()
			service, err := NewService(ctx, api, cache, 0, true, opts)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = service.ListDirectory(ctx, 0); err != nil {
				t.Fatal(err)
			}
			api.mu.Lock()
			api.readIDs, api.urlIDs = nil, nil
			api.mu.Unlock()
			rows, err := service.ListArchive(ctx, file.ID, "inner.7z")
			if err != nil || len(rows) != 1 || rows[0].Name != "folder" {
				t.Fatalf("warm nested listing=%#v err=%v", rows, err)
			}
			reader, size, err := service.OpenArchiveMember(ctx, file.ID, "inner.7z/folder/hello.txt")
			if err != nil {
				t.Fatal(err)
			}
			want := bytes.Repeat([]byte("mount 7z fixture contents\n"), 200)
			got := make([]byte, len(want))
			n, readErr := reader.ReadAt(got, 0)
			if closer, ok := reader.(io.Closer); ok {
				_ = closer.Close()
			}
			if size != int64(len(want)) || n != len(got) || (readErr != nil && readErr != io.EOF) || !bytes.Equal(got, want) {
				t.Fatalf("warm nested payload size=%d read=%d err=%v equal=%t", size, n, readErr, bytes.Equal(got, want))
			}
			api.mu.Lock()
			defer api.mu.Unlock()
			if fmt.Sprint(api.readIDs) != "[9999]" || fmt.Sprint(api.urlIDs) != "[9999]" {
				t.Errorf("selected sidecar should be fetched once per fresh tree: ReadSmallFile IDs=%v DownloadURL IDs=%v", api.readIDs, api.urlIDs)
			}
		})
	}
}
