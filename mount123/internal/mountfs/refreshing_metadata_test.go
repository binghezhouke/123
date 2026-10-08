package mountfs

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
)

type delayedListingAPI struct {
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
}

func (a *delayedListingAPI) List(ctx context.Context, id int64) ([]panapi.File, error) {
	if a.calls.Add(1) > 1 {
		select {
		case a.started <- struct{}{}:
		default:
		}
		select {
		case <-a.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return []panapi.File{{ID: 2, Name: "new.txt"}}, nil
	}
	return []panapi.File{{ID: 1, Name: "old.txt"}}, nil
}
func (a *delayedListingAPI) DownloadURL(context.Context, int64) (string, error) {
	return "", fmt.Errorf("unused")
}
func TestDirectoryRefreshDoesNotBlockCachedListing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api := &delayedListingAPI{started: make(chan struct{}, 1), release: make(chan struct{})}
	root := New(ctx, api, nil, 0, true)
	stream, errno := root.Readdir(ctx)
	if errno != 0 {
		t.Fatal(errno)
	}
	stream.Close()
	root.tree.mu.Lock()
	root.tree.meta["dir:0"].expires = time.Now().Add(-time.Second)
	root.tree.mu.Unlock()
	completed := make(chan error, 1)
	go func() {
		var wg sync.WaitGroup
		errors := make(chan error, 20)
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				s, e := root.Readdir(ctx)
				if e != 0 {
					errors <- e
					return
				}
				defer s.Close()
				v, e := s.Next()
				if e != 0 || v.Name != "old.txt" {
					errors <- fmt.Errorf("stale listing mismatch")
				}
			}()
		}
		wg.Wait()
		close(errors)
		for err := range errors {
			completed <- err
			return
		}
		completed <- nil
	}()
	select {
	case err := <-completed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cached directory waited for API refresh")
	}
	select {
	case <-api.started:
	case <-time.After(time.Second):
		t.Fatal("refresh did not start")
	}
	if api.calls.Load() != 2 {
		t.Fatalf("API requests=%d", api.calls.Load())
	}
	close(api.release)
	deadline := time.Now().Add(time.Second)
	for {
		root.tree.mu.Lock()
		pending := len(root.tree.refreshing) > 0
		root.tree.mu.Unlock()
		if !pending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("refresh did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	s, e := root.Readdir(ctx)
	if e != 0 {
		t.Fatal(e)
	}
	defer s.Close()
	v, e := s.Next()
	if e != 0 || v.Name != "new.txt" {
		t.Fatalf("refreshed entry=%v errno=%v", v, e)
	}
}
