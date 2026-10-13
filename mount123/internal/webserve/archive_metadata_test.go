package webserve

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/mountfs"
	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
)

type cachedArchiveHTMLAPI struct {
	*archiveFixtureAPI
	details  atomic.Int32
	resolves atomic.Int32
}

func (a *cachedArchiveHTMLAPI) Detail(context.Context, int64) (panapi.File, error) {
	a.details.Add(1)
	return panapi.File{}, fmt.Errorf("synthetic Detail unavailable")
}

func (a *cachedArchiveHTMLAPI) DownloadURL(ctx context.Context, id int64) (string, error) {
	a.resolves.Add(1)
	return a.archiveFixtureAPI.DownloadURL(ctx, id)
}

func TestArchiveHTMLRestoresTitleWithoutDetail(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	payload := archiveBytes(t)
	var downloads atomic.Int32
	download := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads.Add(1)
		http.ServeContent(w, r, "photos.zip", time.Time{}, bytes.NewReader(payload))
	}))
	defer download.Close()
	api := &cachedArchiveHTMLAPI{archiveFixtureAPI: &archiveFixtureAPI{url: download.URL, size: int64(len(payload))}}
	dir := t.TempDir()
	cache, err := storage.NewCache(dir, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	opts := mountfs.Options{DirectoryTTL: time.Hour}
	seed, err := mountfs.NewService(ctx, api, cache, 0, false, opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = seed.ListDirectory(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if _, err = seed.ListArchivePage(ctx, 1, "", "", archiveInitialPageSize); err != nil {
		t.Fatal(err)
	}
	if err = cache.Close(); err != nil {
		t.Fatal(err)
	}
	cache, err = storage.NewCache(dir, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	service, err := mountfs.NewService(ctx, api, cache, 0, false, opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.ListDirectory(ctx, 0); err != nil {
		t.Fatal(err)
	}
	server := newTestServer(t, func(opts *Options) { opts.Service = service })
	api.details.Store(0)
	api.resolves.Store(0)
	downloads.Store(0)
	response := authenticatedGET(t, server.Handler(), "/archive/1", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("archive HTTP status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "<h1>photos.zip</h1>") {
		t.Error("rendered archive title is missing")
	}
	if api.details.Load() != 0 || api.resolves.Load() != 0 || downloads.Load() != 0 {
		t.Errorf("warm archive HTML requested Detail=%d DownloadURL=%d HTTP=%d", api.details.Load(), api.resolves.Load(), downloads.Load())
	}
}
