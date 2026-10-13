package storage

import (
	"context"
	"errors"
	"io"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestGrowingVerifiedPrefix(t *testing.T) {
	c, err := NewCache(t.TempDir(), 32)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	written, validate, verified, fail := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	h, err := c.AcquireGrowing(ctx, ctx, "verified", 6, func(ctx context.Context, w io.Writer) error {
		if _, err := io.WriteString(w, "abc"); err != nil {
			return err
		}
		v := w.(VerifiedPrefixWriter)
		if err := v.VerifyThrough(4); !errors.Is(err, syscall.EINVAL) {
			return errors.New("validated unwritten bytes")
		}
		close(written)
		select {
		case <-validate:
		case <-ctx.Done():
			return ctx.Err()
		}
		if err := v.VerifyThrough(3); err != nil {
			return err
		}
		if err := v.VerifyThrough(2); !errors.Is(err, syscall.EINVAL) {
			return errors.New("validation watermark moved backward")
		}
		close(verified)
		select {
		case <-fail:
			return syscall.EIO
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	<-written
	waitCtx, stop := context.WithTimeout(ctx, 10*time.Millisecond)
	defer stop()
	if err := h.WaitVerified(waitCtx, 3); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unverified prefix wait = %v", err)
	}
	close(validate)
	<-verified
	if err := h.WaitVerified(ctx, 3); err != nil {
		t.Fatal(err)
	}
	if h.Materialized() {
		t.Fatal("prefix verification waited for the whole fill")
	}
	if err := h.WaitVerified(ctx, 7); !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("out-of-bounds wait = %v", err)
	}
	close(fail)
	if err := h.Wait(ctx); !errors.Is(err, syscall.EIO) {
		t.Fatalf("failed producer wait = %v", err)
	}
	if err := h.WaitVerified(ctx, 3); !errors.Is(err, syscall.EIO) {
		t.Fatalf("failed producer accepted prefix = %v", err)
	}
	if _, err := c.Open("verified"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed producer published prefix: %v", err)
	}
}

func TestGrowingVerifiedRestoredCache(t *testing.T) {
	dir := t.TempDir()
	c, err := NewCache(dir, 32)
	if err != nil {
		t.Fatal(err)
	}
	h, err := c.AcquireGrowing(context.Background(), context.Background(), "restored", 6, func(_ context.Context, w io.Writer) error {
		_, err := io.WriteString(w, "abcdef")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := h.WaitVerified(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	_ = h.Close()
	_ = c.Close()
	c, err = NewCache(dir, 32)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	h, err = c.AcquireGrowing(context.Background(), context.Background(), "restored", 6, func(context.Context, io.Writer) error {
		return errors.New("restored cache must not refill")
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if !h.Materialized() {
		t.Fatal("cache object was not restored")
	}
	if err := h.WaitVerified(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.WaitVerified(context.Background(), 3); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed validation wait = %v", err)
	}
}
