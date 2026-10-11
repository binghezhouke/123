package webserve

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	payload := archiveBytes(t)
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
