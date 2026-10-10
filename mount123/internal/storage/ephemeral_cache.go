package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// NewEphemeralCache creates a process-private cache that skips file Sync.
// Its directory is unique to this instance and removed on close. A later
// process removes stale ephemeral directories only after acquiring their lock,
// so bytes left by a crash are never reused.
func NewEphemeralCache(root string, maxBytes int64) (*Cache, error) {
	return NewEphemeralCacheWithDownloadConfig(root, maxBytes, DefaultDownloadConfig())
}

// NewEphemeralCacheWithDownloadConfig creates a process-private cache with a
// configured process-wide HTTP transfer budget.
func NewEphemeralCacheWithDownloadConfig(root string, maxBytes int64, download DownloadConfig) (*Cache, error) {
	return NewEphemeralCacheWithIndexBudget(root, maxBytes, defaultIndexBudget(maxBytes), download)
}

// NewEphemeralCacheWithIndexBudget creates a process-private cache with an
// independent archive-index budget.
func NewEphemeralCacheWithIndexBudget(root string, maxBytes, indexBudget int64, download DownloadConfig) (*Cache, error) {
	if maxBytes < 0 {
		return nil, fmt.Errorf("cache size must be non-negative")
	}
	if indexBudget < 0 || indexBudget > maxBytes {
		return nil, fmt.Errorf("index budget must be between 0 and cache size")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(root, 0700); err != nil {
		return nil, err
	}
	guard, err := os.OpenFile(filepath.Join(root, ".ephemeral-cleanup.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(guard.Fd()), syscall.LOCK_EX); err != nil {
		_ = guard.Close()
		return nil, err
	}
	if err = removeStaleEphemeralDirs(root); err != nil {
		_ = syscall.Flock(int(guard.Fd()), syscall.LOCK_UN)
		_ = guard.Close()
		return nil, err
	}
	dir, err := os.MkdirTemp(root, ".ephemeral-")
	if err != nil {
		_ = syscall.Flock(int(guard.Fd()), syscall.LOCK_UN)
		_ = guard.Close()
		return nil, err
	}
	c, err := NewCacheWithIndexBudget(dir, maxBytes, indexBudget, download)
	_ = syscall.Flock(int(guard.Fd()), syscall.LOCK_UN)
	_ = guard.Close()
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	c.durable = false
	c.ephemeral = true
	return c, nil
}

func removeStaleEphemeralDirs(root string) error {
	items, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, item := range items {
		if !item.IsDir() || !strings.HasPrefix(item.Name(), ".ephemeral-") {
			continue
		}
		dir := filepath.Join(root, item.Name())
		lock, openErr := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
		if openErr != nil {
			if os.IsNotExist(openErr) {
				if err = os.RemoveAll(dir); err != nil {
					return err
				}
				continue
			}
			return openErr
		}
		flockErr := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if flockErr == nil {
			removeErr := os.RemoveAll(dir)
			_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
			_ = lock.Close()
			if removeErr != nil {
				return removeErr
			}
			continue
		}
		_ = lock.Close()
		if flockErr != syscall.EWOULDBLOCK && flockErr != syscall.EAGAIN {
			return flockErr
		}
	}
	return nil
}
