package webserve

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStatusSnapshotSharesTreeWithoutCloudRequests(t *testing.T) {
	f := newListingFixture(t)
	f.server.authEnabled = false
	now := time.Now()
	f.server.now = func() time.Time { return now }
	before := f.api.lists.Load()
	page := get(t, f.server.Handler(), "/status", nil)
	if page.Code != 200 || !strings.Contains(page.Body.String(), "chart-traffic") {
		t.Fatalf("status page = %d", page.Code)
	}
	first := get(t, f.server.Handler(), "/api/status", nil)
	if first.Code != 200 || first.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("snapshot response = %d %v", first.Code, first.Header())
	}
	var snapshot statusSnapshot
	if err := json.Unmarshal(first.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.IO.Cache.CapacityBytes != 16<<20 || snapshot.Web.Completed != 0 || snapshot.Web.Active != 0 {
		t.Fatalf("wrong shared cache or diagnostics counted as traffic: %+v", snapshot)
	}
	if bytes.Contains(first.Body.Bytes(), []byte("/tmp/cache")) || bytes.Contains(first.Body.Bytes(), []byte("b-notes.txt")) {
		t.Fatal("snapshot exposes private paths")
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := get(t, f.server.Handler(), "/api/status", nil)
			if !bytes.Equal(r.Body.Bytes(), first.Body.Bytes()) {
				t.Error("viewers did not share sampled snapshot")
			}
		}()
	}
	wg.Wait()
	if f.api.lists.Load() != before {
		t.Fatal("status polling fetched cloud metadata")
	}
	response := get(t, f.server.Handler(), "/file/1/download", map[string]string{"Range": "bytes=0-3"})
	if response.Code != 206 {
		t.Fatalf("file range status = %d", response.Code)
	}
	now = now.Add(6 * time.Second)
	updated := get(t, f.server.Handler(), "/api/status", nil)
	if err := json.Unmarshal(updated.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Web.Completed != 1 || snapshot.Web.ResponseBytes != 4 || snapshot.Web.Success != 1 {
		t.Fatalf("missing real request statistics: %+v", snapshot.Web)
	}
	if snapshot.IO.DownloadedBytes.Bytes == nil || *snapshot.IO.DownloadedBytes.Bytes == 0 {
		t.Fatal("public read not reflected in shared statistics")
	}
}

func TestStatusCountsStreamingBytesBeforeCompletion(t *testing.T) {
	f := newListingFixture(t)
	f.server.authEnabled = false
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	f.server.mux.HandleFunc("GET /stream-test", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello"))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	host := httptest.NewServer(f.server.Handler())
	defer host.Close()
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get(host.URL + "/stream-test")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	buf := make([]byte, 5)
	if _, err = io.ReadFull(response.Body, buf); err != nil {
		t.Fatal(err)
	}
	sample := get(t, f.server.Handler(), "/api/status", nil)
	var snapshot statusSnapshot
	if err := json.Unmarshal(sample.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Web.Active != 1 || snapshot.Web.ResponseBytes != 5 || snapshot.Web.Completed != 0 {
		t.Fatalf("in-flight stream missing: %+v", snapshot.Web)
	}
	once.Do(func() { close(release) })
	_, _ = io.Copy(io.Discard, response.Body)
}

func TestStatusAuthUnavailableAndCancellation(t *testing.T) {
	server := newTestServer(t, nil)
	for _, path := range []string{"/status", "/api/status"} {
		r := get(t, server.Handler(), path, map[string]string{"Accept": "application/json"})
		if r.Code != 401 {
			t.Fatalf("anonymous %s = %d", path, r.Code)
		}
	}
	r := authenticatedGET(t, server.Handler(), "/api/status", map[string]string{"Accept": "application/json"})
	if r.Code != 503 || !json.Valid(r.Body.Bytes()) {
		t.Fatalf("missing service = %d %s", r.Code, r.Body.String())
	}
	server.authEnabled = false
	server.mux.HandleFunc("GET /cancel-test", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "cancelled", 400) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("GET", "/cancel-test", nil).WithContext(ctx)
	server.Handler().ServeHTTP(httptest.NewRecorder(), req)
	stats := server.traffic.snapshot()
	if stats.Cancelled != 1 || stats.ClientError != 1 || stats.Active != 0 {
		t.Fatalf("cancelled request = %+v", stats)
	}
}

func TestStatusBrowserCalculations(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is required for status browser calculation tests")
	}
	out, err := exec.Command(node, "--test", "testdata/status_test.js").CombinedOutput()
	if err != nil {
		t.Fatalf("status browser tests: %v\n%s", err, out)
	}
}

// Opt-in real Chromium boundary test. The fixture uses the actual handlers and
// assets; the browser script injects deterministic counter changes and outages.
func TestStatusBrowserUI(t *testing.T) {
	python := os.Getenv("MOUNT123_BROWSER_PYTHON")
	if python == "" {
		t.Skip("set MOUNT123_BROWSER_PYTHON to a Python with Playwright")
	}
	f := newListingFixture(t)
	f.server.authEnabled = false
	host := httptest.NewServer(f.server.Handler())
	defer host.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, python, "testdata/status_browser_test.py", host.URL).CombinedOutput()
	if err != nil {
		t.Fatalf("status Chromium test: %v\n%s", err, out)
	}
	t.Log(string(out))
}
