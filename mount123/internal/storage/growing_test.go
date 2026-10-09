package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestRetainedGrowingFillSurvivesLastCloseAndStopsWithLifetime(t *testing.T) {
	for _, cancelFill := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelFill), func(t *testing.T) {
			c, err := NewCache(t.TempDir(), 32)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			lifetime, cancel := context.WithCancel(context.Background())
			defer cancel()
			started, proceed, ended := make(chan struct{}), make(chan struct{}), make(chan struct{})
			h, err := c.AcquireGrowingRetained(context.Background(), lifetime, "retained", 6, func(ctx context.Context, w io.Writer) error {
				defer close(ended)
				if _, err := io.WriteString(w, "abc"); err != nil {
					return err
				}
				close(started)
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-proceed:
				}
				_, err := io.WriteString(w, "def")
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			<-started
			if err := h.Close(); err != nil {
				t.Fatal(err)
			}
			if cancelFill {
				cancel()
			} else {
				close(proceed)
			}
			select {
			case <-ended:
			case <-time.After(time.Second):
				t.Fatal("retained fill did not finish")
			}
			deadline := time.Now().Add(time.Second)
			for c.Stats().ReservedBytes != 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if c.Stats().ReservedBytes != 0 {
				t.Fatal("retained reservation leaked")
			}
			cached, err := c.Open("retained")
			if cancelFill {
				if err == nil {
					cached.Close()
					t.Fatal("cancelled fill published")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				defer cached.Close()
				data := make([]byte, 6)
				if _, err := cached.ReadAt(data, 0); err != nil || string(data) != "abcdef" {
					t.Fatalf("retained data=%q err=%v", data, err)
				}
			}
		})
	}
}

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
	if h1.Materialized() || h2.Materialized() {
		t.Fatal("growing prefix was marked as materialized")
	}
	got := make([]byte, 3)
	if n, err := h2.ReadAt(context.Background(), got, 0); err != nil || n != 3 || !bytes.Equal(got, []byte("abc")) {
		t.Fatalf("prefix ReadAt = %q, %d, %v", got, n, err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := h1.ReadAt(waitCtx, make([]byte, 3), 3); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unwritten range ReadAt = %v, want deadline", err)
	}
	if err := h1.Wait(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait = %v, want deadline", err)
	}
	close(release)
	if err := h1.Wait(context.Background()); err != nil {
		t.Fatalf("Wait after fill completion: %v", err)
	}
	if !h1.Materialized() || !h2.Materialized() {
		t.Fatal("completed growing fill was not marked as materialized")
	}
	got = make([]byte, 6)
	if n, err := h1.ReadAt(context.Background(), got, 0); err != nil || n != 6 || !bytes.Equal(got, []byte("abcdef")) {
		t.Fatalf("complete ReadAt = %q, %d, %v", got, n, err)
	}
	if n := fills.Load(); n != 1 {
		t.Fatalf("fill called %d times", n)
	}
	truncated := make([]byte, 4)
	if n, err := h1.ReadAt(context.Background(), truncated, 4); n != 2 || !errors.Is(err, io.EOF) || string(truncated[:n]) != "ef" {
		t.Fatalf("read past growing EOF = %q, %d, %v; want %q, 2, EOF", truncated, n, err, "ef")
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
	if h.Materialized() {
		t.Fatal("failed growing fill was marked as materialized")
	}
	if err := h.Wait(context.Background()); !errors.Is(err, syscall.EIO) {
		t.Fatalf("Wait error = %v, want EIO", err)
	}
	if _, err := h.ReadAt(context.Background(), make([]byte, 6), 0); !errors.Is(err, syscall.EIO) {
		t.Fatalf("incomplete range ReadAt = %v, want EIO", err)
	}
	_ = h.Close()
	if _, err := c.Open("failed"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial growing output became a cache entry: %v", err)
	}
}

func TestGrowingReadAtFinalRangeWaitsForValidation(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "success"
		if fail {
			name = "failure"
		}
		t.Run(name, func(t *testing.T) {
			c, err := NewCache(t.TempDir(), 16)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			written := make(chan struct{})
			validate := make(chan struct{})
			h, err := c.AcquireGrowing(context.Background(), context.Background(), "final-validation", 6, func(ctx context.Context, w io.Writer) error {
				if _, err := io.WriteString(w, "abcdef"); err != nil {
					return err
				}
				close(written)
				select {
				case <-validate:
				case <-ctx.Done():
					return ctx.Err()
				}
				if fail {
					return syscall.EIO
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			<-written

			prefix := make([]byte, 3)
			if n, err := h.ReadAt(context.Background(), prefix, 0); n != 3 || err != nil || string(prefix) != "abc" {
				t.Fatalf("prefix read = %q, %d, %v", prefix, n, err)
			}
			waitCtx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
			defer cancel()
			final := make([]byte, 3)
			if n, err := h.ReadAt(waitCtx, final, 3); n != 0 || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("final read before validation = %q, %d, %v; want deadline", final, n, err)
			}

			close(validate)
			if fail {
				if n, err := h.ReadAt(context.Background(), final, 3); n != 0 || !errors.Is(err, syscall.EIO) {
					t.Fatalf("final read after failed validation = %q, %d, %v; want EIO", final, n, err)
				}
				if err := h.Wait(context.Background()); !errors.Is(err, syscall.EIO) {
					t.Fatalf("Wait after failed validation = %v, want EIO", err)
				}
				if _, err := c.Open("final-validation"); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("failed data was published: %v", err)
				}
				return
			}
			if n, err := h.ReadAt(context.Background(), final, 3); n != 3 || err != nil || string(final) != "def" {
				t.Fatalf("final read after validation = %q, %d, %v", final, n, err)
			}
			if err := h.Wait(context.Background()); err != nil {
				t.Fatalf("Wait after validation: %v", err)
			}
			if !h.Materialized() {
				t.Fatal("successful data was not materialized")
			}
		})
	}
}

func TestAcquireGrowingPublishesLRUAndRangeCoverage(t *testing.T) {
	c, err := NewCache(t.TempDir(), 6)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	identity := cacheID("growing-range")
	key := rangeKey(identity, 0, 6)
	h, err := c.AcquireGrowing(context.Background(), context.Background(), key, 6, func(_ context.Context, w io.Writer) error {
		_, err := io.WriteString(w, "abcdef")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if missing := c.missingRanges(identity, 0, 6); len(missing) != 0 {
		t.Fatalf("growing range was not registered: %v", missing)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	other, err := c.Acquire(context.Background(), "other", 6, func(_ context.Context, w io.Writer) error {
		_, err := io.WriteString(w, "uvwxyz")
		return err
	})
	if err != nil {
		t.Fatalf("completed growing entry was not LRU-evictable: %v", err)
	}
	_ = other.Close()
	if _, err := c.Open(key); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("evicted growing entry still exists: %v", err)
	}
}

func TestAcquireGrowingDoesNotJoinSameKeyWithDifferentSize(t *testing.T) {
	c, err := NewCache(t.TempDir(), 32)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	started := make(chan struct{})
	first, err := c.AcquireGrowing(context.Background(), context.Background(), "size-mismatch", 6, func(ctx context.Context, w io.Writer) error {
		if _, err := io.WriteString(w, "abcd"); err != nil {
			return err
		}
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started

	secondFill := atomic.Int32{}
	result := make(chan *GrowingHandle, 1)
	errResult := make(chan error, 1)
	go func() {
		h, err := c.AcquireGrowing(context.Background(), context.Background(), "size-mismatch", 4, func(_ context.Context, w io.Writer) error {
			secondFill.Add(1)
			_, err := io.WriteString(w, "wxyz")
			return err
		})
		if err != nil {
			errResult <- err
			return
		}
		result <- h
	}()
	select {
	case h := <-result:
		_ = h.Close()
		t.Fatal("different-size request joined the active growing fill")
	case err := <-errResult:
		t.Fatalf("different-size request failed early: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	var second *GrowingHandle
	select {
	case second = <-result:
	case err := <-errResult:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("different-size request did not retry after active fill ended")
	}
	defer second.Close()
	if err := second.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	if n, err := second.ReadAt(context.Background(), got, 0); err != nil || n != len(got) || string(got) != "wxyz" {
		t.Fatalf("different-size fill read = %q, %d, %v", got, n, err)
	}
	if secondFill.Load() != 1 {
		t.Fatalf("replacement fill count = %d, want 1", secondFill.Load())
	}
}

func TestAcquireGrowingSizeReplacementRemovesOldLRUNode(t *testing.T) {
	c, err := NewCache(t.TempDir(), 16)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	first, err := c.AcquireGrowing(context.Background(), context.Background(), "stored-size-mismatch", 6, func(_ context.Context, w io.Writer) error {
		_, err := io.WriteString(w, "abcdef")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := c.AcquireGrowing(context.Background(), context.Background(), "stored-size-mismatch", 4, func(_ context.Context, w io.Writer) error {
		_, err := io.WriteString(w, "wxyz")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.used != 4 || c.lruLen() != 1 {
		t.Fatalf("size replacement left stale cache state: used=%d lru=%d", c.used, c.lruLen())
	}
}

func TestAcquireAndAcquireGrowingShareSameKeyFill(t *testing.T) {
	t.Run("growing-first", func(t *testing.T) {
		c, err := NewCache(t.TempDir(), 32)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		started, release := make(chan struct{}), make(chan struct{})
		var ordinaryFills atomic.Int32
		growing, err := c.AcquireGrowing(context.Background(), context.Background(), "mixed-growing-first", 6, func(ctx context.Context, w io.Writer) error {
			if _, err := io.WriteString(w, "abc"); err != nil {
				return err
			}
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			_, err := io.WriteString(w, "def")
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		defer growing.Close()
		<-started
		result := make(chan *Handle, 1)
		errResult := make(chan error, 1)
		go func() {
			h, err := c.Acquire(context.Background(), "mixed-growing-first", 6, func(context.Context, io.Writer) error {
				ordinaryFills.Add(1)
				return errors.New("ordinary fill must join the growing fill")
			})
			if err != nil {
				errResult <- err
				return
			}
			result <- h
		}()
		select {
		case h := <-result:
			_ = h.Close()
			t.Fatal("ordinary Acquire returned before growing fill completed")
		case err := <-errResult:
			t.Fatalf("ordinary Acquire failed early: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
		close(release)
		if err := growing.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		var ordinary *Handle
		select {
		case ordinary = <-result:
		case err := <-errResult:
			t.Fatal(err)
		case <-time.After(time.Second):
			t.Fatal("ordinary Acquire did not resume after growing fill")
		}
		defer ordinary.Close()
		got := make([]byte, 6)
		if n, err := ordinary.ReadAt(got, 0); n != len(got) || err != nil || string(got) != "abcdef" {
			t.Fatalf("ordinary read = %q, %d, %v", got, n, err)
		}
		if ordinaryFills.Load() != 0 {
			t.Fatalf("ordinary fill called %d times", ordinaryFills.Load())
		}
	})

	t.Run("ordinary-first", func(t *testing.T) {
		c, err := NewCache(t.TempDir(), 32)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		started, release := make(chan struct{}), make(chan struct{})
		regularResult := make(chan *Handle, 1)
		errResult := make(chan error, 1)
		go func() {
			h, err := c.Acquire(context.Background(), "mixed-ordinary-first", 6, func(ctx context.Context, w io.Writer) error {
				if _, err := io.WriteString(w, "abc"); err != nil {
					return err
				}
				close(started)
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
				_, err := io.WriteString(w, "def")
				return err
			})
			if err != nil {
				errResult <- err
				return
			}
			regularResult <- h
		}()
		<-started
		growingResult := make(chan *GrowingHandle, 1)
		go func() {
			h, err := c.AcquireGrowing(context.Background(), context.Background(), "mixed-ordinary-first", 6, func(context.Context, io.Writer) error {
				return errors.New("growing fill must join the ordinary fill")
			})
			if err != nil {
				errResult <- err
				return
			}
			growingResult <- h
		}()
		select {
		case h := <-growingResult:
			_ = h.Close()
			t.Fatal("GrowingAcquire returned before ordinary fill completed")
		case err := <-errResult:
			t.Fatalf("GrowingAcquire failed early: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
		close(release)
		var regular *Handle
		select {
		case regular = <-regularResult:
		case err := <-errResult:
			t.Fatal(err)
		case <-time.After(time.Second):
			t.Fatal("ordinary fill did not complete")
		}
		defer regular.Close()
		var growing *GrowingHandle
		select {
		case growing = <-growingResult:
		case err := <-errResult:
			t.Fatal(err)
		case <-time.After(time.Second):
			t.Fatal("GrowingAcquire did not resume after ordinary fill")
		}
		defer growing.Close()
		if !growing.Materialized() || growing.Wait(context.Background()) != nil {
			t.Fatal("GrowingAcquire did not attach to the completed ordinary cache object")
		}
	})
}
