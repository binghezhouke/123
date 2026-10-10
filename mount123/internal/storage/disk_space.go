package storage

import (
	"os"
	"syscall"
)

// diskFreeBytes is kept behind a function field on Cache so deterministic
// tests can model ENOSPC and reserve pressure without allocating large files.
func diskFreeBytes(path string) (int64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return int64(stat.Bavail) * int64(stat.Bsize), nil
}

func (c *Cache) diskFreeBytesLocked() int64 {
	if c.statfs == nil {
		return -1
	}
	free, err := c.statfs(c.dir)
	if err != nil {
		return -1
	}
	return free
}

// diskAdmissionLocked applies the filesystem reserve. The caller holds c.mu.
// A statfs failure is treated as unknown and leaves the cache's byte budget as
// the safety boundary. Background work is deliberately the first work shed.
func (c *Cache) diskAdmissionLocked(ctxBackground bool, size int64) error {
	if c.minFreeBytes <= 0 || c.statfs == nil {
		return nil
	}
	free, err := c.statfs(c.dir)
	if err != nil {
		return nil
	}
	if free >= c.minFreeBytes {
		return nil
	}
	c.telemetry.diskPressureEvents++
	c.evictSpeculativeForDiskPressureLocked()
	if ctxBackground || free < size {
		c.telemetry.diskPressureRejects++
		c.recordENOSPCLocked()
		return syscall.ENOSPC
	}
	return nil
}

func (c *Cache) evictSpeculativeForDiskPressureLocked() {
	queue := c.lru[cacheSpeculative]
	for node := queue.Back(); node != nil; {
		previous := node.Prev()
		id := node.Value.(string)
		if entry := c.entries[id]; entry != nil && entry.pins == 0 {
			if err := os.Remove(entry.path); err == nil || os.IsNotExist(err) {
				c.removeEntryLocked(id, entry)
				c.telemetry.diskPressureEvictions++
			}
		}
		node = previous
	}
}
