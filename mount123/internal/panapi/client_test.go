package panapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestListFetchesAllPagesAndFiltersTrashed(t *testing.T) {
	tokenCalls := 0
	pageCalls := 0
	requestTimes := []time.Time{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestTimes = append(requestTimes, time.Now())
		switch r.URL.Path {
		case "/api/v1/access_token":
			tokenCalls++
			if r.Header.Get("Platform") != "open_platform" {
				t.Errorf("token Platform header = %q", r.Header.Get("Platform"))
			}
			if r.Method != http.MethodPost {
				t.Errorf("token method = %s", r.Method)
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"test-token","expiresIn":3600}}`))
		case "/api/v2/file/list":
			pageCalls++
			if r.Header.Get("Authorization") != "Bearer test-token" || r.Header.Get("Platform") != "open_platform" {
				t.Errorf("missing API headers: %#v", r.Header)
			}
			if r.URL.Query().Get("limit") != "100" || r.URL.Query().Get("parentFileId") != "42" {
				t.Errorf("unexpected list query: %s", r.URL.RawQuery)
			}
			if pageCalls == 1 {
				_, _ = w.Write([]byte(`{"code":0,"data":{"fileList":[{"fileId":1,"filename":"a.txt","size":12,"type":0,"etag":"abc","updateAt":"now"},{"fileId":2,"filename":"old","trashed":1}],"lastFileId":9}}`))
				return
			}
			if r.URL.Query().Get("lastFileId") != "9" {
				t.Errorf("next cursor = %q", r.URL.Query().Get("lastFileId"))
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"fileList":[{"fileId":3,"filename":"folder","size":0,"type":1,"etag":"dir","updateAt":"later"}],"lastFileId":-1}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	cache := filepath.Join(t.TempDir(), "tokens", "token.json")
	c, err := New(Config{ClientID: "client-A", ClientSecret: "secret", BaseURL: srv.URL, TokenCache: cache})
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.List(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	want := []File{
		{ID: 1, ParentID: 42, Name: "a.txt", Size: 12, IsDir: false, Version: "abc:12"},
		{ID: 3, ParentID: 42, Name: "folder", Size: 0, IsDir: true, Version: "dir:0"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("List() = %#v; want %#v", got, want)
	}
	if tokenCalls != 1 || pageCalls != 2 {
		t.Fatalf("calls: token=%d pages=%d", tokenCalls, pageCalls)
	}
	if gap := requestTimes[2].Sub(requestTimes[1]); gap < 300*time.Millisecond {
		t.Fatalf("v2 list requests were only %s apart", gap)
	}
	info, err := os.Stat(cache)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("token cache mode = %o, want 600", info.Mode().Perm())
	}
	if d, err := os.Stat(filepath.Dir(cache)); err != nil || d.Mode().Perm() != 0700 {
		t.Fatalf("token cache directory mode not 700: %v, %v", d, err)
	}
}

func TestExplicitAccessTokenAndDownloadURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/file/download_info" || r.URL.Query().Get("fileId") != strconv.Itoa(77) {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
		}
		if r.Header.Get("Authorization") != "Bearer env-token" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"downloadUrl":"https://download.example/signed?token=abc"}}`))
	}))
	defer srv.Close()
	c, err := New(Config{AccessToken: "env-token", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.DownloadURL(context.Background(), 77)
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://download.example/signed?token=abc" {
		t.Fatalf("DownloadURL() = %q", got)
	}
}

func TestCacheIdentityScopesExplicitTokenAlongsideSharedClient(t *testing.T) {
	first, err := New(Config{ClientID: "shared-app", ClientSecret: "shared-secret", AccessToken: "account-one"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(Config{ClientID: "shared-app", ClientSecret: "shared-secret", AccessToken: "account-two"})
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := New(Config{ClientID: "shared-app", ClientSecret: "shared-secret", AccessToken: "account-one"})
	if err != nil {
		t.Fatal(err)
	}
	if first.CacheIdentity() == second.CacheIdentity() {
		t.Fatal("different explicit accounts share a cache identity")
	}
	if first.CacheIdentity() != reopened.CacheIdentity() {
		t.Fatal("same explicit account did not retain its cache identity")
	}
	if strings.Contains(first.CacheIdentity(), "account-one") {
		t.Fatal("cache identity exposes the access token")
	}
}

func TestDetailRequiresValidParentIDBeforePasswordCanBeSaved(t *testing.T) {
	var uploadCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/file/detail":
			parent := `"bad"`
			if r.URL.Query().Get("fileID") == "2" {
				parent = `-1`
			}
			if r.URL.Query().Get("fileID") == "3" {
				_, _ = w.Write([]byte(`{"code":0,"data":{"fileID":3,"filename":"archive.zip","size":10,"type":0}}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"fileID":` + r.URL.Query().Get("fileID") + `,"parentFileID":` + parent + `,"filename":"archive.zip","size":10,"type":0}}`))
		case "/upload/v2/file/create":
			uploadCalls++
			_, _ = w.Write([]byte(`{"code":0,"data":{"reuse":true,"fileID":90}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	for _, id := range []int64{1, 2, 3} {
		c, _ := New(Config{AccessToken: "token", BaseURL: srv.URL})
		archive, err := c.Detail(context.Background(), id)
		if err == nil {
			_, err = c.SaveZIPPassword(context.Background(), archive, []byte("pw"))
		}
		if err == nil {
			t.Errorf("Detail(%d) unexpectedly allowed password save", id)
		}
	}
	if uploadCalls != 0 {
		t.Fatalf("malformed parent metadata reached upload endpoint %d times", uploadCalls)
	}
}

func TestInfosRejectsMalformedParentIDWhenPresent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"fileList":[{"fileId":7,"parentFileID":"bad","filename":"x","size":1,"type":0}]}}`))
	}))
	defer srv.Close()
	c, _ := New(Config{AccessToken: "token", BaseURL: srv.URL})
	if _, err := c.Infos(context.Background(), []int64{7}); err == nil {
		t.Fatal("Infos accepted malformed parentFileID")
	}
}

func TestUnauthorizedRefreshesOnce(t *testing.T) {
	tokenCalls, listCalls := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/access_token" {
			tokenCalls++
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"accessToken": "token-" + strconv.Itoa(tokenCalls), "expiresIn": 3600}})
			return
		}
		listCalls++
		if r.Header.Get("Authorization") == "Bearer token-1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"fileList":[],"lastFileId":-1}}`))
	}))
	defer srv.Close()
	c, err := New(Config{ClientID: "id", ClientSecret: "secret", BaseURL: srv.URL, TokenCache: filepath.Join(t.TempDir(), "token.json")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.List(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if tokenCalls != 2 || listCalls != 2 {
		t.Fatalf("calls: token=%d list=%d", tokenCalls, listCalls)
	}
}

func TestRepeatedPaginationCursorFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"fileList":[{"fileId":1}],"lastFileId":9}}`))
	}))
	defer srv.Close()
	c, err := New(Config{AccessToken: "token", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.List(context.Background(), 0)
	if err == nil {
		t.Fatal("expected repeated cursor error")
	}
}

func TestListContinuesAfterEmptyPageWithCursor(t *testing.T) {
	page := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page++
		if page == 1 {
			_, _ = w.Write([]byte(`{"code":0,"data":{"fileList":[],"lastFileId":8}}`))
			return
		}
		if r.URL.Query().Get("lastFileId") != "8" {
			t.Errorf("cursor = %q, want 8", r.URL.Query().Get("lastFileId"))
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"fileList":[{"fileId":5,"filename":"later","size":3,"type":0}],"lastFileId":-1}}`))
	}))
	defer srv.Close()
	c, err := New(Config{AccessToken: "token", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	files, err := c.List(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if page != 2 || len(files) != 1 || files[0].ID != 5 {
		t.Fatalf("pages=%d files=%#v", page, files)
	}
}

func TestThrottleRetriesAndMetadataEndpoints(t *testing.T) {
	listCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/file/list":
			listCalls++
			if listCalls == 1 {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			w.Header().Set("Retry-After", "0")
			_, _ = w.Write([]byte(`{"code":429,"message":"slow down"}`))
		case "/api/v1/file/infos":
			if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("unexpected infos request: %s %#v", r.Method, r.Header)
			}
			var request struct {
				FileIDs []int64 `json:"fileIds"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || !reflect.DeepEqual(request.FileIDs, []int64{7}) {
				t.Errorf("fileIds=%v err=%v", request.FileIDs, err)
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"fileList":[{"fileId":7,"filename":"gone","size":5,"type":0,"trashed":1,"createAt":"2025-01-02 03:04:05","updateAt":1760000000000}]}}`))
		case "/api/v1/file/detail":
			if r.URL.Query().Get("fileID") != "7" {
				t.Errorf("fileID=%q", r.URL.Query().Get("fileID"))
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"fileID":7,"parentFileID":8,"filename":"gone","size":5,"type":0,"trashed":1,"etag":"e"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, err := New(Config{AccessToken: "token", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.List(context.Background(), 0); err == nil || listCalls != 5 {
		t.Fatalf("body 429 retries: calls=%d err=%v", listCalls, err)
	}
	duplicateIDs := make([]int64, 101)
	for i := range duplicateIDs {
		duplicateIDs[i] = 7
	}
	infos, err := c.Infos(context.Background(), duplicateIDs)
	if err != nil {
		t.Fatal(err)
	}
	china := time.FixedZone("China Standard Time", 8*60*60)
	created, _ := time.ParseInLocation("2006-01-02 15:04:05", "2025-01-02 03:04:05", china)
	if len(infos) != 1 || !infos[0].Trashed || !infos[0].CreatedAt.Equal(created) || !infos[0].UpdatedAt.Equal(time.UnixMilli(1760000000000)) || infos[0].Version != ":5" {
		t.Fatalf("Infos()=%#v", infos)
	}
	detail, err := c.Detail(context.Background(), 7)
	if err != nil || !detail.Trashed || detail.Version != "e:5" {
		t.Fatalf("Detail()=%#v err=%v", detail, err)
	}
	if empty, err := c.Infos(context.Background(), nil); err != nil || len(empty) != 0 {
		t.Fatalf("Infos(empty)=%#v err=%v", empty, err)
	}
	for _, id := range []int64{0, -1} {
		if _, err := c.Infos(context.Background(), []int64{id}); err == nil {
			t.Errorf("Infos(%d) succeeded", id)
		}
		if _, err := c.Detail(context.Background(), id); err == nil {
			t.Errorf("Detail(%d) succeeded", id)
		}
	}
}

func TestConcurrentUnauthorizedResponsesShareRefresh(t *testing.T) {
	var mu sync.Mutex
	tokenCalls, oldRequests, allRequests := 0, 0, 0
	seenTwoOld := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		switch r.URL.Path {
		case "/api/v1/access_token":
			tokenCalls++
			call := tokenCalls
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"accessToken": "token-" + strconv.Itoa(call), "expiresIn": 3600}})
			return
		case "/api/v2/file/list":
			allRequests++
			if r.Header.Get("Authorization") == "Bearer token-1" {
				oldRequests++
				if oldRequests == 2 {
					close(seenTwoOld)
				}
				mu.Unlock()
				<-release
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			mu.Unlock()
			_, _ = w.Write([]byte(`{"code":0,"data":{"fileList":[],"lastFileId":-1}}`))
			return
		}
		mu.Unlock()
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c, err := New(Config{ClientID: "id", ClientSecret: "secret", BaseURL: srv.URL, TokenCache: filepath.Join(t.TempDir(), "token.json")})
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { _, err := c.List(context.Background(), 0); results <- err }()
	}
	select {
	case <-seenTwoOld:
		close(release)
	case <-time.After(5 * time.Second):
		t.Fatal("did not observe concurrent old-token requests")
	}
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if tokenCalls != 2 || allRequests != 4 {
		t.Fatalf("token calls=%d API calls=%d", tokenCalls, allRequests)
	}
}

func TestThrottleWaitHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitThrottle(ctx, 1, "10"); err != context.Canceled {
		t.Fatalf("waitThrottle err=%v", err)
	}
	if err := waitThrottle(context.Background(), 1, "999999999999999999999999"); err == nil {
		t.Fatal("expected excessive Retry-After to fail without retrying early")
	}
}

func TestSaveZIPPasswordUsesUploadReuse(t *testing.T) {
	createCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/file/list":
			_, _ = w.Write([]byte(`{"code":0,"data":{"fileList":[{"fileId":20,"parentFileID":8,"filename":"archive.zip","size":10,"type":0}],"lastFileId":-1}}`))
		case "/upload/v2/file/create":
			createCalls++
			var payload struct {
				ParentID  int64  `json:"parentFileID"`
				Filename  string `json:"filename"`
				Duplicate int    `json:"duplicate"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			if payload.ParentID != 8 || payload.Filename != "archive.zip.pwd" || payload.Duplicate != 2 {
				t.Errorf("create payload = %#v", payload)
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"reuse":true,"fileID":21}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, _ := New(Config{AccessToken: "secret-token", BaseURL: srv.URL})
	got, err := c.SaveZIPPassword(context.Background(), File{ID: 20, ParentID: 8, Name: "archive.zip"}, []byte("unlock"))
	if err != nil || got.ID != 21 || got.ParentID != 8 || got.Name != "archive.zip.pwd" {
		t.Fatalf("SaveZIPPassword()=%#v err=%v", got, err)
	}
	if createCalls != 1 {
		t.Fatalf("create calls=%d", createCalls)
	}
}

func TestSaveZIPPasswordValidatesSidecarNameBeforeNetwork(t *testing.T) {
	c, _ := New(Config{AccessToken: "token", BaseURL: "http://127.0.0.1:1"})
	for _, name := range []string{"archive.txt", "bad?.zip", strings.Repeat("a", 248) + ".zip"} {
		if _, err := c.SaveZIPPassword(context.Background(), File{ID: 1, ParentID: 0, Name: name}, []byte("pw")); err == nil {
			t.Errorf("SaveZIPPassword accepted invalid archive name %q", name)
		}
	}
}

func TestSaveZIPPasswordDetectsDuplicateNamesBeforeComparingContent(t *testing.T) {
	downloadInfoCalls := 0
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/file/list":
			_, _ = w.Write([]byte(`{"code":0,"data":{"fileList":[{"fileId":20,"parentFileID":8,"filename":"archive.zip","size":10,"type":0},{"fileId":21,"parentFileID":8,"filename":"archive.zip.pwd","size":2,"type":0},{"fileId":22,"parentFileID":8,"filename":"archive.zip.pwd","size":2,"type":0}],"lastFileId":-1}}`))
		case "/api/v1/file/download_info":
			downloadInfoCalls++
			_, _ = w.Write([]byte(`{"code":0,"data":{"downloadUrl":"` + srv.URL + `/download"}}`))
		case "/download":
			_, _ = w.Write([]byte("pw"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, _ := New(Config{AccessToken: "token", BaseURL: srv.URL})
	_, err := c.SaveZIPPassword(context.Background(), File{ID: 20, ParentID: 8, Name: "archive.zip"}, []byte("pw"))
	if err == nil || !strings.Contains(err.Error(), "multiple sibling") {
		t.Fatalf("SaveZIPPassword error = %v, want duplicate sibling conflict", err)
	}
	if downloadInfoCalls != 0 {
		t.Fatalf("compared first duplicate before scanning all names: download calls=%d", downloadInfoCalls)
	}
}

func TestSaveZIPPasswordPreservesCancellationWhileComparing(t *testing.T) {
	started := make(chan struct{})
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/file/list":
			_, _ = w.Write([]byte(`{"code":0,"data":{"fileList":[{"fileId":20,"parentFileID":8,"filename":"archive.zip","size":10,"type":0},{"fileId":21,"parentFileID":8,"filename":"archive.zip.pwd","size":2,"type":0}],"lastFileId":-1}}`))
		case "/api/v1/file/download_info":
			_, _ = w.Write([]byte(`{"code":0,"data":{"downloadUrl":"` + srv.URL + `/download"}}`))
		case "/download":
			close(started)
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, _ := New(Config{AccessToken: "token", BaseURL: srv.URL})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := c.SaveZIPPassword(ctx, File{ID: 20, ParentID: 8, Name: "archive.zip"}, []byte("pw"))
		result <- err
	}()
	<-started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("SaveZIPPassword cancellation error = %v", err)
	}
}

func TestReadSmallFileIsBoundedAndDoesNotUseRange(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/file/download_info":
			target := "/download"
			if r.URL.Query().Get("fileId") == "3" {
				target = "/large"
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"downloadUrl":"` + srv.URL + target + `"}}`))
		case "/download":
			if r.Header.Get("Range") != "" {
				t.Errorf("unexpected Range header %q", r.Header.Get("Range"))
			}
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte("lo"))
		case "/large":
			if r.Header.Get("Range") != "" {
				t.Errorf("unexpected Range header %q", r.Header.Get("Range"))
			}
			_, _ = w.Write([]byte("long"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, _ := New(Config{AccessToken: "token", BaseURL: srv.URL})
	if _, err := c.ReadSmallFile(context.Background(), 2, 3); err == nil {
		t.Fatal("expected partial response to fail")
	}
	if _, err := c.ReadSmallFile(context.Background(), 3, 3); err == nil {
		t.Fatal("expected over-limit response to fail")
	}
}

func TestSaveZIPPasswordUploadsSliceAndReusesMatchingSidecar(t *testing.T) {
	var uploaded bool
	var createCalls, sliceCalls, completeCalls int
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/file/list":
			if uploaded {
				_, _ = w.Write([]byte(`{"code":0,"data":{"fileList":[{"fileId":20,"parentFileID":8,"filename":"archive.zip","size":10,"type":0},{"fileId":22,"parentFileID":8,"filename":"archive.zip.pwd","size":6,"type":0}],"lastFileId":-1}}`))
			} else {
				_, _ = w.Write([]byte(`{"code":0,"data":{"fileList":[{"fileId":20,"parentFileID":8,"filename":"archive.zip","size":10,"type":0},{"fileId":21,"parentFileID":8,"filename":"archive.zip.pwd","size":3,"type":0}],"lastFileId":-1}}`))
			}
		case "/api/v1/file/download_info":
			_, _ = w.Write([]byte(`{"code":0,"data":{"downloadUrl":"` + srv.URL + `/download"}}`))
		case "/download":
			if uploaded {
				_, _ = w.Write([]byte("unlock"))
			} else {
				_, _ = w.Write([]byte("old"))
			}
		case "/upload/v2/file/create":
			createCalls++
			var payload struct {
				Duplicate int `json:"duplicate"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			if payload.Duplicate != 2 {
				t.Errorf("duplicate=%d, want overwrite only the existing sibling", payload.Duplicate)
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"preuploadID":"pre-1","sliceSize":3,"servers":["` + srv.URL + `"]}}`))
		case "/upload/v2/file/slice":
			sliceCalls++
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Error(err)
			}
			file, _, err := r.FormFile("slice")
			if err != nil {
				t.Error(err)
				return
			}
			defer file.Close()
			part, _ := io.ReadAll(file)
			if string(part) != "unl" && string(part) != "ock" {
				t.Errorf("slice bytes = %q", part)
			}
			_, _ = w.Write([]byte(`{"code":0}`))
		case "/upload/v2/file/upload_complete":
			completeCalls++
			if completeCalls < 3 {
				_, _ = w.Write([]byte(`{"code":20103,"message":"file checking"}`))
				return
			}
			uploaded = true
			_, _ = w.Write([]byte(`{"code":0,"data":{"completed":true,"fileID":22}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, _ := New(Config{AccessToken: "secret-token", BaseURL: srv.URL})
	archive := File{ID: 20, ParentID: 8, Name: "archive.zip"}
	got, err := c.SaveZIPPassword(context.Background(), archive, []byte("unlock"))
	if err != nil || got.ID != 22 {
		t.Fatalf("SaveZIPPassword()=%#v err=%v", got, err)
	}
	if createCalls != 1 || sliceCalls != 2 || completeCalls != 3 {
		t.Fatalf("create=%d slices=%d complete=%d", createCalls, sliceCalls, completeCalls)
	}
	got, err = c.SaveZIPPassword(context.Background(), archive, []byte("unlock"))
	if err != nil || got.ID != 22 {
		t.Fatalf("idempotent SaveZIPPassword()=%#v err=%v", got, err)
	}
	if createCalls != 1 {
		t.Fatalf("matching sidecar was uploaded again: create calls=%d", createCalls)
	}
}

func TestSaveZIPPasswordRejectsUnconfirmedCompletion(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/file/list":
			_, _ = w.Write([]byte(`{"code":0,"data":{"fileList":[{"fileId":20,"parentFileID":8,"filename":"archive.zip","size":10,"type":0}],"lastFileId":-1}}`))
		case "/upload/v2/file/create":
			_, _ = w.Write([]byte(`{"code":0,"data":{"preuploadID":"pre-1","sliceSize":20,"servers":["` + srv.URL + `"]}}`))
		case "/upload/v2/file/slice":
			_, _ = w.Write([]byte(`{"code":0}`))
		case "/upload/v2/file/upload_complete":
			_, _ = w.Write([]byte(`{"code":0,"data":{"completed":false}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, _ := New(Config{AccessToken: "token", BaseURL: srv.URL})
	_, err := c.SaveZIPPassword(context.Background(), File{ID: 20, ParentID: 8, Name: "archive.zip"}, []byte("pw"))
	if err == nil || !strings.Contains(err.Error(), "not confirmed") {
		t.Fatalf("SaveZIPPassword error = %v, want unconfirmed completion", err)
	}
}

func TestCompletionVerificationRetriesAreBounded(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"code":20103,"message":"file checking"}`))
	}))
	defer srv.Close()
	c, _ := New(Config{AccessToken: "token", BaseURL: srv.URL})
	var response struct{}
	err := c.requestJSON(context.Background(), http.MethodPost, "/upload/v2/file/upload_complete", nil, []byte(`{"preuploadID":"p"}`), &response)
	if err == nil || calls != 6 {
		t.Fatalf("completion verification retries: calls=%d err=%v, want six bounded attempts", calls, err)
	}
}

func TestListRejectsMalformedOrNegativeMetadata(t *testing.T) {
	for _, item := range []string{
		`{"fileId":"bad","filename":"x","size":1,"type":0}`,
		`{"fileId":-1,"filename":"x","size":1,"type":0}`,
		`{"fileId":1,"filename":"x","size":"bad","type":0}`,
		`{"fileId":1,"filename":"x","size":-1,"type":0}`,
		`{"fileId":1,"filename":"x","size":1,"type":"bad"}`,
		`{"fileId":1,"filename":"x","size":1,"type":-1}`,
	} {
		t.Run(item, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"code":0,"data":{"fileList":[` + item + `],"lastFileId":-1}}`))
			}))
			defer srv.Close()
			c, err := New(Config{AccessToken: "token", BaseURL: srv.URL})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.List(context.Background(), 0); err == nil {
				t.Fatal("expected malformed metadata error")
			}
		})
	}
}
