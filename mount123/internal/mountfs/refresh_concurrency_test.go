package mountfs

import (
	"context"
	"fmt"
	"sync"
	"syscall"
	"testing"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	goFS "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// Browsing while a refresh lands must never produce an error: readers keep
// seeing a complete snapshot, and the refreshed listing is visible afterwards.
func TestConcurrentBrowsingAndRefreshKeepSnapshotsConsistent(t *testing.T) {
	root, api := refreshTestRoot(t)
	goFS.NewNodeFS(root, &goFS.Options{})
	inode, errno := root.Lookup(context.Background(), testRefreshName, &fuse.EntryOut{})
	if errno != 0 {
		t.Fatalf("lookup refresh control: %v", errno)
	}
	control := inode.Operations()

	stop := make(chan struct{})
	failures := make(chan error, 8)
	var browsers sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		browsers.Add(1)
		go func() {
			defer browsers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, errno := root.Lookup(context.Background(), "before.txt", &fuse.EntryOut{}); errno != 0 && errno != syscall.ENOENT {
					failures <- fmt.Errorf("lookup during refresh: %v", errno)
					return
				}
				stream, errno := root.Readdir(context.Background())
				if errno != 0 {
					failures <- fmt.Errorf("readdir during refresh: %v", errno)
					return
				}
				for stream.HasNext() {
					if _, errno := stream.Next(); errno != 0 {
						failures <- fmt.Errorf("readdir entry during refresh: %v", errno)
						return
					}
				}
				stream.Close()
			}
		}()
	}
	for round := 0; round < 3; round++ {
		api.mu.Lock()
		api.files[0] = []panapi.File{{ID: int64(10 + round), Name: fmt.Sprintf("round-%d.txt", round)}}
		api.mu.Unlock()
		handle, _, errno := control.(goFS.NodeOpener).Open(context.Background(), syscall.O_RDONLY)
		if errno != 0 {
			t.Fatalf("refresh round %d: %v", round, errno)
		}
		if got := readRefreshHandle(t, handle); got != "entries=1\n" {
			t.Fatalf("refresh round %d response = %q", round, got)
		}
		_ = handle.(goFS.FileReleaser).Release(context.Background())
	}
	close(stop)
	browsers.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	if _, errno := root.Lookup(context.Background(), "round-2.txt", &fuse.EntryOut{}); errno != 0 {
		t.Fatalf("refreshed listing is not visible: %v", errno)
	}
	if _, errno := root.Lookup(context.Background(), "before.txt", &fuse.EntryOut{}); errno != syscall.ENOENT {
		t.Fatalf("stale entry survived the refresh: %v", errno)
	}
}
