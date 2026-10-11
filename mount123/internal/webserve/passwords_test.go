package webserve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/binghezhouke/123/mount123/internal/mountfs"
	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
)

type passwordFixtureAPI struct {
	mu             sync.Mutex
	files          map[int64][]panapi.File
	details        map[int64]panapi.File
	sharedPassword string
	sharedSaves    []struct {
		directoryID int64
		password    string
		overwrite   bool
	}
	archiveSaves []struct {
		archiveName string
		password    string
		overwrite   bool
	}
}

func (a *passwordFixtureAPI) CacheIdentity() string { return "webserve-passwords-test" }

func (a *passwordFixtureAPI) List(_ context.Context, id int64) ([]panapi.File, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]panapi.File(nil), a.files[id]...), nil
}
func (a *passwordFixtureAPI) DownloadURL(context.Context, int64) (string, error) {
	return "", syscall.EOPNOTSUPP
}
func (a *passwordFixtureAPI) Infos(_ context.Context, ids []int64) ([]panapi.File, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]panapi.File, 0, len(ids))
	for _, id := range ids {
		if f, ok := a.details[id]; ok {
			out = append(out, f)
		}
	}
	return out, nil
}
func (a *passwordFixtureAPI) Detail(_ context.Context, id int64) (panapi.File, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f, ok := a.details[id]
	if !ok {
		return panapi.File{}, syscall.ENOENT
	}
	return f, nil
}
func (a *passwordFixtureAPI) ReadSmallFile(_ context.Context, id, _ int64) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return []byte(a.sharedPassword), nil
}
func (a *passwordFixtureAPI) SaveSharedPassword(_ context.Context, dir panapi.File, password []byte, overwrite bool) (panapi.File, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sharedPassword = string(password)
	a.sharedSaves = append(a.sharedSaves, struct {
		directoryID int64
		password    string
		overwrite   bool
	}{dir.ID, string(password), overwrite})
	return panapi.File{ID: 9000, Name: ".mount123.pwd"}, false, nil
}
func (a *passwordFixtureAPI) SaveArchivePassword(_ context.Context, archive panapi.File, password []byte, overwrite bool) (panapi.File, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.archiveSaves = append(a.archiveSaves, struct {
		archiveName string
		password    string
		overwrite   bool
	}{archive.Name, string(password), overwrite})
	return panapi.File{ID: 9001, Name: archive.Name + ".pwd"}, false, nil
}

func newPasswordFixture(t *testing.T) (*Server, *passwordFixtureAPI) {
	t.Helper()
	api := &passwordFixtureAPI{
		files: map[int64][]panapi.File{
			0: {
				{ID: 1, Name: "photos.zip", Size: 100},
				{ID: 2, Name: "notes.txt", Size: 10},
			},
		},
		details: map[int64]panapi.File{
			1: {ID: 1, Name: "photos.zip", Size: 100, ParentID: 0},
			2: {ID: 2, Name: "notes.txt", Size: 10, ParentID: 0},
		},
	}
	cache, err := storage.NewCache(t.TempDir(), 8<<20)
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
	return server, api
}

func TestPasswordPageListsArchivesAndIsPlainText(t *testing.T) {
	server, _ := newPasswordFixture(t)
	recorder := authenticatedGET(t, server.Handler(), "/passwords", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "photos.zip") {
		t.Fatalf("password page does not list the archive:\n%s", body)
	}
	if strings.Contains(body, `type="password"`) {
		t.Fatalf("password page must render plain-text inputs:\n%s", body)
	}
	if !strings.Contains(body, `type="text"`) {
		t.Fatalf("password page is missing a plain-text input:\n%s", body)
	}
}

func TestSaveSharedPassword(t *testing.T) {
	server, api := newPasswordFixture(t)
	cookie := sessionCookie(t, login(t, server.Handler(), testPassword))
	form := url.Values{"parent_id": {"0"}, "password": {"共享密钥"}, "overwrite": {"1"}}
	request := httptest.NewRequest(http.MethodPost, "/passwords/shared", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303: %s", recorder.Code, recorder.Body.String())
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.sharedSaves) != 1 || api.sharedSaves[0].password != "共享密钥" || !api.sharedSaves[0].overwrite {
		t.Fatalf("shared saves = %#v", api.sharedSaves)
	}
}

func TestSaveArchivePassword(t *testing.T) {
	server, api := newPasswordFixture(t)
	cookie := sessionCookie(t, login(t, server.Handler(), testPassword))
	form := url.Values{"password": {"archive-pass"}}
	request := httptest.NewRequest(http.MethodPost, "/passwords/archive/1", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303: %s", recorder.Code, recorder.Body.String())
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.archiveSaves) != 1 || api.archiveSaves[0].archiveName != "photos.zip" || api.archiveSaves[0].password != "archive-pass" {
		t.Fatalf("archive saves = %#v", api.archiveSaves)
	}
}

func TestPasswordRoutesRequireAuth(t *testing.T) {
	server, _ := newPasswordFixture(t)
	recorder := get(t, server.Handler(), "/passwords", nil)
	if recorder.Code != http.StatusUnauthorized && recorder.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 401 or 303", recorder.Code)
	}
}
