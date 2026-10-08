package storage

import (
	"container/list"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/binghezhouke/123/mount123/internal/iostats"
	"github.com/binghezhouke/123/mount123/internal/workqueue"
)

var ErrClosed = errors.New("storage cache is closed")

type cacheEntry struct {
	path                 string
	size                 int64
	class                cacheClass
	hasUse               bool
	scanDir              int8
	lastUseStart         int64
	lastUseEnd           int64
	used                 time.Time
	lastTouch            time.Time
	pins                 int
	lru                  *list.Element
	rangeID              string
	rangeStart, rangeEnd int64
}
type flight struct {
	done chan struct{}
	err  error
}

// Cache is a persistent, size-bounded cache. Its directory is exclusively
// owned by one process for the lifetime of the Cache.
type Cache struct {
	downloads           *downloadScheduler
	staging             *downloadScheduler
	stats               *iostats.Tracker
	lifetimeCtx         context.Context
	cancel              context.CancelFunc
	links               map[string]cachedLink
	linkFlights         map[string]chan struct{}
	mu                  sync.Mutex
	dir                 string
	max, used, reserved int64
	indexBudget         int64
	indexUsed           int64
	reservedIndex       int64
	entries             map[string]*cacheEntry
	lru                 [cacheClassCount]*list.List // one newest-first queue per retention class
	ranges              map[string][]*cacheRange
	rangeFlights        map[string][]*rangeFlight
	flights             map[string]*flight
	growing             map[string]*growingFlight
	lock                *os.File
	identityKey         [32]byte
	durable             bool
	syncFile            func(*os.File) error
	ephemeral           bool
	closed              bool
	statsReady          bool
	telemetry           cacheTelemetry
	remoteRecovery      remoteRecoveryCounters
	ghost               map[string]*list.Element
	ghostLRU            *list.List
}

// NewCache opens an exclusive cache directory with the requested byte limit.
func NewCache(dir string, maxBytes int64) (*Cache, error) {
	return NewCacheWithDownloadConfig(dir, maxBytes, DefaultDownloadConfig())
}

// NewCacheWithDownloadConfig opens a cache with a process-wide HTTP transfer
// budget. The budget covers in-flight response bytes, not memory or disk use.
func NewCacheWithDownloadConfig(dir string, maxBytes int64, download DownloadConfig) (*Cache, error) {
	var err error
	download, err = download.normalized()
	if err != nil {
		return nil, err
	}
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
	lifetimeCtx, cancel := context.WithCancel(context.Background())
	indexBudget := min(maxBytes/8, int64(64<<20))
	c := &Cache{dir: dir, max: maxBytes, indexBudget: indexBudget, entries: make(map[string]*cacheEntry), lru: newClassLRUs(), ranges: make(map[string][]*cacheRange), rangeFlights: make(map[string][]*rangeFlight), flights: make(map[string]*flight), growing: make(map[string]*growingFlight), lock: lock, durable: true, downloads: newDownloadScheduler(download), staging: newDownloadScheduler(download), stats: iostats.New(), lifetimeCtx: lifetimeCtx, cancel: cancel, ghost: make(map[string]*list.Element), ghostLRU: list.New()}
	if err = c.loadIdentityKey(); err != nil {
		c.Close()
		return nil, err
	}
	if err = c.load(); err != nil {
		c.Close()
		return nil, err
	}
	c.statsReady = true
	return c, nil
}

// DownloadStats returns a point-in-time snapshot of transfer scheduler usage.
func (c *Cache) DownloadStats() DownloadStats {
	d, s := c.downloads.snapshot(), c.staging.snapshot()
	d.StagingActiveBytes, d.StagingPeakBytes = s.ActiveBytes, s.PeakActiveBytes
	d.StagingWaitingForeground, d.StagingWaitingBackground = s.WaitingForeground, s.WaitingBackground
	return d
}

func (c *Cache) promotePriority(p *downloadPriority) {
	if p == nil || p.promoted.Swap(true) {
		return
	}
	c.downloads.promoteQueued(p)
	c.staging.promoteQueued(p)
}

// IOStats returns the fixed-size aggregate I/O tracker owned by this cache.
func (c *Cache) IOStats() *iostats.Tracker { return c.stats }

// StableDigest returns a keyed digest for cache identities. The key is unique
// to this private cache root and persists across mount processes. The input is
// never written to disk; the private HMAC key is stored in the cache root.
func (c *Cache) StableDigest(namespace, value string) string {
	h := hmac.New(sha256.New, c.identityKey[:])
	_, _ = io.WriteString(h, namespace)
	_, _ = h.Write([]byte{0})
	_, _ = io.WriteString(h, value)
	return hex.EncodeToString(h.Sum(nil))
}

// Open returns a pinned cache object without creating or filling it.
func (c *Cache) Open(key string) (*Handle, error) {
	return c.open(key, false)
}

// OpenArchiveIndex opens a persisted archive index without treating its
// metadata read as user-content heat. Legacy entries are classified under the
// bounded index share when possible; otherwise they remain ordinary entries.
func (c *Cache) OpenArchiveIndex(key string) (*Handle, error) {
	return c.open(key, true)
}

func (c *Cache) open(key string, archiveIndex bool) (*Handle, error) {
	id := cacheID(key)
	for {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return nil, ErrClosed
		}
		if active := c.flights[id]; active != nil {
			done := active.done
			c.mu.Unlock()
			<-done
			continue
		}
		e := c.entries[id]
		if e == nil {
			c.mu.Unlock()
			return nil, os.ErrNotExist
		}
		f, err := os.Open(e.path)
		if err != nil {
			if e.pins == 0 {
				c.removeEntryLocked(id, e)
			}
			c.mu.Unlock()
			return nil, err
		}
		e.pins++
		if archiveIndex {
			c.classifyIndexLocked(id, e)
		}
		c.touchLRULocked(id, e)
		h := &Handle{file: f, cache: c, key: id, size: e.size, promoteOnRead: !archiveIndex}
		c.mu.Unlock()
		return h, nil
	}
}

// Store publishes data under key using the cache's regular byte budget,
// private file mode and atomic fill semantics.
func (c *Cache) Store(ctx context.Context, key string, data []byte) error {
	return c.store(ctx, key, data, cacheProbation)
}

// StoreArchiveIndex stores a complete archive index under the bounded
// protected-index share. Entries beyond that share are retained as ordinary
// probationary cache data when the total cache budget permits.
func (c *Cache) StoreArchiveIndex(ctx context.Context, key string, data []byte) error {
	return c.store(ctx, key, data, cacheIndex)
}

func (c *Cache) store(ctx context.Context, key string, data []byte, class cacheClass) error {
	h, err := c.acquire(ctx, key, int64(len(data)), c.filename(key, class), class, func(ctx context.Context, w io.Writer) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := w.Write(data)
		return err
	})
	if err != nil {
		return err
	}
	return h.Close()
}

// Remove discards an unpinned cache object, such as an invalid persisted
// metadata record. Missing objects are ignored.
func (c *Cache) Remove(key string) error {
	id := cacheID(key)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrClosed
	}
	e := c.entries[id]
	if e == nil {
		return nil
	}
	if c.flights[id] != nil {
		return syscall.EBUSY
	}
	if e.pins != 0 {
		return syscall.EBUSY
	}
	if err := os.Remove(e.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	c.removeEntryLocked(id, e)
	return nil
}

func (c *Cache) loadIdentityKey() error {
	path := filepath.Join(c.dir, ".identity-hmac-key")
	read := func() error {
		f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			return err
		}
		if !st.Mode().IsRegular() || st.Size() != int64(len(c.identityKey)) {
			return errors.New("invalid cache identity key")
		}
		if _, err = io.ReadFull(f, c.identityKey[:]); err != nil {
			return err
		}
		return f.Chmod(0600)
	}
	if err := read(); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, err := rand.Read(c.identityKey[:]); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if os.IsExist(err) {
		return read()
	}
	if err != nil {
		return err
	}
	n := 0
	if n, err = f.Write(c.identityKey[:]); err == nil && n != len(c.identityKey) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = f.Chmod(0600)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

// Directory returns the active cache directory. Ephemeral caches use a private
// per-process subdirectory that is removed on close.
func (c *Cache) Directory() string { return c.dir }

func (c *Cache) filename(key string, class cacheClass) string {
	h := sha256.Sum256([]byte(key))
	return filepath.Join(c.dir, hex.EncodeToString(h[:])+"."+class.suffix()+".blob")
}

func (c *Cache) pathForKey(key string, class cacheClass) string {
	if start, end, identity, ok := parseRangeKey(key); ok {
		return filepath.Join(c.dir, rangeFilename(identity, start, end, cacheID(key), class))
	}
	return c.filename(key, class)
}

func parseObjectFilename(name string) (string, cacheClass, bool) {
	base := strings.TrimSuffix(name, ".blob")
	parts := strings.Split(base, ".")
	if len(parts) == 1 && len(parts[0]) == 64 {
		return parts[0], cacheProbation, true // pre-policy cache entry
	}
	if len(parts) != 2 || len(parts[0]) != 64 {
		return "", cacheProbation, false
	}
	class, ok := parseCacheClass(parts[1])
	return parts[0], class, ok
}

func (c *Cache) pathForEntry(id string, e *cacheEntry, class cacheClass) string {
	if e.rangeID != "" {
		return filepath.Join(c.dir, rangeFilename(e.rangeID, e.rangeStart, e.rangeEnd, id, class))
	}
	return filepath.Join(c.dir, id+"."+class.suffix()+".blob")
}

func newClassLRUs() [cacheClassCount]*list.List {
	var queues [cacheClassCount]*list.List
	for i := range queues {
		queues[i] = list.New()
	}
	return queues
}

func (c *Cache) lruLen() int {
	count := 0
	for _, queue := range c.lru {
		if queue != nil {
			count += queue.Len()
		}
	}
	return count
}
func cacheID(key string) string { h := sha256.Sum256([]byte(key)); return hex.EncodeToString(h[:]) }
func (c *Cache) load() error {
	items, err := os.ReadDir(c.dir)
	if err != nil {
		return err
	}
	type loadedEntry struct {
		id    string
		entry *cacheEntry
	}
	var loaded []loadedEntry
	for _, item := range items {
		name := item.Name()
		if strings.HasPrefix(name, ".fill-") || strings.HasPrefix(name, ".metadata-") {
			_ = os.Remove(filepath.Join(c.dir, name))
			continue
		}
		if item.IsDir() {
			continue
		}
		var id string
		class := cacheProbation
		var ranged *cacheRange
		if strings.HasPrefix(name, "extent-") {
			var err error
			ranged, id, class, err = parseRangeFilename(name)
			if err != nil {
				continue
			}
		} else if strings.HasSuffix(name, ".blob") {
			id, class, _ = parseObjectFilename(name)
			if id == "" {
				continue
			}
		} else {
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
		if ranged != nil && st.Size() != ranged.end-ranged.start {
			_ = os.Remove(p)
			continue
		}
		entry := &cacheEntry{path: p, size: st.Size(), class: class, used: st.ModTime(), lastTouch: st.ModTime()}
		if ranged != nil {
			entry.rangeID, entry.rangeStart, entry.rangeEnd = ranged.identity, ranged.start, ranged.end
		}
		loaded = append(loaded, loadedEntry{id: id, entry: entry})
		c.used += st.Size()
	}
	sort.Slice(loaded, func(i, j int) bool { return loaded[i].entry.used.Before(loaded[j].entry.used) })
	for _, item := range loaded {
		item.entry.lru = c.lru[item.entry.class].PushFront(item.id)
		c.entries[item.id] = item.entry
		if item.entry.class == cacheIndex {
			c.indexUsed += item.entry.size
		}
		if item.entry.rangeID != "" {
			c.addRangeLocked(&cacheRange{identity: item.entry.rangeID, start: item.entry.rangeStart, end: item.entry.rangeEnd, id: item.id})
		}
	}
	for node := c.lru[cacheIndex].Back(); node != nil && c.indexUsed > c.indexBudget; {
		previous := node.Prev()
		id := node.Value.(string)
		if e := c.entries[id]; e != nil && e.class == cacheIndex {
			c.setClassLocked(id, e, cacheProbation)
		}
		node = previous
	}
	// Old and oversized entries are removed using the same bounded eviction rule.
	return c.evictLocked(0)
}

func (c *Cache) evictLocked(need int64) error {
	for c.used+c.reserved+need > c.max {
		var victim *list.Element
		for class := cacheSpeculative; class <= cacheIndex && victim == nil; class++ {
			for node := c.lru[class].Back(); node != nil; node = node.Prev() {
				id := node.Value.(string)
				if e := c.entries[id]; e != nil && e.pins == 0 {
					victim = node
					break
				}
			}
		}
		if victim == nil {
			c.recordENOSPCLocked()
			return syscall.ENOSPC
		}
		id := victim.Value.(string)
		e := c.entries[id]
		if err := os.Remove(e.path); err != nil && !os.IsNotExist(err) {
			return err
		}
		c.recordCapacityEvictionLocked(id, e)
		c.removeEntryLocked(id, e)
	}
	return nil
}

func (c *Cache) touchLRULocked(id string, e *cacheEntry) {
	e.used = time.Now()
	if e.lru != nil {
		c.lru[e.class].MoveToFront(e.lru)
	} else {
		e.lru = c.lru[e.class].PushFront(id)
	}
	// Persist recency at most once per minute per file; ordinary cache hits
	// only update the in-memory LRU and do not issue a filesystem metadata op.
	if e.used.Sub(e.lastTouch) >= time.Minute {
		if os.Chtimes(e.path, e.used, e.used) == nil {
			e.lastTouch = e.used
		}
	}
}

func consumeClass(class *cacheClass, hasUse *bool, scanDir *int8, lastStart, lastEnd *int64, start, end int64) {
	if end <= start || *class == cacheIndex {
		return
	}
	if *class == cacheSpeculative {
		*class = cacheProbation
		*hasUse = true
		*lastStart, *lastEnd = start, end
		return
	}
	if *class != cacheProbation {
		return
	}
	if !*hasUse {
		*hasUse = true
		*lastStart, *lastEnd = start, end
		return
	}
	switch *scanDir {
	case 1:
		if start >= *lastEnd {
			*lastEnd = max(*lastEnd, end)
			return
		}
	case -1:
		if end <= *lastStart {
			*lastStart = min(*lastStart, start)
			return
		}
	default:
		if start >= *lastEnd {
			*scanDir = 1
			*lastEnd = max(*lastEnd, end)
			return
		}
		if end <= *lastStart {
			*scanDir = -1
			*lastStart = min(*lastStart, start)
			return
		}
	}
	*class = cacheHot
}

func (c *Cache) consumeEntryLocked(id string, e *cacheEntry, start, end int64) {
	class := e.class
	consumeClass(&class, &e.hasUse, &e.scanDir, &e.lastUseStart, &e.lastUseEnd, start, end)
	c.setClassLocked(id, e, class)
}

func (c *Cache) classifyIndexLocked(id string, e *cacheEntry) {
	if e.class == cacheIndex {
		return
	}
	if e.size <= c.indexBudget-c.indexUsed-c.reservedIndex {
		c.setClassLocked(id, e, cacheIndex)
	} else {
		c.setClassLocked(id, e, cacheProbation)
	}
}

func (c *Cache) setClassLocked(id string, e *cacheEntry, class cacheClass) {
	if e.class == class {
		return
	}
	newPath := c.pathForEntry(id, e, class)
	if e.path != newPath {
		if err := os.Rename(e.path, newPath); err != nil {
			return
		}
		e.path = newPath
	}
	if e.lru != nil {
		c.lru[e.class].Remove(e.lru)
		e.lru = c.lru[class].PushFront(id)
	}
	if e.class == cacheIndex {
		c.indexUsed -= e.size
	}
	e.class = class
	if class == cacheIndex {
		c.indexUsed += e.size
	}
}

func (c *Cache) removeEntryLocked(id string, e *cacheEntry) {
	if e.rangeID != "" {
		c.removeRangeLocked(e.rangeID, id)
	}
	if e.lru != nil {
		c.lru[e.class].Remove(e.lru)
		e.lru = nil
	}
	delete(c.entries, id)
	c.used -= e.size
	if e.class == cacheIndex {
		c.indexUsed -= e.size
	}
}

// Acquire returns a pinned handle for key, filling and atomically publishing it
// on a miss. Concurrent misses for the same key share one fill operation.
func (c *Cache) Acquire(ctx context.Context, key string, size int64, fill func(context.Context, io.Writer) error) (*Handle, error) {
	class := foregroundClass(workqueue.IsBackground(ctx))
	return c.acquire(ctx, key, size, c.filename(key, class), class, fill)
}

func (c *Cache) acquire(ctx context.Context, key string, size int64, targetPath string, class cacheClass, fill func(context.Context, io.Writer) error) (*Handle, error) {
	return c.acquireWithProgress(ctx, key, size, targetPath, class, nil, fill)
}

func (c *Cache) acquireWithProgress(ctx context.Context, key string, size int64, targetPath string, class cacheClass, progress *rangeProgress, fill func(context.Context, io.Writer) error) (*Handle, error) {
	id := cacheID(key)
	if size < 0 || size > c.max {
		c.mu.Lock()
		if size >= 0 {
			c.consumeCacheGhostLocked(id)
		}
		c.recordENOSPCLocked()
		c.mu.Unlock()
		return nil, syscall.ENOSPC
	}
	for {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return nil, ErrClosed
		}
		if f := c.flights[id]; f != nil {
			c.recordExistingFillWaitLocked()
			done := f.done
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-done:
				if f.err != nil {
					if errors.Is(f.err, context.Canceled) || errors.Is(f.err, context.DeadlineExceeded) {
						if err := ctx.Err(); err != nil {
							return nil, err
						}
						continue
					}
					return nil, f.err
				}
				continue
			}
		}
		if e := c.entries[id]; e != nil && e.size == size {
			f, err := os.Open(e.path)
			if err == nil {
				e.pins++
				if class == cacheIndex {
					c.classifyIndexLocked(id, e)
				}
				c.touchLRULocked(id, e)
				c.mu.Unlock()
				return &Handle{file: f, cache: c, key: id, size: size, promoteOnRead: class != cacheIndex && class != cacheSpeculative}, nil
			}
			if e.pins != 0 {
				c.mu.Unlock()
				return nil, err
			}
			c.removeEntryLocked(id, e)
		} else if e != nil {
			if e.pins != 0 {
				c.recordENOSPCLocked()
				c.mu.Unlock()
				return nil, syscall.ENOSPC
			}
			_ = os.Remove(e.path)
			c.removeEntryLocked(id, e)
		}
		if growing := c.growing[id]; growing != nil {
			c.recordExistingFillWaitLocked()
			done := growing.done
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-done:
				continue
			}
		}
		if f := c.flights[id]; f != nil {
			c.recordExistingFillWaitLocked()
			done := f.done
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-done:
				if f.err != nil {
					// A fill belongs to its initiating caller. If that caller
					// cancels, live waiters should get a chance to own a retry.
					if errors.Is(f.err, context.Canceled) || errors.Is(f.err, context.DeadlineExceeded) {
						if err := ctx.Err(); err != nil {
							return nil, err
						}
						continue
					}
					return nil, f.err
				}
				continue
			}
		}
		if class == cacheIndex && size > c.indexBudget-c.indexUsed-c.reservedIndex {
			class = cacheProbation
			targetPath = c.pathForKey(key, class)
		}
		c.consumeCacheGhostLocked(id)
		if err := c.evictLocked(size); err != nil {
			c.mu.Unlock()
			return nil, err
		}
		f := &flight{done: make(chan struct{})}
		c.flights[id] = f
		c.reserved += size
		if class == cacheIndex {
			c.reservedIndex += size
		}
		c.mu.Unlock()
		f.err = c.fill(ctx, targetPath, key, size, class, progress, fill)
		c.mu.Lock()
		c.reserved -= size
		if class == cacheIndex {
			c.reservedIndex -= size
		}
		var acquired *Handle
		if f.err == nil && !c.closed {
			st, err := os.Stat(targetPath)
			if err != nil || st.Size() != size {
				f.err = fmt.Errorf("cache fill published invalid file")
			} else {
				state := rangeProgressState{class: class}
				if progress != nil {
					if published, ok := progress.publishState(id); ok {
						state = published
					}
				}
				finalPath := targetPath
				if state.class != class {
					finalPath = c.pathForKey(key, state.class)
					if err := os.Rename(targetPath, finalPath); err != nil {
						f.err = err
					}
				}
				if f.err == nil {
					entry := &cacheEntry{path: finalPath, size: size, class: state.class, hasUse: state.hasUse, scanDir: state.scanDir, lastUseStart: state.lastUseStart, lastUseEnd: state.lastUseEnd, used: time.Now(), lastTouch: time.Now()}
					entry.lru = c.lru[entry.class].PushFront(id)
					c.entries[id] = entry
					if start, end, identity, ok := parseRangeKey(key); ok {
						entry.rangeID, entry.rangeStart, entry.rangeEnd = identity, start, end
						c.addRangeLocked(&cacheRange{identity: identity, start: start, end: end, id: id})
					}
					c.used += size
					if entry.class == cacheIndex {
						c.indexUsed += size
					}
					file, openErr := os.Open(finalPath)
					if openErr != nil {
						f.err = openErr
					} else {
						entry.pins++
						acquired = &Handle{file: file, cache: c, key: id, size: size, promoteOnRead: entry.class != cacheIndex && entry.class != cacheSpeculative}
					}
					if f.err != nil {
						c.removeEntryLocked(id, entry)
						_ = os.Remove(finalPath)
					}
				} else {
					_ = os.Remove(targetPath)
				}
			}
		} else if f.err == nil {
			_ = os.Remove(targetPath)
		}
		delete(c.flights, id)
		if c.statsReady {
			if acquired != nil {
				c.telemetry.fillSuccesses++
			} else {
				c.telemetry.fillFailures++
				if errors.Is(f.err, context.Canceled) {
					c.telemetry.fillCancelled++
				} else {
					c.telemetry.fillErrors++
				}
			}
		}
		close(f.done)
		c.mu.Unlock()
		if f.err != nil {
			return nil, f.err
		}
		if acquired != nil {
			return acquired, nil
		}
	}
}

func (c *Cache) fill(ctx context.Context, targetPath, key string, size int64, class cacheClass, progress *rangeProgress, fill func(context.Context, io.Writer) error) error {
	if fill == nil {
		return errors.New("cache fill callback is nil")
	}
	id := cacheID(key)
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
	var output io.Writer = tmp
	if progress != nil {
		progressStart := int64(0)
		if start, _, _, ok := parseRangeKey(key); ok {
			progressStart = start
		}
		reader, openErr := os.Open(temp)
		if openErr != nil {
			_ = tmp.Close()
			return openErr
		}
		part := progress.addFile(progressStart, size, id, class, reader)
		output = &progressWriter{w: tmp, p: progress, part: part}
	}
	w := &limitedWriter{w: output, left: size}
	if err = fill(ctx, w); err != nil {
		tmp.Close()
		return err
	}
	if w.written != size {
		tmp.Close()
		return fmt.Errorf("cache fill size mismatch: got %d, want %d", w.written, size)
	}
	publishStarted := time.Now()
	defer func() { c.stats.ObserveCachePublication(time.Since(publishStarted)) }()
	if c.durable {
		syncFile := c.syncFile
		if syncFile == nil {
			syncFile = func(f *os.File) error { return f.Sync() }
		}
		if err = syncFile(tmp); err != nil {
			tmp.Close()
			return err
		}
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(temp, targetPath); err != nil {
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
	c.cancel()
	var pending []<-chan struct{}
	for _, f := range c.flights {
		pending = append(pending, f.done)
	}
	for _, group := range c.rangeFlights {
		for _, f := range group {
			f.cancel()
			pending = append(pending, f.done)
		}
	}
	for _, f := range c.growing {
		f.cancel()
		pending = append(pending, f.done)
	}
	c.mu.Unlock()
	for _, done := range pending {
		<-done
	}
	c.downloads.waitIdle()
	c.staging.waitIdle()
	e := syscall.Flock(int(c.lock.Fd()), syscall.LOCK_UN)
	ce := c.lock.Close()
	if c.ephemeral {
		if removeErr := os.RemoveAll(c.dir); ce == nil {
			ce = removeErr
		}
	}
	if e != nil {
		return e
	}
	return ce
}

// Handle pins an immutable cache file until Close.
type Handle struct {
	mu            sync.Mutex
	file          *os.File
	cache         *Cache
	key           string
	size          int64
	closed        bool
	promoteOnRead bool
}

func (h *Handle) Size() int64 { return h.size }
func (h *Handle) ReadAt(p []byte, off int64) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return 0, os.ErrClosed
	}
	n, err := h.file.ReadAt(p, off)
	if n > 0 && h.promoteOnRead {
		c := h.cache
		c.mu.Lock()
		if entry := c.entries[h.key]; entry != nil {
			c.consumeEntryLocked(h.key, entry, off, off+int64(n))
		}
		c.mu.Unlock()
	}
	return n, err
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

// existing pins a cached data block without starting a fill on a miss.
func (c *Cache) existing(key string, size int64) (*Handle, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrClosed
	}
	id := cacheID(key)
	e := c.entries[id]
	if e == nil || e.size != size {
		return nil, nil
	}
	f, err := os.Open(e.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	e.pins++
	c.touchLRULocked(id, e)
	return &Handle{file: f, cache: c, key: id, size: size}, nil
}
