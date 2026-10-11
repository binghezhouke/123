package mountfs

import (
	"context"
	"syscall"
	"testing"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
)

type serviceAPI struct {
	files   map[int64][]panapi.File
	details map[int64]panapi.File
}

func (a *serviceAPI) List(_ context.Context, id int64) ([]panapi.File, error) {
	return append([]panapi.File(nil), a.files[id]...), nil
}
func (a *serviceAPI) DownloadURL(context.Context, int64) (string, error) {
	return "", syscall.EOPNOTSUPP
}
func (a *serviceAPI) Infos(_ context.Context, ids []int64) ([]panapi.File, error) {
	out := make([]panapi.File, 0, len(ids))
	for _, id := range ids {
		if f, ok := a.details[id]; ok {
			out = append(out, f)
		}
	}
	return out, nil
}
func (a *serviceAPI) Detail(_ context.Context, id int64) (panapi.File, error) {
	f, ok := a.details[id]
	if !ok {
		return panapi.File{}, syscall.ENOENT
	}
	return f, nil
}

func TestServiceListsDirectoryAndResolvesFile(t *testing.T) {
	api := &serviceAPI{
		files: map[int64][]panapi.File{
			0: {{ID: 1, Name: "photo.jpg", Size: 42}, {ID: 2, Name: "dir", IsDir: true}},
		},
		details: map[int64]panapi.File{
			1: {ID: 1, Name: "photo.jpg", Size: 42},
			2: {ID: 2, Name: "dir", IsDir: true},
		},
	}
	cache, err := storage.NewCache(t.TempDir(), 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	svc, err := NewService(context.Background(), api, cache, 0, false, Options{})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := svc.ListDirectory(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Name != "photo.jpg" || !entries[1].IsDir {
		t.Fatalf("ListDirectory = %#v", entries)
	}
	file, err := svc.File(context.Background(), 1)
	if err != nil || file == nil || file.Name != "photo.jpg" {
		t.Fatalf("File = %#v, err=%v", file, err)
	}
	// Opening a directory as a file is rejected before any network access.
	if _, _, err := svc.OpenFile(context.Background(), 2); err != syscall.EISDIR {
		t.Fatalf("OpenFile(dir) err = %v, want EISDIR", err)
	}
}
