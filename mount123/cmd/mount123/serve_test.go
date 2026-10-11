//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/mountfs"
	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
)

// serveTestAPI stands in for the cloud so the serve command can be exercised
// without network access.
type serveTestAPI struct{}

func (serveTestAPI) List(context.Context, int64) ([]panapi.File, error) {
	return []panapi.File{
		{ID: 11, ParentID: 0, Name: "docs", IsDir: true},
		{ID: 12, ParentID: 0, Name: "readme.txt", Size: 5},
	}, nil
}

func (serveTestAPI) Infos(context.Context, []int64) ([]panapi.File, error) {
	return nil, nil
}

func (serveTestAPI) Detail(context.Context, int64) (panapi.File, error) {
	return panapi.File{}, fmt.Errorf("no metadata in this test")
}

func (serveTestAPI) DownloadURL(context.Context, int64) (string, error) {
	return "", fmt.Errorf("no downloads in this test")
}

func serveTestDeps(listenAddr chan<- string) serveDeps {
	return serveDeps{
		newAPI: func(panapi.Config) (mountfs.API, error) { return serveTestAPI{}, nil },
		newCache: func(dir string, maxBytes, indexBudget, minFreeBytes int64, durability string, download storage.DownloadConfig) (*storage.Cache, error) {
			return openCacheWithIndexBudgetAndMinFree(dir, maxBytes, indexBudget, minFreeBytes, durability, download)
		},
		onListen: func(addr string) {
			if listenAddr != nil {
				listenAddr <- addr
			}
		},
	}
}

func writeServeConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"CLIENT_ID":"test-id","CLIENT_SECRET":"test-secret","SECRET_KEY":"test-secret-key"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRunServeStartsAuthenticatesAndShutsDown covers the acceptance criteria
// for the skeleton: the command binds an address, refuses anonymous access,
// serves the index for a logged-in session, and exits cleanly on cancellation.
func TestRunServeStartsAuthenticatesAndShutsDown(t *testing.T) {
	cacheDir := t.TempDir()
	configPath := writeServeConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	listenAddr := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- runServeWith(ctx, []string{
			"-config", configPath,
			"-addr", "127.0.0.1:0",
			"-cache-dir", cacheDir,
			"-password", "test-password",
		}, io.Discard, io.Discard, serveTestDeps(listenAddr))
	}()

	var addr string
	select {
	case addr = <-listenAddr:
	case err := <-done:
		t.Fatalf("serve exited before listening: %v", err)
	case <-time.After(60 * time.Second):
		t.Fatal("serve never bound an address")
	}
	baseURL := "http://" + addr
	client := &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()

	health := waitForStatus(t, client, baseURL+"/healthz", http.StatusOK)
	defer health.Body.Close()
	payload, err := io.ReadAll(health.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), `"status":"ok"`) {
		t.Fatalf("healthz payload = %s", payload)
	}

	anonymous := getWithHeader(t, client, baseURL+"/", "text/html")
	defer anonymous.Body.Close()
	if anonymous.StatusCode != http.StatusSeeOther {
		t.Fatalf("anonymous index status = %d, want 303", anonymous.StatusCode)
	}
	if location := anonymous.Header.Get("Location"); !strings.HasPrefix(location, "/login") {
		t.Fatalf("anonymous redirect = %q, want the login page", location)
	}

	form := strings.NewReader("password=test-password")
	login, err := client.Post(baseURL+"/login", "application/x-www-form-urlencoded", form)
	if err != nil {
		t.Fatal(err)
	}
	defer login.Body.Close()
	if login.StatusCode != http.StatusSeeOther {
		t.Fatalf("login status = %d, want 303", login.StatusCode)
	}
	cookies := login.Cookies()
	if len(cookies) == 0 {
		t.Fatal("login did not return a session cookie")
	}

	request, err := http.NewRequest(http.MethodGet, baseURL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	index, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer index.Body.Close()
	if index.StatusCode != http.StatusOK {
		t.Fatalf("index status = %d, want 200", index.StatusCode)
	}
	page, err := io.ReadAll(index.Body)
	if err != nil {
		t.Fatal(err)
	}
	resolvedCache, err := filepath.EvalSymlinks(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(page), resolvedCache) {
		t.Fatalf("index page does not report the shared cache dir %q: %s", resolvedCache, page)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve did not shut down cleanly: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("serve did not shut down after cancellation")
	}

	// The cache directory must stay usable for the FUSE mount afterwards.
	cache, err := openCache(cacheDir, 64<<20, "durable")
	if err != nil {
		t.Fatalf("cache directory is not reusable: %v", err)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
}

func waitForStatus(t *testing.T, client *http.Client, url string, want int) *http.Response {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		response, err := client.Get(url)
		if err == nil {
			if response.StatusCode == want {
				return response
			}
			lastErr = fmt.Errorf("status %d", response.StatusCode)
			response.Body.Close()
		} else {
			lastErr = err
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("GET %s never returned %d: %v", url, want, lastErr)
	return nil
}

func getWithHeader(t *testing.T, client *http.Client, url, accept string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Accept", accept)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func TestValidateServeSettings(t *testing.T) {
	if err := validateServeSettings(serveSettings{}); err == nil {
		t.Fatal("empty settings were accepted")
	}
	valid := func() serveSettings {
		return serveSettings{
			addr:               "127.0.0.1:0",
			cacheDir:           "/tmp/mount123",
			cacheGiB:           50,
			cacheDurability:    "durable",
			archiveEntries:     1000,
			metadataMiB:        64,
			directoryTTL:       time.Hour,
			sourceTTL:          time.Hour,
			downloadRequests:   32,
			downloadBytesMiB:   128,
			downloadReserveMiB: 16,
		}
	}
	if err := validateServeSettings(valid()); err != nil {
		t.Fatalf("valid settings were rejected: %v", err)
	}
	for name, mutate := range map[string]func(*serveSettings){
		"empty address":        func(s *serveSettings) { s.addr = "" },
		"bad durability":       func(s *serveSettings) { s.cacheDurability = "maybe" },
		"negative session ttl": func(s *serveSettings) { s.sessionTTL = -time.Second },
		"zero archive entries": func(s *serveSettings) { s.archiveEntries = 0 },
		"reserve over budget":  func(s *serveSettings) { s.downloadReserveMiB = s.downloadBytesMiB },
		"zero cache":           func(s *serveSettings) { s.cacheGiB = 0 },
	} {
		settings := valid()
		mutate(&settings)
		if err := validateServeSettings(settings); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

// fakeCloud answers the read-only 123 endpoints the serve command needs, so the
// integration test exercises the real panapi client, storage cache and mountfs
// tree without reaching the network.
type fakeCloud struct {
	server    *httptest.Server
	listCalls atomic.Int64
	rootID    int64
}

func newFakeCloud(t *testing.T, rootID int64) *fakeCloud {
	t.Helper()
	cloud := &fakeCloud{rootID: rootID}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/access_token", func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, map[string]any{"accessToken": "fake-token", "expiresIn": 3600})
	})
	mux.HandleFunc("/api/v1/file/detail", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("fileID") != fmt.Sprint(rootID) {
			http.Error(w, "unknown file", http.StatusNotFound)
			return
		}
		writeEnvelope(w, map[string]any{
			"fileId": rootID, "parentFileId": 0, "filename": "root", "size": 0, "type": 1, "etag": "root-v1",
		})
	})
	mux.HandleFunc("/api/v2/file/list", func(w http.ResponseWriter, r *http.Request) {
		cloud.listCalls.Add(1)
		parent := r.URL.Query().Get("parentFileId")
		writeEnvelope(w, map[string]any{
			"fileList": []any{
				map[string]any{"fileId": 101, "parentFileId": parent, "filename": "docs", "size": 0, "type": 1, "etag": "docs-v1"},
				map[string]any{"fileId": 102, "parentFileId": parent, "filename": "readme.txt", "size": 5, "type": 0, "etag": "readme-v1"},
			},
			"lastFileId": nil,
		})
	})
	cloud.server = httptest.NewServer(mux)
	t.Cleanup(cloud.server.Close)
	return cloud
}

func writeEnvelope(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "message": "ok", "data": data})
}

// TestRunServeBuildsTheSharedStack drives the command against a local fake API
// so the real panapi client, disk cache and mountfs tree are exercised together
// with no FUSE mount and no network access.
func TestRunServeBuildsTheSharedStack(t *testing.T) {
	cacheDir := t.TempDir()
	configPath := writeServeConfig(t)
	cloud := newFakeCloud(t, 42)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	listenAddr := make(chan string, 1)
	deps := serveDeps{
		newAPI: func(cfg panapi.Config) (mountfs.API, error) {
			cfg.BaseURL = cloud.server.URL
			return panapi.New(cfg)
		},
		newCache: func(dir string, maxBytes, indexBudget, minFreeBytes int64, durability string, download storage.DownloadConfig) (*storage.Cache, error) {
			return openCacheWithIndexBudgetAndMinFree(dir, maxBytes, indexBudget, minFreeBytes, durability, download)
		},
		onListen: func(addr string) { listenAddr <- addr },
	}
	done := make(chan error, 1)
	go func() {
		done <- runServeWith(ctx, []string{
			"-config", configPath,
			"-addr", "127.0.0.1:0",
			"-cache-dir", cacheDir,
			"-root-id", "42",
			"-password", "test-password",
		}, io.Discard, io.Discard, deps)
	}()

	var addr string
	select {
	case addr = <-listenAddr:
	case err := <-done:
		t.Fatalf("serve exited before listening: %v", err)
	case <-time.After(60 * time.Second):
		t.Fatal("serve never bound an address")
	}
	if calls := cloud.listCalls.Load(); calls == 0 {
		t.Fatal("the serve tree never listed the cloud root")
	}
	client := &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()

	login, err := client.Post("http://"+addr+"/login", "application/x-www-form-urlencoded", strings.NewReader("password=test-password"))
	if err != nil {
		t.Fatal(err)
	}
	defer login.Body.Close()
	if login.StatusCode != http.StatusSeeOther {
		t.Fatalf("login status = %d, want 303", login.StatusCode)
	}
	request, err := http.NewRequest(http.MethodGet, "http://"+addr+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, cookie := range login.Cookies() {
		request.AddCookie(cookie)
	}
	index, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer index.Body.Close()
	page, err := io.ReadAll(index.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(page), "42") {
		t.Fatalf("index page does not report the root ID: %s", page)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "token.json")); err != nil {
		t.Fatalf("the served process did not use the shared cache directory: %v", err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve did not shut down cleanly: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("serve did not shut down after cancellation")
	}
}

func TestParseServeSettingsHelp(t *testing.T) {
	settings, err := parseServeSettings([]string{"-h"}, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("help returned an error: %v", err)
	}
	if settings != nil {
		t.Fatal("help returned settings instead of nil")
	}
}

func TestParseServeSettingsRejectsPositionalArguments(t *testing.T) {
	if _, err := parseServeSettings([]string{"extra"}, io.Discard, io.Discard); err == nil {
		t.Fatal("positional arguments were accepted")
	}
}

func TestResolveServePasswordPrecedence(t *testing.T) {
	config := serveConfig{ServePassword: "from-config", WebPassword: "legacy-config"}
	t.Setenv("MOUNT123_SERVE_PASSWORD", "from-env")
	if got := resolveServePassword(serveSettings{password: "from-flag"}, config); got != "from-flag" {
		t.Fatalf("flag password lost: %q", got)
	}
	if got := resolveServePassword(serveSettings{}, config); got != "from-env" {
		t.Fatalf("environment password lost: %q", got)
	}
	t.Setenv("MOUNT123_SERVE_PASSWORD", "")
	if got := resolveServePassword(serveSettings{}, config); got != "from-config" {
		t.Fatalf("config password lost: %q", got)
	}
	if got := resolveServePassword(serveSettings{}, serveConfig{WebPassword: "legacy-config"}); got != "legacy-config" {
		t.Fatalf("legacy config password lost: %q", got)
	}
	if got := resolveServePassword(serveSettings{}, serveConfig{}); got != "" {
		t.Fatalf("unset password = %q, want empty so it is generated", got)
	}
}

func TestReadServeConfigAllowsTokenOnly(t *testing.T) {
	t.Setenv("PAN123_ACCESS_TOKEN", "token-from-environment")
	config, err := readServeConfig(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("token-only configuration was rejected: %v", err)
	}
	if config.AccessToken != "token-from-environment" {
		t.Fatalf("access token = %q", config.AccessToken)
	}
	t.Setenv("PAN123_ACCESS_TOKEN", "")
	if _, err = readServeConfig(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("a missing config without a token was accepted")
	}
}

func TestPrepareServeCacheDirIsPrivate(t *testing.T) {
	target := filepath.Join(t.TempDir(), "nested", "cache")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	abs, err := prepareServeCacheDir(target)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(abs) {
		t.Fatalf("cache directory %q is not absolute", abs)
	}
	info, err := os.Stat(abs)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("cache directory mode = %v, want 0700", info.Mode().Perm())
	}
}

func TestServeSessionKey(t *testing.T) {
	first := serveSessionKey("shared-secret")
	second := serveSessionKey("shared-secret")
	if len(first) < 32 || string(first) != string(second) {
		t.Fatal("a configured secret did not produce a stable session key")
	}
	if a, b := serveSessionKey(""), serveSessionKey(""); string(a) == string(b) {
		t.Fatal("an unset secret reused the same random session key")
	}
}
