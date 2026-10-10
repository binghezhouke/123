package storage

import (
	"context"
	"errors"
	"io"
	"os"
	"syscall"
	"testing"

	"github.com/binghezhouke/123/mount123/internal/workqueue"
)

func TestCacheDiskReserveShedsBackgroundButAllowsSmallForeground(t *testing.T) {
	c, err := NewCacheWithIndexBudgetAndMinFree(t.TempDir(), 64, 8, 100, DefaultDownloadConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.statfs = func(string) (int64, error) { return 10, nil }
	fill := func(context.Context, io.Writer) error { return nil }
	if _, err := c.Acquire(workqueue.Background(context.Background()), "prefetch", 4, fill); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("background admission error = %v, want ENOSPC", err)
	}
	if _, err := c.Acquire(context.Background(), "foreground", 4, func(_ context.Context, w io.Writer) error {
		_, err := io.WriteString(w, "data")
		return err
	}); err != nil {
		t.Fatalf("small foreground admission failed: %v", err)
	}
	stats := c.Stats()
	if stats.DiskFreeBytes != 10 || stats.DiskMinimumFreeBytes != 100 || stats.DiskPressureRejects != 1 {
		t.Fatalf("disk stats = %+v", stats)
	}
}

func TestCacheStartupCleansOnlyRegularOrphanFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{".fill-crashed", ".metadata-crashed"} {
		if err := os.WriteFile(dir+"/"+name, []byte("stale"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(dir+"/.fill-directory", 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", dir+"/.metadata-link"); err != nil {
		t.Fatal(err)
	}
	c, err := NewCache(dir, 64)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := c.Stats().OrphanFilesCleaned; got != 2 {
		t.Fatalf("cleaned orphan count = %d, want 2", got)
	}
	if _, err := os.Stat(dir + "/.fill-directory"); err != nil {
		t.Fatalf("non-regular orphan was changed: %v", err)
	}
	if _, err := os.Lstat(dir + "/.metadata-link"); err != nil {
		t.Fatalf("symlink orphan was changed: %v", err)
	}
}
