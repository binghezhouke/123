package mountfs

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
)

type probeServiceAPI struct {
	url   string
	files []panapi.File
}

func (a *probeServiceAPI) List(context.Context, int64) ([]panapi.File, error) {
	return append([]panapi.File(nil), a.files...), nil
}
func (a *probeServiceAPI) DownloadURL(_ context.Context, id int64) (string, error) {
	return fmt.Sprintf("%s/%d", a.url, id), nil
}
func (a *probeServiceAPI) Infos(context.Context, []int64) ([]panapi.File, error) {
	return append([]panapi.File(nil), a.files...), nil
}
func (a *probeServiceAPI) Detail(_ context.Context, id int64) (panapi.File, error) {
	for _, f := range a.files {
		if f.ID == id {
			return f, nil
		}
	}
	return panapi.File{}, fmt.Errorf("missing")
}

// fakeZIP builds a valid minimal ZIP so detectArchiveFormat recognises the
// file as a zip even though its cloud name has no .zip suffix.
func fakeZIP() []byte {
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	entry, _ := writer.Create("hello.txt")
	_, _ = entry.Write([]byte("hello"))
	_ = writer.Close()
	return buffer.Bytes()
}

func TestServiceProbesDirectoryAndReportsDetectedArchives(t *testing.T) {
	payload := fakeZIP()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "renamed.bin", time.Unix(100, 0), bytes.NewReader(payload))
	}))
	t.Cleanup(server.Close)
	api := &probeServiceAPI{
		url: server.URL,
		files: []panapi.File{
			{ID: 1, Name: "renamed", Size: int64(len(payload)), Version: "v1"},
			{ID: 2, Name: "notes.txt", Size: 10, Version: "v1"},
		},
	}
	cache, err := storage.NewCache(t.TempDir(), 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	service, err := NewService(context.Background(), api, cache, 0, true, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ProbeDirectory(context.Background(), 0); err != nil {
		t.Fatalf("ProbeDirectory: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if status := service.ProbeStatus(0); status.State == "complete" || status.State == "failed" || status.State == "cancelled" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	detected, err := service.DetectedArchives(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if detected[1] != ".zip" {
		t.Fatalf("DetectedArchives = %#v, want file 1 as .zip", detected)
	}
	if _, ok := detected[2]; ok {
		t.Fatalf("notes.txt should not be detected: %#v", detected)
	}
}
