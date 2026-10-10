package storage

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDownloadLinkSixDaysSurvivesRemoteRecreation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"fixed"`)
		w.Header().Set("Content-Range", "bytes 0-0/1")
		w.WriteHeader(206)
		w.Write([]byte("x"))
	}))
	defer server.Close()
	c, err := NewCache(t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var calls atomic.Int32
	resolve := func(context.Context) (string, error) { calls.Add(1); return server.URL, nil }
	first, err := NewRemote(context.Background(), c, "file-version", 1, resolve)
	if err != nil {
		t.Fatal(err)
	}
	left := time.Until(first.urlUntil)
	if left > 6*24*time.Hour || left < 6*24*time.Hour-time.Minute {
		t.Fatalf("deadline = %v", left)
	}
	second, err := NewRemote(context.Background(), c, "file-version", 1, resolve)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || !first.urlUntil.Equal(second.urlUntil) {
		t.Fatal("recreated source renewed link/deadline")
	}
	c.mu.Lock()
	v := c.links["file-version"]
	v.until = time.Now().Add(-time.Second)
	c.links["file-version"] = v
	c.mu.Unlock()
	if _, err := NewRemote(context.Background(), c, "file-version", 1, resolve); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatal("expired link was not refreshed")
	}
}

func TestDownloadLinkConcurrentReuseAndRejectedURL(t *testing.T) {
	c, err := NewCache(t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var calls atomic.Int32
	resolve := func(context.Context) (string, error) {
		return fmt.Sprintf("https://example.test/%d", calls.Add(1)), nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.downloadLink(context.Background(), "k", "", resolve); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("got %d API calls", calls.Load())
	}
	fresh, err := c.downloadLink(context.Background(), "k", "https://example.test/1", resolve)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatal("rejected link not refreshed")
	}
	again, err := c.downloadLink(context.Background(), "k", "https://example.test/1", resolve)
	if err != nil || again.url != fresh.url || calls.Load() != 2 {
		t.Fatal("stale rejection discarded newer link")
	}
}

func TestRedirectEndpointReusedAndExpiredEndpointRefreshesResolver(t *testing.T) {
	var redirects, resolves atomic.Int32
	var expired atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/link1" || r.URL.Path == "/link2" {
			redirects.Add(1)
			http.Redirect(w, r, "/cdn"+r.URL.Path[len("/link"):], http.StatusFound)
			return
		}
		if r.URL.Path == "/cdn1" && expired.Load() {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("ETag", `"fixed"`)
		w.Header().Set("Content-Range", "bytes 0-0/1")
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte("x"))
	}))
	defer server.Close()
	c, err := NewCache(t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()
	r, err := NewRemote(ctx, c, "redirect", 1, func(context.Context) (string, error) {
		return fmt.Sprintf("%s/link%d", server.URL, resolves.Add(1)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		resp, err := r.requestRange(ctx, 0, 0, true)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 206 {
			t.Fatalf("status %d", resp.StatusCode)
		}
	}
	if redirects.Load() != 1 {
		t.Fatal("redirect repeated per range")
	}
	expired.Store(true)
	resp, err := r.requestRange(ctx, 0, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 206 || resolves.Load() != 2 {
		t.Fatal("expired CDN endpoint did not refresh API link")
	}
}

func TestDownloadLinkPersistsAcrossCacheRestart(t *testing.T) {
	dir := t.TempDir()
	c, err := NewCache(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	resolve := func(context.Context) (string, error) {
		calls.Add(1)
		return "https://example.invalid/file?sig=one", nil
	}
	got, err := c.downloadLink(context.Background(), "file:42", "", resolve)
	if err != nil || got.url == "" {
		t.Fatalf("first resolve: %#v %v", got, err)
	}
	c.Close()
	time.Sleep(50 * time.Millisecond)
	c2, err := NewCache(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	got, err = c2.downloadLink(context.Background(), "file:42", "", func(context.Context) (string, error) { calls.Add(1); return "wrong", nil })
	if err != nil || got.url != "https://example.invalid/file?sig=one" {
		t.Fatalf("restored link: %#v %v", got, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("resolver calls=%d", calls.Load())
	}
}
