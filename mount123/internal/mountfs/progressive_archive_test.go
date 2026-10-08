package mountfs

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func TestActualFUSERARProgressiveListing(t *testing.T) {
	if os.Getenv("MOUNT123_FUSE_TEST") != "1" {
		t.Skip("requires FUSE")
	}
	data := rarFixture(128, 256<<10)
	release := make(chan struct{})
	blocked := make(chan struct{})
	var unblockOnce, blockedOnce sync.Once
	unblock := func() { unblockOnce.Do(func() { close(release) }) }
	defer unblock()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var start, end int64
		fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end)
		if start >= 1<<20 {
			blockedOnce.Do(func() { close(blocked) })
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("ETag", `"progressive"`)
		http.ServeContent(w, r, "archive.rar", time.Unix(1, 0), io.NewSectionReader(data, 0, data.size))
	}))
	defer server.Close()
	defer unblock()
	cache, err := storage.NewCache(t.TempDir(), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api := &archiveAPI{url: server.URL, files: []panapi.File{{ID: 77, Name: "archive.rar", Size: data.size, Version: "v1"}}}
	root := New(ctx, api, cache, 0, true)
	point := t.TempDir()
	mounted, err := fs.Mount(point, root, &fs.Options{MountOptions: fuse.MountOptions{Options: []string{"ro"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { unblock(); cancel(); mounted.Unmount(); mounted.Wait() }()
	f, err := os.Open(filepath.Join(point, "archive.rar"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	first := make(chan error, 1)
	go func() {
		names, err := f.Readdirnames(1)
		if err == nil && len(names) != 1 {
			err = fmt.Errorf("first batch=%v", names)
		}
		first <- err
	}()
	select {
	case err := <-first:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first directory entry waited for full RAR scan")
	}
	select {
	case <-blocked:
	case <-time.After(3 * time.Second):
		t.Fatal("scan did not reach blocked suffix")
	}
	// Entries already discovered must be directly accessible while the suffix waits.
	if _, err := os.Stat(filepath.Join(point, "archive.rar", "0000.jpg")); err != nil {
		t.Fatal(err)
	}
	// Ordinary sorted ls must finish while the suffix remains blocked.
	lsCtx, stop := context.WithTimeout(context.Background(), time.Second)
	output, lsErr := exec.CommandContext(lsCtx, "ls", "-1", filepath.Join(point, "archive.rar")).Output()
	stop()
	if lsErr != nil {
		t.Fatalf("ordinary ls waited for the unfinished scan: %v", lsErr)
	}
	if len(output) == 0 {
		t.Fatal("ls omitted already discovered entries")
	}
	rest, err := f.Readdirnames(-1)
	if err != nil {
		t.Fatal(err)
	}
	initialCount := 1 + len(rest)
	if initialCount >= 128 {
		t.Fatal("test suffix was not blocked")
	}
	// Prompt probes must not wait for the archive to finish either.
	probeCtx, stopProbe := context.WithTimeout(context.Background(), time.Second)
	_ = exec.CommandContext(probeCtx, "git", "-C", filepath.Join(point, "archive.rar"), "rev-parse", "--is-inside-work-tree").Run()
	if probeCtx.Err() != nil {
		stopProbe()
		t.Fatal("git prompt probe blocked")
	}
	stopProbe()
	unblock()
	archive := lookup(t, root, "archive.rar")
	if _, err := archive.list(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A handle has a stable snapshot even if its index grew in the meantime.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	same, err := f.Readdirnames(-1)
	if err != nil || len(same) != initialCount {
		t.Fatalf("snapshot changed: count=%d err=%v", len(same), err)
	}
	// Opening a new listing observes the finished index.
	all, err := os.ReadDir(filepath.Join(point, "archive.rar"))
	if err != nil || len(all) != 128 {
		t.Fatalf("refreshed listing: count=%d err=%v", len(all), err)
	}
}
