package webserve

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
)

func TestSelectedImageOvertakesBackgroundInBrowser(t *testing.T) {
	python := os.Getenv("MOUNT123_BROWSER_PYTHON")
	if python == "" {
		t.Skip("set MOUNT123_BROWSER_PYTHON to a Python with Playwright")
	}
	listing := newListingFixture(t)
	listing.api.files[7] = nil
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	for i := 0; i < 12; i++ {
		var picture bytes.Buffer
		if err := png.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 64+i, 48))); err != nil {
			t.Fatal(err)
		}
		name := fmt.Sprintf("%02d.png", i)
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(picture.Bytes()); err != nil {
			t.Fatal(err)
		}
		id := int64(101 + i)
		listing.payload[id] = picture.Bytes()
		file := panapi.File{ID: id, ParentID: 7, Name: name, Size: int64(picture.Len())}
		listing.api.files[7] = append(listing.api.files[7], file)
		listing.api.details[id] = file
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	archiveServer, _ := newArchiveFixtureData(t, archive.Bytes())
	for _, tc := range []struct {
		name, path string
		server     *Server
	}{
		{"archive", "/archive/1", archiveServer}, {"directory", "/browse?parent_id=7", listing.server},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.server.authEnabled = false
			release := make(chan struct{})
			handler := tc.server.Handler()
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				imageRequest := strings.HasSuffix(r.URL.Path, "/member") || strings.HasSuffix(r.URL.Path, "/preview")
				if imageRequest && (r.Header.Get("X-Priority") == "background" || r.URL.Query().Get("direct") == "1") {
					// Model a slow speculative read. It cannot deliver an image
					// until test cleanup; the selected request must overtake it.
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
				}
				handler.ServeHTTP(w, r)
			}))
			defer host.Close()
			defer close(release)
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, python, "testdata/image_priority_browser_test.py", host.URL, tc.path).CombinedOutput()
			if err != nil {
				t.Fatalf("foreground priority browser: %v\n%s", err, out)
			}
			t.Log(string(out))
		})
	}
}
