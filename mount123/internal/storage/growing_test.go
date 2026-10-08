package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestAcquireGrowingSharesReadablePrefixAndPublishesOnSuccess(t *testing.T) {
	c, err := NewCache(t.TempDir(), 32)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	firstWrite := make(chan struct{})
	release := make(chan struct{})
	var fills atomic.Int32
	fill := func(ctx context.Context, w io.Writer) error {
		fills.Add(1)
		if _, err := io.WriteString(w, "abc"); err != nil {
			return err
		}
		close(firstWrite)
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		_, err := io.WriteString(w, "def")
		return err
	}
	lifetime, stop := context.WithCancel(context.Background())
	defer stop()
	h1, err := c.AcquireGrowing(context.Background(), lifetime, "grow", 6, fill)
	if err != nil {
		t.Fatal(err)
	}
	defer h1.Close()
	<-firstWrite
	h2, err := c.AcquireGrowing(context.Background(), lifetime, "grow", 6, fill)
	if err != nil {
		t.Fatal(err)
	}
	defer h2.Close()
	got := make([]byte, 3)
	if n, err := h2.ReadAt(context.Background(), got, 0); err != nil || n != 3 || !bytes.Equal(got, []byte("abc")) {
		t.Fatalf("prefix ReadAt = %q, %d, %v", got, n, err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := h1.ReadAt(waitCtx, make([]byte, 3), 3); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unwritten range ReadAt = %v, want deadline", err)
	}
	close(release)
	got = make([]byte, 6)
	if n, err := h1.ReadAt(context.Background(), got, 0); err != nil || n != 6 || !bytes.Equal(got, []byte("abcdef")) {
		t.Fatalf("complete ReadAt = %q, %d, %v", got, n, err)
	}
	if n := fills.Load(); n != 1 {
		t.Fatalf("fill called %d times", n)
	}
	h, err := c.Acquire(context.Background(), "grow", 6, func(context.Context, io.Writer) error {
		t.Fatal("complete growing result was not cached")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = h.Close()
}

func TestAcquireGrowingLastCloseCancelsWithoutPublishing(t *testing.T) {
	c, err := NewCache(t.TempDir(), 32)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	started := make(chan struct{})
	canceled := make(chan struct{})
	h, err := c.AcquireGrowing(context.Background(), context.Background(), "cancel", 6, func(ctx context.Context, w io.Writer) error {
		close(started)
		_, _ = io.WriteString(w, "abc")
		<-ctx.Done()
		close(canceled)
		return ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("last close did not cancel the growing fill")
	}
	if _, err := c.Open("cancel"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed growing output became a cache entry: %v", err)
	}
	regular, err := c.Acquire(context.Background(), "cancel", 6, func(_ context.Context, w io.Writer) error {
		_, err := io.WriteString(w, "newone")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = regular.Close()
}

func TestAcquireGrowingFailureDoesNotPublishPartialBytes(t *testing.T) {
	c, err := NewCache(t.TempDir(), 32)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	writeDone := make(chan struct{})
	h, err := c.AcquireGrowing(context.Background(), context.Background(), "failed", 6, func(_ context.Context, w io.Writer) error {
		_, _ = io.WriteString(w, "abc")
		close(writeDone)
		return syscall.EIO
	})
	if err != nil {
		t.Fatal(err)
	}
	<-writeDone
	if _, err := h.ReadAt(context.Background(), make([]byte, 6), 0); !errors.Is(err, syscall.EIO) {
		t.Fatalf("incomplete range ReadAt = %v, want EIO", err)
	}
	_ = h.Close()
	if _, err := c.Open("failed"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial growing output became a cache entry: %v", err)
	}
}
