package storage

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func TestOpenCachedDoesNotWaitForFill(t *testing.T) {
	c, err := NewCache(t.TempDir(), 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	started, finish := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		h, err := c.Acquire(context.Background(), "data", 4, func(_ context.Context, w io.Writer) error {
			close(started)
			<-finish
			_, err := io.WriteString(w, "test")
			return err
		})
		if h != nil {
			_ = h.Close()
		}
		done <- err
	}()
	defer func() { close(finish); <-done }()
	<-started
	opened := make(chan error, 1)
	go func() {
		h, err := c.OpenCached("data", 4)
		if h != nil {
			_ = h.Close()
			err = errors.New("returned unpublished object")
		}
		opened <- err
	}()
	select {
	case err := <-opened:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("OpenCached waited for a running fill")
	}
}

func TestOpenCachedPinsCompleteMatchingObject(t *testing.T) {
	c, err := NewCache(t.TempDir(), 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Store(context.Background(), "data", []byte("test")); err != nil {
		t.Fatal(err)
	}
	if h, err := c.OpenCached("data", 3); h != nil || err != nil {
		t.Fatalf("wrong size returned an object: %v %v", h, err)
	}
	h, err := c.OpenCached("data", 4)
	if err != nil || h == nil {
		t.Fatalf("complete object not found: %v", err)
	}
	if got := c.Stats().PinnedBytes; got != 4 {
		t.Fatalf("pinned bytes=%d", got)
	}
	buf := make([]byte, 4)
	if _, err := h.ReadAt(buf, 0); err != nil || string(buf) != "test" {
		t.Fatalf("content=%q err=%v", buf, err)
	}
	_ = h.Close()
	if got := c.Stats().PinnedBytes; got != 0 {
		t.Fatalf("pin leaked: %d", got)
	}
}
