package storage

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// IsEphemeral reports whether this cache is removed when it closes.
func (c *Cache) IsEphemeral() bool { return c != nil && c.ephemeral }

// ReplaceArchiveIndex atomically replaces a metadata object while preserving
// the old object if writing or capacity admission fails. The object remains
// part of the ordinary bounded cache budget.
func (c *Cache) ReplaceArchiveIndex(ctx context.Context, key string, data []byte) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	size := int64(len(data))
	if size > c.max {
		return syscall.ENOSPC
	}
	id := cacheID(key)
	var operation *flight
	var old *cacheEntry
	var newClass cacheClass
	var delta int64
	var reserveIndex int64

	for {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return ErrClosed
		}
		if active := c.flights[id]; active != nil {
			done := active.done
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-done:
			}
			continue
		}
		old = c.entries[id]
		if old != nil && old.pins != 0 {
			c.mu.Unlock()
			return syscall.EBUSY
		}
		delta = size
		newClass = cacheIndex
		if old != nil {
			delta -= old.size
			newClass = old.class
		}
		if newClass == cacheIndex {
			otherIndexBytes := c.indexUsed
			if old != nil && old.class == cacheIndex {
				otherIndexBytes -= old.size
			}
			if size > c.indexBudget-otherIndexBytes-c.reservedIndex {
				newClass = cacheProbation
			}
		}

		// The old object remains on disk while the complete new temp object is
		// written, so reserve the full new size rather than only the final delta.
		need := max(int64(0), c.used+c.reserved+size-c.max)
		if need > 0 {
			available := int64(0)
			for class := cacheSpeculative; class <= cacheIndex; class++ {
				for node := c.lru[class].Back(); node != nil; node = node.Prev() {
					victimID := node.Value.(string)
					if victimID == id {
						continue
					}
					if candidate := c.entries[victimID]; candidate != nil && candidate.pins == 0 {
						available += candidate.size
					}
				}
			}
			if need > available {
				c.recordENOSPCLocked()
				c.mu.Unlock()
				return syscall.ENOSPC
			}
		}
		if old != nil {
			old.pins++ // Keep the last good object protected until commit or abort.
		}
		if need > 0 {
			if err := c.evictLocked(need); err != nil {
				if old != nil {
					old.pins--
				}
				c.mu.Unlock()
				return err
			}
		}
		operation = &flight{done: make(chan struct{})}
		c.flights[id] = operation
		c.reserved += size
		if newClass == cacheIndex {
			reserveIndex = size
			c.reservedIndex += reserveIndex
		}
		c.mu.Unlock()
		break
	}

	tmp, err := os.CreateTemp(c.dir, ".metadata-")
	if err == nil {
		err = tmp.Chmod(0600)
	}
	if err == nil {
		var n int
		n, err = tmp.Write(data)
		if err == nil && n != len(data) {
			err = io.ErrShortWrite
		}
	}
	if err == nil && c.durable {
		syncFile := c.syncFile
		if syncFile == nil {
			syncFile = func(f *os.File) error { return f.Sync() }
		}
		err = syncFile(tmp)
	}
	if tmp != nil {
		if closeErr := tmp.Close(); err == nil {
			err = closeErr
		}
	}
	var tempPath string
	if tmp != nil {
		tempPath = tmp.Name()
		defer os.Remove(tempPath)
	}
	if err == nil {
		err = ctx.Err()
	}

	c.mu.Lock()
	if err == nil && c.closed {
		err = ErrClosed
	}
	if err == nil {
		current := c.entries[id]
		if current != old || old != nil && old.pins != 1 {
			err = syscall.EBUSY
		} else {
			commitPath := c.pathForKey(key, newClass)
			if old != nil {
				commitPath = old.path
			}
			err = os.Rename(tempPath, commitPath)
			if err == nil {
				now := time.Now()
				if old == nil {
					old = &cacheEntry{path: commitPath, class: newClass, kind: indexKindForKey(key), size: size, used: now, lastTouch: now}
					old.lru = c.lru[newClass].PushFront(id)
					c.entries[id] = old
					c.used += size
					if newClass == cacheIndex {
						c.indexUsed += size
					}
				} else {
					oldClass := old.class
					if oldClass == cacheIndex {
						c.indexUsed += delta
					}
					old.size = size
					if newClass != oldClass {
						c.setClassLocked(id, old, newClass)
					}
					old.used, old.lastTouch = now, now
					c.used += delta
				}
			}
		}
	}
	c.reserved -= size
	c.reservedIndex -= reserveIndex
	if old != nil && old.pins > 0 {
		old.pins--
	}
	operation.err = err
	delete(c.flights, id)
	close(operation.done)
	c.mu.Unlock()
	if err != nil {
		return err
	}
	if c.durable {
		if dir, openErr := os.Open(filepath.Dir(tempPath)); openErr == nil {
			_ = dir.Sync()
			_ = dir.Close()
		}
	}
	return nil
}
