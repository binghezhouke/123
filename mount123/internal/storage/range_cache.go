package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/binghezhouke/123/mount123/internal/workqueue"
)

type cacheRange struct {
	identity   string
	start, end int64
	id         string
}

type rangeFlight struct {
	start, end int64
	done       chan struct{}
	err        error
	ctx        context.Context
	cancel     context.CancelFunc
	priority   *downloadPriority
	progress   *rangeProgress
	refs       int
	finished   bool
}

type byteRange struct{ start, end int64 }

type pinnedRangePart struct {
	handle     *Handle
	start, end int64
	base       int64
}

func rangeKey(identity string, start, end int64) string {
	return fmt.Sprintf("cache-range:%s:%d:%d", identity, start, end)
}

func parseRangeKey(key string) (start, end int64, identity string, ok bool) {
	if !strings.HasPrefix(key, "cache-range:") {
		return
	}
	parts := strings.Split(strings.TrimPrefix(key, "cache-range:"), ":")
	if len(parts) != 3 {
		return
	}
	identity = parts[0]
	start, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, 0, "", false
	}
	end, err = strconv.ParseInt(parts[2], 10, 64)
	if err != nil || start < 0 || end <= start {
		return 0, 0, "", false
	}
	return start, end, identity, true
}

func rangeFilename(identity string, start, end int64, id string, class cacheClass) string {
	return fmt.Sprintf("extent-%s-%d-%d-%s-%s.blob", identity, start, end, id, class.suffix())
}

func parseRangeFilename(name string) (*cacheRange, string, cacheClass, error) {
	parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(name, "extent-"), ".blob"), "-")
	if (len(parts) != 4 && len(parts) != 5) || len(parts[0]) != 64 || len(parts[3]) != 64 {
		return nil, "", cacheProbation, errors.New("invalid extent filename")
	}
	start, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || start < 0 {
		return nil, "", cacheProbation, errors.New("invalid extent start")
	}
	end, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || end <= start {
		return nil, "", cacheProbation, errors.New("invalid extent end")
	}
	if cacheID(rangeKey(parts[0], start, end)) != parts[3] {
		return nil, "", cacheProbation, errors.New("extent filename key mismatch")
	}
	class := cacheProbation
	if len(parts) == 5 {
		var ok bool
		class, ok = parseCacheClass(parts[4])
		if !ok {
			return nil, "", cacheProbation, errors.New("invalid extent class")
		}
	}
	return &cacheRange{identity: parts[0], start: start, end: end, id: parts[3]}, parts[3], class, nil
}

// AcquireRange stores one immutable byte extent in one cache blob.
func (c *Cache) AcquireRange(ctx context.Context, identity string, start, end int64, fill func(context.Context, io.Writer) error) (*Handle, error) {
	return c.AcquireRangeWithProgress(ctx, identity, start, end, cacheProbation, nil, fill)
}

func (c *Cache) AcquireRangeWithProgress(ctx context.Context, identity string, start, end int64, class cacheClass, progress *rangeProgress, fill func(context.Context, io.Writer) error) (*Handle, error) {
	if len(identity) != 64 || start < 0 || end <= start {
		return nil, errors.New("invalid cache range")
	}
	key := rangeKey(identity, start, end)
	path := filepath.Join(c.dir, rangeFilename(identity, start, end, cacheID(key), class))
	return c.acquireWithProgress(ctx, key, end-start, path, class, progress, fill)
}

func (c *Cache) addRangeLocked(r *cacheRange) {
	group := c.ranges[r.identity]
	idx := sort.Search(len(group), func(i int) bool { return group[i].start >= r.start })
	group = append(group, nil)
	copy(group[idx+1:], group[idx:])
	group[idx] = r
	c.ranges[r.identity] = group
}

func (c *Cache) removeRangeLocked(identity, id string) {
	group := c.ranges[identity]
	for i, r := range group {
		if r.id == id {
			group = append(group[:i], group[i+1:]...)
			break
		}
	}
	if len(group) == 0 {
		delete(c.ranges, identity)
	} else {
		c.ranges[identity] = group
	}
}

func (c *Cache) missingRanges(identity string, start, end int64) []byteRange {
	c.mu.Lock()
	defer c.mu.Unlock()
	var missing []byteRange
	cursor := start
	for _, r := range c.ranges[identity] {
		if r.end <= cursor || r.start >= end || c.entries[r.id] == nil {
			continue
		}
		if r.start > cursor {
			missing = append(missing, byteRange{cursor, min(r.start, end)})
		}
		if r.end > cursor {
			cursor = r.end
		}
		if cursor >= end {
			break
		}
	}
	if cursor < end {
		missing = append(missing, byteRange{cursor, end})
	}
	return missing
}

// beginRangeFlight joins overlapping fills. The flight owns its context so a
// canceled initiating reader cannot stop work still needed by another reader.
func (c *Cache) beginRangeFlight(ctx context.Context, identity string, start, end int64) (*rangeFlight, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, false, ErrClosed
	}
	flightEnd := end
	for _, active := range c.rangeFlights[identity] {
		if active.ctx.Err() != nil {
			continue
		}
		if start >= active.start && start < active.end {
			active.refs++
			c.recordExistingFillWaitLocked()
			c.mu.Unlock()
			if !workqueue.IsBackground(ctx) {
				c.promotePriority(active.priority)
			}
			return active, false, nil
		}
		if active.start > start && active.start < flightEnd {
			flightEnd = active.start
		}
	}
	if flightEnd <= start {
		c.mu.Unlock()
		return nil, false, errors.New("range flight made no progress")
	}
	flightCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	priority := &downloadPriority{}
	if !workqueue.IsBackground(ctx) {
		priority.promoted.Store(true)
	}
	f := &rangeFlight{start: start, end: flightEnd, done: make(chan struct{}), ctx: flightCtx, cancel: cancel, priority: priority, progress: newRangeProgress(), refs: 1}
	c.rangeFlights[identity] = append(c.rangeFlights[identity], f)
	c.mu.Unlock()
	return f, true, nil
}

func (c *Cache) releaseRangeFlight(f *rangeFlight) {
	closeProgress := false
	c.mu.Lock()
	if f.refs > 0 {
		f.refs--
	}
	if f.refs == 0 {
		if f.finished {
			closeProgress = true
		} else if !f.progress.retained() {
			f.cancel()
		}
	}
	c.mu.Unlock()
	if closeProgress {
		f.progress.close()
	}
}

func (c *Cache) finishRangeFlight(identity string, f *rangeFlight, err error) {
	c.mu.Lock()
	f.err = err
	f.finished = true
	f.progress.finish(err)
	group := c.rangeFlights[identity]
	for i, active := range group {
		if active == f {
			group = append(group[:i], group[i+1:]...)
			break
		}
	}
	if len(group) == 0 {
		delete(c.rangeFlights, identity)
	} else {
		c.rangeFlights[identity] = group
	}
	close(f.done)
	f.cancel()
	closeProgress := f.refs == 0
	c.mu.Unlock()
	if closeProgress {
		f.progress.close()
	}
}

// pinRange atomically pins every extent needed for [start,end). A coverage
// hole returns ok=false after releasing all partial pins.
func (c *Cache) pinRange(identity string, start, end int64, foregroundArg ...bool) ([]pinnedRangePart, bool, error) {
	foreground := len(foregroundArg) > 0 && foregroundArg[0]
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, false, ErrClosed
	}
	cursor := start
	var parts []pinnedRangePart
	for _, r := range c.ranges[identity] {
		if r.end <= cursor || r.start > cursor {
			continue
		}
		e := c.entries[r.id]
		if e == nil {
			continue
		}
		f, err := os.Open(e.path)
		if err != nil {
			if e.pins == 0 {
				c.removeEntryLocked(r.id, e)
			}
			c.mu.Unlock()
			closeRangeParts(parts)
			if os.IsNotExist(err) {
				return nil, false, nil
			}
			return nil, false, err
		}
		e.pins++
		c.touchLRULocked(r.id, e)
		partEnd := min(end, r.end)
		if foreground {
			c.consumeEntryLocked(r.id, e, cursor, partEnd)
		}
		parts = append(parts, pinnedRangePart{handle: &Handle{file: f, cache: c, key: r.id, size: e.size}, start: cursor, end: partEnd, base: r.start})
		cursor = partEnd
		if cursor >= end {
			break
		}
	}
	c.mu.Unlock()
	if cursor < end {
		closeRangeParts(parts)
		return nil, false, nil
	}
	return parts, true, nil
}

// pinAvailableRange pins every currently cached extent intersecting [start,end),
// even when the range has holes. Callers can copy these bytes into their own
// destination before filling the holes, so later LRU eviction cannot force a
// refetch of bytes already available in the cache.
func (c *Cache) pinAvailableRange(identity string, start, end int64, foreground bool) ([]pinnedRangePart, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	var parts []pinnedRangePart
	for _, r := range c.ranges[identity] {
		if r.end <= start || r.start >= end {
			continue
		}
		e := c.entries[r.id]
		if e == nil {
			continue
		}
		f, err := os.Open(e.path)
		if err != nil {
			if e.pins == 0 {
				c.removeEntryLocked(r.id, e)
			}
			c.mu.Unlock()
			closeRangeParts(parts)
			if os.IsNotExist(err) {
				return nil, nil
			}
			return nil, err
		}
		e.pins++
		c.touchLRULocked(r.id, e)
		if foreground {
			c.consumeEntryLocked(r.id, e, max(start, r.start), min(end, r.end))
		}
		parts = append(parts, pinnedRangePart{handle: &Handle{file: f, cache: c, key: r.id, size: e.size}, start: max(start, r.start), end: min(end, r.end), base: r.start})
	}
	c.mu.Unlock()
	return parts, nil
}

func closeRangeParts(parts []pinnedRangePart) {
	for _, part := range parts {
		_ = part.handle.Close()
	}
}

func readPinnedRange(parts []pinnedRangePart, dst []byte, start int64) error {
	for _, part := range parts {
		from, to := max(start, part.start), min(start+int64(len(dst)), part.end)
		if from >= to {
			continue
		}
		dstOff, handleOff := from-start, from-part.base
		got, err := part.handle.ReadAt(dst[dstOff:dstOff+(to-from)], handleOff)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if int64(got) != to-from {
			return io.ErrUnexpectedEOF
		}
	}
	return nil
}
