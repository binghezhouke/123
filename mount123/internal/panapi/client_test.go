package panapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
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
		{ID: 1, Name: "a.txt", Size: 12, IsDir: false, Version: "abc:12:now"},
		{ID: 3, Name: "folder", Size: 0, IsDir: true, Version: "dir:0:later"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("List() = %#v; want %#v", got, want)
	}
	if tokenCalls != 1 || pageCalls != 2 {
		t.Fatalf("calls: token=%d pages=%d", tokenCalls, pageCalls)
	}
	if gap := requestTimes[1].Sub(requestTimes[0]); gap < 300*time.Millisecond {
		t.Fatalf("token and first API request were only %s apart", gap)
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
