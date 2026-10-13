package webserve

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/mountfs"
	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
)

type pendingArchiveAPI struct{ *archiveFixtureAPI }

func (a *pendingArchiveAPI) file() panapi.File {
	return panapi.File{ID: 1, Name: "pending.7z", Size: a.size, Version: "synthetic-v1"}
}
func (a *pendingArchiveAPI) List(context.Context, int64) ([]panapi.File, error) {
	return []panapi.File{a.file()}, nil
}
func (a *pendingArchiveAPI) Detail(context.Context, int64) (panapi.File, error) {
	return a.file(), nil
}
func (a *pendingArchiveAPI) Infos(context.Context, []int64) ([]panapi.File, error) {
	return []panapi.File{a.file()}, nil
}

// Delay only the external archive source. The real Service owns its normal
// five-second index wait and the shared task which survives that wait.
func TestArchivePendingPageAndEntriesResume(t *testing.T) {
	payload, err := os.ReadFile("../mountfs/testdata/encrypted/plain.7z")
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(ready) }) }
	download := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-ready:
			http.ServeContent(w, r, "pending.7z", time.Unix(100, 0), bytes.NewReader(payload))
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(download.Close)
	cache, err := storage.NewCache(t.TempDir(), 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	lifetime, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	t.Cleanup(unblock)
	api := &pendingArchiveAPI{&archiveFixtureAPI{url: download.URL, size: int64(len(payload))}}
	service, err := mountfs.NewService(lifetime, api, cache, 0, true, mountfs.Options{MaxConcurrentBuilds: 1})
	if err != nil {
		t.Fatal(err)
	}
	server := newTestServer(t, func(opts *Options) { opts.Service = service })

	started := time.Now()
	page := authenticatedGET(t, server.Handler(), "/archive/1", nil)
	t.Logf("initial page status=%d elapsed=%s", page.Code, time.Since(started))
	if page.Code != http.StatusOK {
		t.Errorf("pending initial page status=%d, want 200 loading shell", page.Code)
	}
	if !strings.Contains(page.Body.String(), "data-archive-sentinel") || !strings.Contains(page.Body.String(), `data-archive-complete="false"`) || !strings.Contains(page.Body.String(), "正在读取目录") {
		t.Error("pending initial page lacks existing loading state")
	}
	if strings.Contains(page.Body.String(), "文件夹为空") {
		t.Error("pending initial page incorrectly declares an empty directory")
	}

	started = time.Now()
	pending := authenticatedGET(t, server.Handler(), "/archive/1/entries?cursor=synthetic-cursor", nil)
	t.Logf("pending entries status=%d elapsed=%s", pending.Code, time.Since(started))
	if pending.Code != http.StatusAccepted {
		t.Errorf("pending entries status=%d, want 202", pending.Code)
	}
	if pending.Header().Get("Retry-After") != "1" {
		t.Error("pending entries lack bounded Retry-After")
	}
	var waiting struct {
		Code       string `json:"code"`
		Retryable  bool   `json:"retryable"`
		Complete   bool   `json:"complete"`
		NextCursor string `json:"next_cursor"`
	}
	if err := json.Unmarshal(pending.Body.Bytes(), &waiting); err != nil {
		t.Errorf("pending entries are not JSON: %v", err)
	} else if waiting.Code != "archive_index_pending" || !waiting.Retryable || waiting.Complete || waiting.NextCursor != "synthetic-cursor" {
		t.Errorf("pending entries lost retry/cursor state: %+v", waiting)
	}

	unblock()
	resolved := authenticatedGET(t, server.Handler(), "/archive/1/entries", nil)
	if resolved.Code != http.StatusOK {
		t.Fatalf("ready entries status=%d", resolved.Code)
	}
	var result struct {
		Items    []archiveEntryJSON `json:"items"`
		Total    int                `json:"total"`
		Complete bool               `json:"complete"`
	}
	if err := json.Unmarshal(resolved.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Complete || result.Total != len(result.Items) || len(result.Items) == 0 || result.Items[0].Name != "folder" {
		t.Fatalf("ready archive metadata incorrect: %+v", result)
	}
}

func TestArchiveRealErrorsAreNotPending(t *testing.T) {
	encrypted, err := os.ReadFile("../mountfs/testdata/encrypted/headers.7z")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name       string
		body       []byte
		pageStatus int
	}{{"format", []byte("synthetic invalid archive"), http.StatusBadRequest}, {"password", encrypted, http.StatusForbidden}} {
		t.Run(test.name, func(t *testing.T) {
			download := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.ServeContent(w, r, "pending.7z", time.Unix(100, 0), bytes.NewReader(test.body))
			}))
			defer download.Close()
			cache, err := storage.NewCache(t.TempDir(), 16<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer cache.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			api := &pendingArchiveAPI{&archiveFixtureAPI{url: download.URL, size: int64(len(test.body))}}
			service, err := mountfs.NewService(ctx, api, cache, 0, true, mountfs.Options{})
			if err != nil {
				t.Fatal(err)
			}
			server := newTestServer(t, func(opts *Options) { opts.Service = service })
			page := authenticatedGET(t, server.Handler(), "/archive/1", nil)
			if page.Code != test.pageStatus {
				t.Fatalf("real error initial page status=%d want=%d", page.Code, test.pageStatus)
			}
			entries := authenticatedGET(t, server.Handler(), "/archive/1/entries", nil)
			if entries.Code != http.StatusBadRequest || entries.Header().Get("Retry-After") != "" || strings.Contains(entries.Body.String(), "archive_index_pending") {
				t.Fatalf("real error became retryable pending: status=%d", entries.Code)
			}
		})
	}
}
