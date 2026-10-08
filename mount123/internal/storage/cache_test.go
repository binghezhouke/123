package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
)

func fillBytes(b []byte) func(context.Context, io.Writer) error {
	return func(_ context.Context, w io.Writer) error { _, err := w.Write(b); return err }
}

func TestCachePinnedEvictionAndPersistence(t *testing.T) {
	dir := t.TempDir()
	c, err := NewCache(dir, 6)
	if err != nil {
		t.Fatal(err)
	}
	h1, err := c.Acquire(context.Background(), "one", 3, fillBytes([]byte("one")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Acquire(context.Background(), "two", 4, fillBytes([]byte("four"))); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("want ENOSPC while pinned, got %v", err)
	}
	if err = h1.Close(); err != nil {
		t.Fatal(err)
	}
	h2, err := c.Acquire(context.Background(), "two", 4, fillBytes([]byte("four")))
	if err != nil {
		t.Fatal(err)
	}
	if err = h2.Close(); err != nil {
		t.Fatal(err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err = NewCache(dir, 6)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	h, err := c.Acquire(context.Background(), "two", 4, func(context.Context, io.Writer) error { t.Fatal("persistent entry unexpectedly refilled"); return nil })
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if n, err := h.ReadAt(got, 0); err != nil || n != 4 || !bytes.Equal(got, []byte("four")) {
		t.Fatalf("ReadAt = %q, %d, %v", got, n, err)
	}
	_ = h.Close()
}

func TestStableDigestPersistsAndKeyIsPrivate(t *testing.T) {
	dir := t.TempDir()
	c, err := NewCache(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	first := c.StableDigest("password", "account\x00secret")
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, ".identity-hmac-key")
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("identity key mode = %o, want 600", info.Mode().Perm())
	}
	c, err = NewCache(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := c.StableDigest("password", "account\x00secret"); got != first {
		t.Fatal("stable digest changed after cache reopen")
	}
	if got := c.StableDigest("password", "other-account\x00secret"); got == first {
		t.Fatal("different account shared a stable digest")
	}
}

func TestCacheFailedFillReleasesReservation(t *testing.T) {
	c, err := NewCache(t.TempDir(), 4)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err = c.Acquire(context.Background(), "bad", 4, func(_ context.Context, w io.Writer) error {
		_, _ = io.WriteString(w, "xx")
		return errors.New("failed")
	}); err == nil {
		t.Fatal("expected failed fill")
	}
	h, err := c.Acquire(context.Background(), "good", 4, fillBytes([]byte("good")))
	if err != nil {
		t.Fatal(err)
	}
	_ = h.Close()
}

func TestCacheDeduplicatesConcurrentFill(t *testing.T) {
	c, err := NewCache(t.TempDir(), 16)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var fills atomic.Int32
	fill := func(_ context.Context, w io.Writer) error {
		fills.Add(1)
		_, err := io.WriteString(w, "shared")
		return err
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var first error
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, e := c.Acquire(context.Background(), "shared", 6, fill)
			if e == nil {
				e = h.Close()
			}
			if e != nil {
				mu.Lock()
				if first == nil {
					first = e
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if first != nil {
		t.Fatal(first)
	}
	if n := fills.Load(); n != 1 {
		t.Fatalf("fill called %d times", n)
	}
}

func TestCacheRejectsConcurrentDirectoryOwnerAndOversize(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	c, err := NewCache(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = NewCache(dir, 2); err == nil {
		t.Fatal("expected exclusive lock")
	}
	if _, err = c.Acquire(context.Background(), "x", 3, fillBytes([]byte("xxx"))); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("got %v", err)
	}
	_ = c.Close()
	if _, err = os.Stat(filepath.Join(dir, ".lock")); err != nil {
		t.Fatal(err)
	}
}
