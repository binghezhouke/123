package webserve

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/mountfs"
	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
)

// listingFixtureAPI is a small cloud API stand-in that serves directory
// listings and metadata from maps.
type listingFixtureAPI struct {
	url     string
	files   map[int64][]panapi.File
	details map[int64]panapi.File
	lists   atomic.Int32
}

func (a *listingFixtureAPI) CacheIdentity() string { return "webserve-listing-test" }

func (a *listingFixtureAPI) List(_ context.Context, id int64) ([]panapi.File, error) {
	a.lists.Add(1)
	return append([]panapi.File(nil), a.files[id]...), nil
}

func (a *listingFixtureAPI) DownloadURL(_ context.Context, id int64) (string, error) {
	return fmt.Sprintf("%s/%d", a.url, id), nil
}

func (a *listingFixtureAPI) Infos(_ context.Context, ids []int64) ([]panapi.File, error) {
	out := make([]panapi.File, 0, len(ids))
	for _, id := range ids {
		if file, ok := a.details[id]; ok {
			out = append(out, file)
		}
	}
	return out, nil
}

func (a *listingFixtureAPI) Detail(_ context.Context, id int64) (panapi.File, error) {
	file, ok := a.details[id]
	if !ok {
		return panapi.File{}, syscall.ENOENT
	}
	return file, nil
}

type listingFixture struct {
	server  *Server
	api     *listingFixtureAPI
	payload map[int64][]byte
}

// newListingFixture builds a real mountfs service over a fake cloud API and a
// local HTTP download server, so the handlers exercise the shared directory
// snapshot, disk cache and Range reads without touching the real account.
func newListingFixture(t *testing.T) *listingFixture {
	t.Helper()
	payload := map[int64][]byte{
		1: []byte("0123456789abcdefghij"),
		3: []byte("\x89PNG\r\n\x1a\npretend-image-bytes"),
		4: []byte("第一行\n第二行\n"),
		6: []byte("\x00\x01\x02opaque-binary-payload"),
	}
	download := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/"), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		body, ok := payload[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, fmt.Sprintf("file-%d.bin", id), time.Time{}, bytes.NewReader(body))
	}))
	t.Cleanup(download.Close)

	updated := time.Date(2026, 10, 9, 14, 30, 0, 0, time.UTC)
	api := &listingFixtureAPI{
		url: download.URL,
		files: map[int64][]panapi.File{
			0: {
				{ID: 2, Name: "Album", IsDir: true},
				{ID: 1, Name: "b-notes.txt", Size: int64(len(payload[1])), UpdatedAt: updated},
				{ID: 3, Name: "a-photo.png", Size: int64(len(payload[3]))},
			},
			2: {
				{ID: 7, Name: "Raw", IsDir: true},
				{ID: 5, Name: "clip.mp4", Size: 2048},
			},
			7: {
				{ID: 8, Name: "shot.jpg", Size: 1234},
			},
		},
		details: map[int64]panapi.File{
			1: {ID: 1, Name: "b-notes.txt", Size: int64(len(payload[1])), UpdatedAt: updated},
			2: {ID: 2, Name: "Album", IsDir: true},
			3: {ID: 3, Name: "a-photo.png", Size: int64(len(payload[3]))},
			4: {ID: 4, Name: "readme.txt", Size: int64(len(payload[4])), CreatedAt: updated, UpdatedAt: updated},
			5: {ID: 5, Name: "clip.mp4", Size: 2048},
			6: {ID: 6, Name: "blob.bin", Size: int64(len(payload[6]))},
			7: {ID: 7, Name: "Raw", IsDir: true, ParentID: 2},
			8: {ID: 8, Name: "shot.jpg", Size: 1234},
		},
	}
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
		// The mount root and the page's root ID are the same value in
		// production, so the fixture keeps them aligned too.
		opts.Info.RootID = 0
	})
	return &listingFixture{server: server, api: api, payload: payload}
}

// authenticatedGET performs one request inside a real login session.
func authenticatedGET(t *testing.T, handler http.Handler, path string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	cookie := sessionCookie(t, login(t, handler, testPassword))
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.AddCookie(cookie)
	for key, value := range header {
		request.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestBrowseRendersEntriesWithFoldersFirst(t *testing.T) {
	fixture := newListingFixture(t)
	recorder := authenticatedGET(t, fixture.server.Handler(), "/browse?sort=name", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, want := range []string{"Album", "a-photo.png", "b-notes.txt", "20 B", "文件夹"} {
		if !strings.Contains(body, want) {
			t.Fatalf("listing page does not contain %q:\n%s", want, body)
		}
	}
	album := strings.Index(body, "Album")
	photo := strings.Index(body, "a-photo.png")
	notes := strings.Index(body, "b-notes.txt")
	if !(album < photo && photo < notes) {
		t.Fatalf("name sort is wrong: Album=%d a-photo=%d b-notes=%d", album, photo, notes)
	}
	if !strings.Contains(body, `href="/browse?parent_id=2"`) {
		t.Fatalf("folder row does not link to its listing:\n%s", body)
	}
	if !strings.Contains(body, `href="/file/1"`) {
		t.Fatalf("file row does not link to its detail page:\n%s", body)
	}
}

func TestBrowseShowsBreadcrumbsAndSubdirectory(t *testing.T) {
	fixture := newListingFixture(t)
	recorder := authenticatedGET(t, fixture.server.Handler(), "/browse?parent_id=7", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "shot.jpg") {
		t.Fatalf("subdirectory listing is missing its entry:\n%s", body)
	}
	if !strings.Contains(body, `href="/browse?parent_id=2"`) {
		t.Fatalf("parent folder is missing from the breadcrumb:\n%s", body)
	}
	if !strings.Contains(body, ">Album</a>") {
		t.Fatalf("breadcrumb does not name the folder:\n%s", body)
	}
	if !strings.Contains(body, "<h1>Raw</h1>") {
		t.Fatalf("heading does not name the current folder:\n%s", body)
	}
}

func TestBrowseFiltersByMediaKind(t *testing.T) {
	fixture := newListingFixture(t)
	recorder := authenticatedGET(t, fixture.server.Handler(), "/browse?kind=image&sort=name", nil)
	body := recorder.Body.String()
	if !strings.Contains(body, "a-photo.png") {
		t.Fatalf("image filter dropped the image:\n%s", body)
	}
	if strings.Contains(body, "b-notes.txt") {
		t.Fatalf("image filter kept a non-image:\n%s", body)
	}
	if !strings.Contains(body, "已按筛选隐藏 1 项") {
		t.Fatalf("image filter did not report the hidden entry:\n%s", body)
	}
}

func TestBrowseSortsDescendingBySize(t *testing.T) {
	fixture := newListingFixture(t)
	recorder := authenticatedGET(t, fixture.server.Handler(), "/browse?sort=size&direction=desc", nil)
	body := recorder.Body.String()
	photo := strings.Index(body, "a-photo.png")
	notes := strings.Index(body, "b-notes.txt")
	if photo < 0 || notes < 0 || photo > notes {
		t.Fatalf("size sort is wrong: a-photo=%d b-notes=%d", photo, notes)
	}
}

func TestBrowseReusesDirectorySnapshot(t *testing.T) {
	fixture := newListingFixture(t)
	handler := fixture.server.Handler()
	first := authenticatedGET(t, handler, "/browse", nil)
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200", first.Code)
	}
	afterFirst := fixture.api.lists.Load()
	second := authenticatedGET(t, handler, "/browse", nil)
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, want 200", second.Code)
	}
	if afterFirst != fixture.api.lists.Load() {
		t.Fatalf("second browse called the list API again: %d then %d", afterFirst, fixture.api.lists.Load())
	}
}

func TestFileDetailEmbedsInlinePreview(t *testing.T) {
	fixture := newListingFixture(t)
	recorder := authenticatedGET(t, fixture.server.Handler(), "/file/3", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	body := recorder.Body.String()
	for _, want := range []string{"a-photo.png", `src="/file/3/preview"`, `href="/file/3/download"`, "图片"} {
		if !strings.Contains(body, want) {
			t.Fatalf("detail page does not contain %q:\n%s", want, body)
		}
	}
}

func TestFileDetailRedirectsDirectories(t *testing.T) {
	fixture := newListingFixture(t)
	recorder := authenticatedGET(t, fixture.server.Handler(), "/file/2", nil)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", recorder.Code)
	}
	if location := recorder.Header().Get("Location"); location != "/browse?parent_id=2" {
		t.Fatalf("Location = %q, want /browse?parent_id=2", location)
	}
}

func TestDownloadServesRange(t *testing.T) {
	fixture := newListingFixture(t)
	recorder := authenticatedGET(t, fixture.server.Handler(), "/file/1/download", map[string]string{"Range": "bytes=2-5"})
	if recorder.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", recorder.Code)
	}
	payload := fixture.payload[1]
	if want := fmt.Sprintf("bytes 2-5/%d", len(payload)); recorder.Header().Get("Content-Range") != want {
		t.Fatalf("Content-Range = %q, want %q", recorder.Header().Get("Content-Range"), want)
	}
	if recorder.Header().Get("Accept-Ranges") != "bytes" {
		t.Fatalf("Accept-Ranges = %q, want bytes", recorder.Header().Get("Accept-Ranges"))
	}
	if body := recorder.Body.String(); body != string(payload[2:6]) {
		t.Fatalf("range body = %q, want %q", body, payload[2:6])
	}
	disposition := recorder.Header().Get("Content-Disposition")
	if !strings.HasPrefix(disposition, "attachment;") || !strings.Contains(disposition, "b-notes.txt") {
		t.Fatalf("Content-Disposition = %q, want an attachment named b-notes.txt", disposition)
	}
}

func TestDownloadServesWholeFileAndRejectsBadRange(t *testing.T) {
	fixture := newListingFixture(t)
	handler := fixture.server.Handler()
	whole := authenticatedGET(t, handler, "/file/1/download", nil)
	if whole.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", whole.Code)
	}
	if body := whole.Body.String(); body != string(fixture.payload[1]) {
		t.Fatalf("body = %q, want the full payload", body)
	}
	invalid := authenticatedGET(t, handler, "/file/1/download", map[string]string{"Range": "bytes=500-600"})
	if invalid.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("status = %d, want 416", invalid.Code)
	}
	if want := fmt.Sprintf("bytes */%d", len(fixture.payload[1])); invalid.Header().Get("Content-Range") != want {
		t.Fatalf("Content-Range = %q, want %q", invalid.Header().Get("Content-Range"), want)
	}
}

// TestDownloadRangeOverHTTP drives the handler through a real HTTP server and
// client, which is the path a resumable `curl -r` download takes.
func TestDownloadRangeOverHTTP(t *testing.T) {
	fixture := newListingFixture(t)
	httpServer := httptest.NewServer(fixture.server.Handler())
	t.Cleanup(httpServer.Close)
	cookie := sessionCookie(t, login(t, fixture.server.Handler(), testPassword))

	request, err := http.NewRequest(http.MethodGet, httpServer.URL+"/file/1/download", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	request.AddCookie(cookie)
	request.Header.Set("Range", "bytes=5-9")
	response, err := httpServer.Client().Do(request)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", response.StatusCode)
	}
	payload := fixture.payload[1]
	if want := fmt.Sprintf("bytes 5-9/%d", len(payload)); response.Header.Get("Content-Range") != want {
		t.Fatalf("Content-Range = %q, want %q", response.Header.Get("Content-Range"), want)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != string(payload[5:10]) {
		t.Fatalf("range body = %q, want %q", body, payload[5:10])
	}
}

func TestPreviewServesImagesAndTextInline(t *testing.T) {
	fixture := newListingFixture(t)
	handler := fixture.server.Handler()
	image := authenticatedGET(t, handler, "/file/3/preview", nil)
	if image.Code != http.StatusOK {
		t.Fatalf("image status = %d, want 200", image.Code)
	}
	if contentType := image.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "image/png") {
		t.Fatalf("image Content-Type = %q, want image/png", contentType)
	}
	if disposition := image.Header().Get("Content-Disposition"); !strings.HasPrefix(disposition, "inline;") {
		t.Fatalf("image Content-Disposition = %q, want inline", disposition)
	}
	if body := image.Body.String(); body != string(fixture.payload[3]) {
		t.Fatalf("image body was rewritten: %q", body)
	}

	text := authenticatedGET(t, handler, "/file/4/preview", nil)
	if text.Code != http.StatusOK {
		t.Fatalf("text status = %d, want 200", text.Code)
	}
	if contentType := text.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/plain") {
		t.Fatalf("text Content-Type = %q, want text/plain", contentType)
	}
	if body := text.Body.String(); body != string(fixture.payload[4]) {
		t.Fatalf("text body = %q, want %q", body, fixture.payload[4])
	}
}

func TestPreviewRedirectsOpaqueFilesToDownload(t *testing.T) {
	fixture := newListingFixture(t)
	recorder := authenticatedGET(t, fixture.server.Handler(), "/file/6/preview", nil)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", recorder.Code)
	}
	if location := recorder.Header().Get("Location"); location != "/file/6/download" {
		t.Fatalf("Location = %q, want /file/6/download", location)
	}
}

func TestListingRoutesRequireSession(t *testing.T) {
	fixture := newListingFixture(t)
	handler := fixture.server.Handler()
	browser := get(t, handler, "/browse", map[string]string{"Accept": "text/html"})
	if browser.Code != http.StatusSeeOther {
		t.Fatalf("browser status = %d, want 303", browser.Code)
	}
	if location := browser.Header().Get("Location"); location != "/login?next=%2Fbrowse" {
		t.Fatalf("Location = %q, want /login?next=%%2Fbrowse", location)
	}
	client := get(t, handler, "/file/1/download", map[string]string{"Accept": "application/json"})
	if client.Code != http.StatusUnauthorized {
		t.Fatalf("client status = %d, want 401", client.Code)
	}
}

func TestMissingFileIsNotFound(t *testing.T) {
	fixture := newListingFixture(t)
	recorder := authenticatedGET(t, fixture.server.Handler(), "/file/99/download", nil)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
}

func TestListingPagesRenderWithoutService(t *testing.T) {
	server := newTestServer(t, nil)
	recorder := authenticatedGET(t, server.Handler(), "/browse", nil)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "网盘服务未配置") {
		t.Fatalf("missing explanation: %s", recorder.Body.String())
	}
}

func TestCompareNaturalOrdersDigitRunsNumerically(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"file2.txt", "file10.txt", -1},
		{"file10.txt", "file2.txt", 1},
		{"photo.jpg", "photo.jpg", 0},
		{"Photo.JPG", "photo.jpg", 0},
		{"a", "a1", -1},
		{"a1", "a", 1},
		{"007", "7", 0},
		{"note9", "note", 1},
	}
	for _, test := range cases {
		got := compareNatural(test.a, test.b)
		if sign(got) != test.want {
			t.Fatalf("compareNatural(%q, %q) = %d, want sign %d", test.a, test.b, got, test.want)
		}
	}
}

func sign(value int) int {
	switch {
	case value < 0:
		return -1
	case value > 0:
		return 1
	default:
		return 0
	}
}
