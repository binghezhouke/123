package storage

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/workqueue"
)

// RAR prefetch decoders read headers and packed data in small pieces. Returning
// the first piece must not cancel the containing response and redownload it for
// the next piece, while a foreground Open waits for that prefetch's member fill.
func TestBackgroundSmallReadsKeepProgressiveBlock(t *testing.T) {
	const size = 1 << 20
	data := bytes.Repeat([]byte{'r'}, size)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	cancelled := make(chan struct{}, 2)
	var requests atomic.Int32
	cache, remote := windowTestRemote(t, size, func(w http.ResponseWriter, r *http.Request) {
		a, b := rangeWindowHeaders(t, w, r, size)
		w.Header().Set("Content-Length", fmt.Sprint(b-a+1))
		w.WriteHeader(206)
		if a == b {
			_, _ = w.Write(data[a : b+1])
			return
		}
		requests.Add(1)
		end := min(b+1, a+512)
		_, _ = w.Write(data[a:end])
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			cancelled <- struct{}{}
			return
		case <-release:
			_, _ = w.Write(data[end : b+1])
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	bg := workqueue.Background(ctx)
	got := make([]byte, 512)
	if n, err := remote.ReadAtContext(bg, got, 0); err != nil || n != len(got) {
		t.Fatalf("first piece=%d/%v", n, err)
	}
	select {
	case <-cancelled:
		t.Fatal("background decoder's small read cancelled its unfinished HTTP block")
	case <-time.After(50 * time.Millisecond):
	}
	unblock()
	for {
		d := cache.DownloadStats()
		if d.ActiveRequests == 0 && d.StagingActiveBytes == 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("block did not complete")
		case <-time.After(time.Millisecond):
		}
	}
	if n, err := remote.ReadAtContext(bg, got, 512); err != nil || n != len(got) || !bytes.Equal(got, data[512:1024]) {
		t.Fatalf("second piece=%d/%v", n, err)
	}
	if requests.Load() != 1 {
		t.Fatalf("two small decoder reads required %d HTTP blocks", requests.Load())
	}
}

func TestCacheCloseCancelsRetainedBackgroundBlock(t *testing.T) {
	const size = 1 << 20
	cancelled := make(chan struct{}, 1)
	cache, remote := windowTestRemote(t, size, func(w http.ResponseWriter, r *http.Request) {
		a, b := rangeWindowHeaders(t, w, r, size)
		w.Header().Set("Content-Length", fmt.Sprint(b-a+1))
		w.WriteHeader(206)
		if a == b {
			_, _ = w.Write([]byte{'r'})
			return
		}
		_, _ = w.Write(bytes.Repeat([]byte{'r'}, 512))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		cancelled <- struct{}{}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if n, err := remote.ReadAtContext(workqueue.Background(ctx), make([]byte, 512), 0); err != nil || n != 512 {
		t.Fatalf("prefix=%d/%v", n, err)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-ctx.Done():
		t.Fatal("mount close left retained background HTTP running")
	}
	d := cache.DownloadStats()
	if d.ActiveRequests != 0 || d.StagingActiveBytes != 0 {
		t.Fatalf("mount close leaked budgets: %+v", d)
	}
}
