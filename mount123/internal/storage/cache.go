package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

var ErrClosed = errors.New("storage cache is closed")

type cacheEntry struct {
	path string
	size int64
	used time.Time
	pins int
}
type flight struct {
	done chan struct{}
	err  error
}

// Cache is a persistent, size-bounded cache. Its directory is exclusively
// owned by one process for the lifetime of the Cache.
type Cache struct {
	mu                  sync.Mutex
	dir                 string
	max, used, reserved int64
	entries             map[string]*cacheEntry
	flights             map[string]*flight
	lock                *os.File
	closed              bool
}

// NewCache opens an exclusive cache directory with the requested byte limit.
func NewCache(dir string, maxBytes int64) (*Cache, error) {
	if maxBytes < 0 {
		return nil, fmt.Errorf("cache size must be non-negative")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("cache directory is already in use: %w", err)
	}
	if err = lock.Chmod(0600); err != nil {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
		return nil, err
	}
	c := &Cache{dir: dir, max: maxBytes, entries: make(map[string]*cacheEntry), flights: make(map[string]*flight), lock: lock}
	if err = c.load(); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func (c *Cache) filename(key string) string {
	h := sha256.Sum256([]byte(key))
	return filepath.Join(c.dir, hex.EncodeToString(h[:])+".blob")
}
func cacheID(key string) string { h := sha256.Sum256([]byte(key)); return hex.EncodeToString(h[:]) }
func (c *Cache) load() error {
	items, err := os.ReadDir(c.dir)
	if err != nil {
		return err
	}
	for _, item := range items {
		name := item.Name()
		if strings.HasPrefix(name, ".fill-") {
			_ = os.Remove(filepath.Join(c.dir, name))
			continue
		}
		if item.IsDir() || len(name) != 69 || name[64:] != ".blob" {
			continue
		}
		p := filepath.Join(c.dir, name)
		st, e := os.Stat(p)
		if e != nil {
			continue
		}
		if !st.Mode().IsRegular() {
			continue
		}
		key := name[:64]
		c.entries[key] = &cacheEntry{path: p, size: st.Size(), used: st.ModTime()}
		c.used += st.Size()
	}
	// Old and oversized entries are removed using the same bounded eviction rule.
	return c.evictLocked(0)
}

func (c *Cache) evictLocked(need int64) error {
	for c.used+c.reserved+need > c.max {
		var oldestKey string
		var oldest *cacheEntry
		for k, e := range c.entries {
			if e.pins == 0 && (oldest == nil || e.used.Before(oldest.used)) {
				oldestKey, oldest = k, e
			}
		}
		if oldest == nil {
			return syscall.ENOSPC
		}
		if err := os.Remove(oldest.path); err != nil && !os.IsNotExist(err) {
			return err
		}
		delete(c.entries, oldestKey)
		c.used -= oldest.size
	}
	return nil
}

// Acquire returns a pinned handle for key, filling and atomically publishing it
// on a miss. Concurrent misses for the same key share one fill operation.
func (c *Cache) Acquire(ctx context.Context, key string, size int64, fill func(context.Context, io.Writer) error) (*Handle, error) {
	if size < 0 || size > c.max {
		return nil, syscall.ENOSPC
	}
	id := cacheID(key)
	for {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return nil, ErrClosed
		}
		if e := c.entries[id]; e != nil && e.size == size {
			f, err := os.Open(e.path)
			if err == nil {
				e.pins++
				e.used = time.Now()
				_ = os.Chtimes(e.path, e.used, e.used)
				c.mu.Unlock()
				return &Handle{file: f, cache: c, key: id, size: size}, nil
			}
			if e.pins != 0 {
				c.mu.Unlock()
				return nil, err
			}
			delete(c.entries, id)
			c.used -= e.size
		} else if e != nil {
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
				if f.err != nil {
					return nil, f.err
				}
				continue
			}
		}
		if err := c.evictLocked(size); err != nil {
			c.mu.Unlock()
			return nil, err
		}
		f := &flight{done: make(chan struct{})}
		c.flights[id] = f
		c.reserved += size
		c.mu.Unlock()
		f.err = c.fill(ctx, key, size, fill)
		c.mu.Lock()
		c.reserved -= size
		if f.err == nil && !c.closed {
			p := c.filename(key)
			st, err := os.Stat(p)
			if err != nil || st.Size() != size {
				f.err = fmt.Errorf("cache fill published invalid file")
			} else {
				c.entries[id] = &cacheEntry{path: p, size: size, used: time.Now()}
				c.used += size
			}
		} else if f.err == nil {
			_ = os.Remove(c.filename(key))
		}
		delete(c.flights, id)
		close(f.done)
		c.mu.Unlock()
		if f.err != nil {
			return nil, f.err
		}
	}
}

func (c *Cache) fill(ctx context.Context, key string, size int64, fill func(context.Context, io.Writer) error) error {
	if fill == nil {
		return errors.New("cache fill callback is nil")
	}
	tmp, err := os.CreateTemp(c.dir, ".fill-")
	if err != nil {
		return err
	}
	temp := tmp.Name()
	defer os.Remove(temp)
	if err = tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	w := &limitedWriter{w: tmp, left: size}
	if err = fill(ctx, w); err != nil {
		tmp.Close()
		return err
	}
	if w.written != size {
		tmp.Close()
		return fmt.Errorf("cache fill size mismatch: got %d, want %d", w.written, size)
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(temp, c.filename(key)); err != nil {
		return err
	}
	return nil
}

type limitedWriter struct {
	w             io.Writer
	left, written int64
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.left {
		return 0, fmt.Errorf("cache fill exceeds declared size")
	}
	n, e := w.w.Write(p)
	w.left -= int64(n)
	w.written += int64(n)
	return n, e
}

// Close releases the directory lock and prevents new acquisitions.
func (c *Cache) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	var pending []<-chan struct{}
	for _, f := range c.flights {
		pending = append(pending, f.done)
	}
	c.mu.Unlock()
	for _, done := range pending {
		<-done
	}
	e := syscall.Flock(int(c.lock.Fd()), syscall.LOCK_UN)
	ce := c.lock.Close()
	if e != nil {
		return e
	}
	return ce
}

// Handle pins an immutable cache file until Close.
type Handle struct {
	mu     sync.Mutex
	file   *os.File
	cache  *Cache
	key    string
	size   int64
	closed bool
}

func (h *Handle) Size() int64 { return h.size }
func (h *Handle) ReadAt(p []byte, off int64) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return 0, os.ErrClosed
	}
	return h.file.ReadAt(p, off)
}
func (h *Handle) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	err := h.file.Close()
	h.mu.Unlock()
	c := h.cache
	c.mu.Lock()
	if e := c.entries[h.key]; e != nil && e.pins > 0 {
		e.pins--
	}
	c.mu.Unlock()
	return err
}
