package webserve

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/mountfs"
	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
)

func archiveBytes(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	file, err := writer.Create("pics/one.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("hello archive member")); err != nil {
		t.Fatal(err)
	}
	image, err := writer.Create("pics/cover.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := image.Write([]byte("fake image payload")); err != nil {
		t.Fatal(err)
	}
	video, err := writer.Create("clips/sample.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := video.Write([]byte("fake mp4 payload")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

type archiveFixtureAPI struct {
	url  string
	size int64
}

func (a *archiveFixtureAPI) CacheIdentity() string { return "webserve-archive-test" }
func (a *archiveFixtureAPI) List(_ context.Context, id int64) ([]panapi.File, error) {
	return []panapi.File{{ID: 1, Name: "photos.zip", Size: a.size, ParentID: 0}}, nil
}
func (a *archiveFixtureAPI) DownloadURL(_ context.Context, id int64) (string, error) {
	return fmt.Sprintf("%s/%d", a.url, id), nil
}
func (a *archiveFixtureAPI) Infos(_ context.Context, ids []int64) ([]panapi.File, error) {
	return []panapi.File{{ID: 1, Name: "photos.zip", Size: a.size, ParentID: 0}}, nil
}
func (a *archiveFixtureAPI) Detail(_ context.Context, id int64) (panapi.File, error) {
	return panapi.File{ID: 1, Name: "photos.zip", Size: a.size, ParentID: 0}, nil
}

func newArchiveFixture(t *testing.T) (*Server, []byte) {
	t.Helper()
	return newArchiveFixtureData(t, archiveBytes(t))
}

func newArchiveFixtureData(t *testing.T, payload []byte) (*Server, []byte) {
	t.Helper()
	download := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/"), 10, 64)
		if err != nil || id != 1 {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, "photos.zip", time.Time{}, bytes.NewReader(payload))
	}))
	t.Cleanup(download.Close)
	api := &archiveFixtureAPI{url: download.URL, size: int64(len(payload))}
	cache, err := storage.NewCache(t.TempDir(), 16<<20)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	service, err := mountfs.NewService(context.Background(), api, cache, 0, false, mountfs.Options{})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	server := newTestServer(t, func(opts *Options) {
		opts.Service = service
		opts.Info.RootID = 0
	})
	return server, payload
}

func TestArchivePagesEnumerateEveryMember(t *testing.T) {
	var payload bytes.Buffer
	w := zip.NewWriter(&payload)
	// Archive order differs from display order, with more than two pages.
	for i := 236; i >= 0; i-- {
		f, err := w.Create(fmt.Sprintf("%03d.jpg", i))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte("image")); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	server, _ := newArchiveFixtureData(t, payload.Bytes())
	first := authenticatedGET(t, server.Handler(), "/archive/1", nil)
	if first.Code != http.StatusOK {
		t.Fatal(first.Body.String())
	}
	if got := strings.Count(first.Body.String(), `class="file-grid-item file-item"`); got != 60 {
		t.Fatalf("initial rows=%d", got)
	}
	attr := `data-archive-next-cursor="`
	_, after, ok := strings.Cut(first.Body.String(), attr)
	if !ok {
		t.Fatal("missing initial cursor")
	}
	cursor, _, _ := strings.Cut(after, `"`)
	if cursor == "" {
		t.Fatal("initial page marked terminal")
	}
	count := 60
	seen := map[string]bool{}
	for cursor != "" {
		resp := authenticatedGET(t, server.Handler(), "/archive/1/entries?cursor="+url.QueryEscape(cursor), nil)
		if resp.Code != http.StatusOK {
			t.Fatalf("entries: %s", resp.Body.String())
		}
		var page struct {
			Items    []archiveEntryJSON `json:"items"`
			Next     string             `json:"next_cursor"`
			Complete bool               `json:"complete"`
			Total    int                `json:"total"`
		}
		if err := json.Unmarshal(resp.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if page.Total != 237 || !page.Complete || len(page.Items) == 0 || len(page.Items) > 100 {
			t.Fatalf("bad page: %+v", page)
		}
		for _, row := range page.Items {
			if seen[row.Name] || row.Name != fmt.Sprintf("%03d.jpg", count) {
				t.Fatalf("duplicate or missing member at %d: %s", count, row.Name)
			}
			seen[row.Name] = true
			count++
		}
		cursor = page.Next
		if count > 237 {
			t.Fatal("pagination did not terminate")
		}
	}
	if count != 237 {
		t.Fatalf("only %d members enumerated", count)
	}
}

func TestArchiveListRendersMembers(t *testing.T) {
	server, _ := newArchiveFixture(t)
	recorder := authenticatedGET(t, server.Handler(), "/archive/1", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "pics/") {
		t.Fatalf("archive page does not list the folder:\n%s", body)
	}
	imagePage := authenticatedGET(t, server.Handler(), "/archive/1?path=pics", nil)
	if imagePage.Code != http.StatusOK || !strings.Contains(imagePage.Body.String(), `class="browse-layout"`) || !strings.Contains(imagePage.Body.String(), `data-gallery-image`) || !strings.Contains(imagePage.Body.String(), `id="preview-pane"`) {
		t.Fatalf("archive image browser controls are missing:\n%s", imagePage.Body.String())
	}
}

func TestArchiveMemberPreview(t *testing.T) {
	server, _ := newArchiveFixture(t)
	recorder := authenticatedGET(t, server.Handler(), "/archive/1/member?path=pics/one.txt", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	if recorder.Body.String() != "hello archive member" {
		t.Fatalf("member body = %q", recorder.Body.String())
	}
	if recorder.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("content type = %q", recorder.Header().Get("Content-Type"))
	}
}

func TestArchiveVideoMemberPlaysInline(t *testing.T) {
	server, _ := newArchiveFixture(t)
	recorder := authenticatedGET(t, server.Handler(), "/archive/1/member?path=clips/sample.mp4", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "video/mp4" {
		t.Fatalf("content type = %q, want video/mp4", got)
	}
	if got := recorder.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "inline;") {
		t.Fatalf("content disposition = %q, want inline", got)
	}
}

func TestArchiveMissingMemberReturnsNotFound(t *testing.T) {
	server, _ := newArchiveFixture(t)
	recorder := authenticatedGET(t, server.Handler(), "/archive/1/member?path=nope.txt", nil)
	if recorder.Code != http.StatusBadRequest && recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 400/404: %s", recorder.Code, recorder.Body.String())
	}
}

func TestArchiveRoutesRequireAuth(t *testing.T) {
	server, _ := newArchiveFixture(t)
	if recorder := get(t, server.Handler(), "/archive/1", nil); recorder.Code != http.StatusUnauthorized && recorder.Code != http.StatusSeeOther {
		t.Fatalf("archive list status = %d", recorder.Code)
	}
	if recorder := get(t, server.Handler(), "/archive/1/member?path=x", nil); recorder.Code != http.StatusUnauthorized && recorder.Code != http.StatusSeeOther {
		t.Fatalf("archive member status = %d", recorder.Code)
	}
}
