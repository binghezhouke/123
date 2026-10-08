package mountfs

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func cloudNames(t *testing.T, root *Node) map[string]int64 {
	t.Helper()
	stream, errno := root.Readdir(context.Background())
	if errno != 0 {
		t.Fatalf("list cloud directory: %v", errno)
	}
	defer stream.Close()
	names := make(map[string]int64)
	for stream.HasNext() {
		entry, errno := stream.Next()
		if errno != 0 {
			t.Fatal(errno)
		}
		child := lookup(t, root, entry.Name)
		if _, exists := names[entry.Name]; exists {
			t.Fatalf("duplicate visible name %q", entry.Name)
		}
		names[entry.Name] = child.item.cloud.ID
	}
	return names
}

func TestCloudDuplicateNamesRemainAccessibleAndStable(t *testing.T) {
	api := &directorySnapshotAPI{files: []panapi.File{
		{ID: 20, Name: "notes.txt"}, {ID: 10, Name: "notes.txt"},
		{ID: 30, Name: "notes [id=20].txt"},
		{ID: 40, Name: "folder", IsDir: true}, {ID: 41, Name: "folder"},
		{ID: 50, Name: "archive.zip"}, {ID: 51, Name: "archive.zip"},
	}}
	want := map[string]int64{"notes.txt": 10, "notes [id=20-2].txt": 20, "notes [id=20].txt": 30,
		"folder": 40, "folder [id=41]": 41, "archive.zip": 50, "archive [id=51].zip": 51}
	root := NewWithOptions(context.Background(), api, nil, 0, true, Options{})
	fs.NewNodeFS(root, &fs.Options{})
	if got := cloudNames(t, root); !reflect.DeepEqual(got, want) {
		t.Fatalf("names = %v; want %v", got, want)
	}
	archive := lookup(t, root, "archive [id=51].zip")
	if !archive.item.directory || archive.item.cloud.Name != "archive.zip" {
		t.Fatal("display alias changed archive recognition or original cloud name")
	}
	api.mu.Lock()
	for i, j := 0, len(api.files)-1; i < j; i, j = i+1, j-1 {
		api.files[i], api.files[j] = api.files[j], api.files[i]
	}
	api.mu.Unlock()
	if _, err := root.RefreshDirectory(context.Background(), "."); err != nil {
		t.Fatal(err)
	}
	if got := cloudNames(t, root); !reflect.DeepEqual(got, want) {
		t.Fatalf("names after reordered refresh = %v; want %v", got, want)
	}
}

func TestCloudDuplicateAliasFitsLinuxNameLimit(t *testing.T) {
	name := strings.Repeat("图", 83) + ".jpg"
	api := &directorySnapshotAPI{files: []panapi.File{{ID: 1, Name: name}, {ID: 2, Name: name}}}
	root := NewWithOptions(context.Background(), api, nil, 0, true, Options{})
	fs.NewNodeFS(root, &fs.Options{})
	got := cloudNames(t, root)
	if len(got) != 2 || got[name] != 1 {
		t.Fatalf("names = %v", got)
	}
	for alias, id := range got {
		if len(alias) > 255 || !utf8.ValidString(alias) || !strings.HasSuffix(alias, ".jpg") {
			t.Fatalf("invalid alias: bytes=%d id=%d", len(alias), id)
		}
	}
}

type cloudDuplicateReadAPI struct {
	*directorySnapshotAPI
	url string
}

func (a *cloudDuplicateReadAPI) DownloadURL(_ context.Context, id int64) (string, error) {
	return fmt.Sprintf("%s/%d", a.url, id), nil
}

func TestActualFUSECloudDuplicateNamesAndRefresh(t *testing.T) {
	if os.Getenv("MOUNT123_FUSE_TEST") != "1" {
		t.Skip("set MOUNT123_FUSE_TEST=1 to exercise the kernel FUSE path")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		content := []byte("file" + r.URL.Path)
		http.ServeContent(w, r, "file", time.Unix(100, 0), bytes.NewReader(content))
	}))
	defer server.Close()
	api := &cloudDuplicateReadAPI{directorySnapshotAPI: &directorySnapshotAPI{files: []panapi.File{
		{ID: 1, Name: "same.txt", Size: 6}, {ID: 2, Name: "same.txt", Size: 6},
	}}, url: server.URL}
	cache, err := storage.NewCache(t.TempDir(), 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	root := NewWithOptions(context.Background(), api, cache, 0, true, Options{})
	mountpoint := t.TempDir()
	ttl := time.Hour // Refresh must invalidate entries, not wait for the kernel TTL.
	mounted, err := fs.Mount(mountpoint, root, &fs.Options{MountOptions: fuse.MountOptions{Options: []string{"ro"}}, EntryTimeout: &ttl, AttrTimeout: &ttl})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := mounted.Unmount(); err != nil {
			t.Error(err)
		}
		mounted.Wait()
	}()
	entries, err := os.ReadDir(mountpoint)
	if err != nil || len(entries) != 2 {
		t.Fatalf("ReadDir = %v, %v", entries, err)
	}
	checkRead := func(name, want string) {
		t.Helper()
		got, err := os.ReadFile(filepath.Join(mountpoint, name))
		if err != nil || string(got) != want {
			t.Fatalf("ReadFile(%q) = %q, %v; want %q", name, got, err, want)
		}
	}
	checkRead("same.txt", "file/1")
	checkRead("same [id=2].txt", "file/2")
	api.mu.Lock()
	api.files = []panapi.File{{ID: 2, Name: "same.txt", Size: 6}}
	api.mu.Unlock()
	if _, err := root.RefreshDirectory(context.Background(), "."); err != nil {
		t.Fatal(err)
	}
	checkRead("same.txt", "file/2")
	if _, err := os.Stat(filepath.Join(mountpoint, "same [id=2].txt")); !os.IsNotExist(err) {
		t.Fatalf("removed alias stat = %v; want ENOENT", err)
	}
}
