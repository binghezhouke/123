package webserve

import (
	"archive/zip"
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/mountfs"
	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
)

func TestImageResourceSharingBoundaries(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is required for image resource boundary tests")
	}
	out, err := exec.Command(node, "--test", "../../../tests/js/image_resources.test.cjs").CombinedOutput()
	if err != nil {
		t.Fatalf("image resource boundaries: %v\n%s", err, out)
	}
}

func TestThumbnailOriginalReuseInBrowser(t *testing.T) {
	python := os.Getenv("MOUNT123_BROWSER_PYTHON")
	if python == "" {
		t.Skip("set MOUNT123_BROWSER_PYTHON to a Python with Playwright")
	}
	var picture, archive bytes.Buffer
	bitmap := image.NewRGBA(image.Rect(0, 0, 64, 48))
	for y := 0; y < 48; y++ {
		for x := 0; x < 64; x++ {
			bitmap.Set(x, y, color.RGBA{uint8(x * 3), uint8(y * 4), 160, 255})
		}
	}
	if err := png.Encode(&picture, bitmap); err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(&archive)
	w, err := zw.Create("one.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Write(picture.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err = zw.Close(); err != nil {
		t.Fatal(err)
	}
	server, _ := newArchiveFixtureData(t, archive.Bytes())
	download := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		http.ServeContent(w, r, "one.png", time.Time{}, bytes.NewReader(picture.Bytes()))
	}))
	defer download.Close()
	file := panapi.File{ID: 3, Name: "one.png", Size: int64(picture.Len())}
	api := &listingFixtureAPI{url: download.URL, files: map[int64][]panapi.File{0: {file}}, details: map[int64]panapi.File{3: file}}
	cache, err := storage.NewCache(t.TempDir(), 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	service, err := mountfs.NewService(context.Background(), api, cache, 0, false, mountfs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	listing := newTestServer(t, func(opts *Options) { opts.Service = service; opts.Info.RootID = 0 })
	for _, tc := range []struct {
		name, path string
		server     *Server
	}{{"archive", "/archive/1", server}, {"directory", "/browse?parent_id=0", listing}} {
		t.Run(tc.name, func(t *testing.T) {
			tc.server.authEnabled = false
			host := httptest.NewServer(tc.server.Handler())
			defer host.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, python, "testdata/image_cache_browser_test.py", host.URL, tc.path).CombinedOutput()
			if err != nil {
				t.Fatalf("thumbnail to original browser reuse: %v\n%s", err, out)
			}
			t.Log(string(out))
		})
	}
}
