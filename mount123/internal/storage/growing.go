package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"time"
)

type growingFlight struct {
	mu      sync.Mutex
	cache   *Cache
	id      string
	key     string
	temp    string
	target  string
	size    int64
	written int64
	refs    int
	state   uint8
	err     error
	changed chan struct{}
	done    chan struct{}
	cancel  context.CancelFunc
}

const (
	growingRunning uint8 = iota
	growingComplete
	growingFailed
)

// GrowingHandle reads ranges from a shared cache fill as they become
// available. Only a successful, complete fill is published to the cache.
type GrowingHandle struct {
	mu      sync.Mutex
	flight  *growingFlight
	file    *os.File
	ready   *Handle
	size    int64
	closed  bool
	closedC chan struct{}
}

// AcquireGrowing joins or starts an asynchronous cache fill. lifetime owns the
// fill task; ctx only bounds this caller's acquisition wait.
func (c *Cache) AcquireGrowing(ctx, lifetime context.Context, key string, size int64, fill func(context.Context, io.Writer) error) (*GrowingHandle, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if fill == nil {
		return nil, errors.New("cache fill callback is nil")
	}
	if size < 0 || size > c.max {
		return nil, syscall.ENOSPC
	}
	if lifetime == nil {
		lifetime = context.Background()
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if h, err := c.existing(key, size); err != nil {
			return nil, err
		} else if h != nil {
			return &GrowingHandle{ready: h, size: size, closedC: make(chan struct{})}, nil
		}
		id := cacheID(key)
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return nil, ErrClosed
		}
		if e := c.entries[id]; e != nil && e.size != size {
			if e.pins != 0 {
				c.mu.Unlock()
				return nil, syscall.ENOSPC
			}
			_ = os.Remove(e.path)
			delete(c.entries, id)
			c.used -= e.size
		}
		if f := c.flights[id]; f != nil {
			done := f.done
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-done:
				continue
			}
		}
		if f := c.growing[id]; f != nil {
			f.mu.Lock()
			if f.state != growingRunning {
				f.mu.Unlock()
				c.mu.Unlock()
				continue
			}
			file, err := os.Open(f.temp)
			if err != nil {
				f.mu.Unlock()
				c.mu.Unlock()
				return nil, err
			}
			f.refs++
			reader := &GrowingHandle{flight: f, file: file, size: size, closedC: make(chan struct{})}
			f.mu.Unlock()
			c.mu.Unlock()
			return reader, nil
		}
		if err := c.evictLocked(size); err != nil {
			c.mu.Unlock()
			return nil, err
		}
		writer, err := os.CreateTemp(c.dir, ".fill-")
		if err != nil {
			c.mu.Unlock()
			return nil, err
		}
		if err := writer.Chmod(0600); err != nil {
			_ = writer.Close()
			_ = os.Remove(writer.Name())
			c.mu.Unlock()
			return nil, err
		}
		readerFile, err := os.Open(writer.Name())
		if err != nil {
			_ = writer.Close()
			_ = os.Remove(writer.Name())
			c.mu.Unlock()
			return nil, err
		}
		fillCtx, cancel := context.WithCancel(lifetime)
		f := &growingFlight{cache: c, id: id, key: key, temp: writer.Name(), target: c.filename(key), size: size, refs: 1, changed: make(chan struct{}), done: make(chan struct{}), cancel: cancel}
		c.growing[id] = f
		c.reserved += size
		reader := &GrowingHandle{flight: f, file: readerFile, size: size, closedC: make(chan struct{})}
		c.mu.Unlock()
		go f.run(fillCtx, writer, fill)
		return reader, nil
	}
}

func (f *growingFlight) run(ctx context.Context, writer *os.File, fill func(context.Context, io.Writer) error) {
	err := fill(ctx, &growingWriter{flight: f, file: writer})
	if err == nil {
		f.mu.Lock()
		if f.written != f.size {
			err = fmt.Errorf("cache fill size mismatch: got %d, want %d", f.written, f.size)
		}
		f.mu.Unlock()
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = writer.Sync()
	}
	if closeErr := writer.Close(); err == nil {
		err = closeErr
	}
	c := f.cache
	c.mu.Lock()
	if err == nil && c.closed {
		err = ErrClosed
	}
	f.mu.Lock()
	if err == nil && f.refs == 0 {
		err = context.Canceled
	}
	f.mu.Unlock()
	if err == nil {
		err = os.Rename(f.temp, f.target)
	}
	if err == nil {
		c.reserved -= f.size
		c.entries[f.id] = &cacheEntry{path: f.target, size: f.size, used: time.Now(), pins: f.refs}
		c.used += f.size
		delete(c.growing, f.id)
		f.mu.Lock()
		f.state = growingComplete
		f.err = nil
		signalGrowingChange(f)
		close(f.done)
		f.mu.Unlock()
		c.mu.Unlock()
		f.cancel()
		return
	}
	c.reserved -= f.size
	delete(c.growing, f.id)
	c.mu.Unlock()
	_ = os.Remove(f.temp)
	f.mu.Lock()
	f.state = growingFailed
	f.err = err
	signalGrowingChange(f)
	close(f.done)
	f.mu.Unlock()
	f.cancel()
}

func (f *growingFlight) release() {
	c := f.cache
	c.mu.Lock()
	f.mu.Lock()
	if f.refs > 0 {
		f.refs--
	}
	if f.state == growingComplete {
		if entry := c.entries[f.id]; entry != nil && entry.pins > 0 {
			entry.pins--
		}
	} else if f.state == growingRunning && f.refs == 0 {
		f.cancel()
	}
	f.mu.Unlock()
	c.mu.Unlock()
}

func (h *GrowingHandle) Size() int64 { return h.size }

func (h *GrowingHandle) ReadAt(ctx context.Context, p []byte, off int64) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if h.ready != nil {
		return h.ready.ReadAt(p, off)
	}
	if off < 0 {
		return 0, syscall.EINVAL
	}
	if len(p) == 0 {
		return 0, nil
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if off >= h.size || len(p) == 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > h.size-off {
		p = p[:h.size-off]
	}
	end := off + int64(len(p))
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		h.flight.mu.Lock()
		written, state, fillErr, changed := h.flight.written, h.flight.state, h.flight.err, h.flight.changed
		h.flight.mu.Unlock()
		if state == growingFailed {
			if fillErr == nil {
				fillErr = io.ErrUnexpectedEOF
			}
			return 0, fillErr
		}
		if written >= end || (state == growingComplete && written >= end) {
			h.mu.Lock()
			if h.closed {
				h.mu.Unlock()
				return 0, os.ErrClosed
			}
			n, err := h.file.ReadAt(p, off)
			h.mu.Unlock()
			return n, err
		}
		if state != growingRunning {
			if fillErr == nil {
				fillErr = io.ErrUnexpectedEOF
			}
			return 0, fillErr
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-h.closedC:
			return 0, os.ErrClosed
		case <-changed:
		}
	}
}

func (h *GrowingHandle) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	close(h.closedC)
	if h.ready != nil {
		err := h.ready.Close()
		h.mu.Unlock()
		return err
	}
	file := h.file
	h.mu.Unlock()
	err := file.Close()
	h.flight.release()
	return err
}

type growingWriter struct {
	flight *growingFlight
	file   *os.File
}

func (w *growingWriter) Write(p []byte) (int, error) {
	f := w.flight
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state != growingRunning {
		return 0, os.ErrClosed
	}
	if int64(len(p)) > f.size-f.written {
		return 0, errors.New("cache fill exceeds declared size")
	}
	n, err := w.file.Write(p)
	if n > 0 {
		f.written += int64(n)
		signalGrowingChange(f)
	}
	return n, err
}

func signalGrowingChange(f *growingFlight) {
	close(f.changed)
	f.changed = make(chan struct{})
}
