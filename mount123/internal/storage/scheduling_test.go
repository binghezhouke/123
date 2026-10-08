package storage

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/workqueue"
)

func TestForegroundDownloadWhileBackgroundTransfersBlocked(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	started := make(chan struct{}, 7)
	data := bytes.Repeat([]byte("x"), 1<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/foreground" && r.Header.Get("Range") != "bytes=0-0" {
			started <- struct{}{}
			<-r.Context().Done()
			return
		}
		w.Header().Set("ETag", "\"stable\"")
		http.ServeContent(w, r, "data", time.Unix(1, 0), bytes.NewReader(data))
	}))
	cache, err := NewCache(t.TempDir(), 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); workers.Wait(); srv.Close(); cache.Close() })
	for i := 0; i < 7; i++ {
		url := fmt.Sprintf("%s/%d", srv.URL, i)
		remote, err := NewRemote(ctx, cache, url, int64(len(data)), func(context.Context) (string, error) { return url, nil })
		if err != nil {
			t.Fatal(err)
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			_, _ = remote.ReadAtContext(workqueue.Background(ctx), make([]byte, 1), 0)
		}()
	}
	for i := 0; i < 7; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("background transfer did not start")
		}
	}
	foreground, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	remote, err := NewRemoteContext(ctx, foreground, cache, "foreground", int64(len(data)), func(context.Context) (string, error) { return srv.URL + "/foreground", nil })
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1)
	if _, err := remote.ReadAtContext(foreground, buf, 0); err != nil || buf[0] != 'x' {
		t.Fatalf("foreground download: %q, %v", buf, err)
	}
}
