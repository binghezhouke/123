package storage

import (
	"container/list"

	"github.com/binghezhouke/123/mount123/internal/iostats"
)

type cacheTelemetry struct {
	capacityEvictions     uint64
	capacityEvictionBytes int64
	enospc                uint64
	fillSuccesses         uint64
	fillFailures          uint64
	fillCancelled         uint64
	fillErrors            uint64
	existingFillWaits     uint64
	classes               [cacheClassCount]cacheClassTelemetry
	indexKinds            [indexKindCount]cacheIndexKindTelemetry
	rangeReads            [2]cacheRangeTelemetry
	refaults              iostats.CacheRefaultSummary
}

type cacheClassTelemetry struct {
	capacityEvictions     uint64
	capacityEvictionBytes int64
}

// cacheIndexKindTelemetry counts how often a protected object of one family
// left the index share. Demotions are the documented response to an exhausted
// budget; capacity evictions are the last resort when nothing else remains.
type cacheIndexKindTelemetry struct {
	demotions             uint64
	demotionBytes         int64
	capacityEvictions     uint64
	capacityEvictionBytes int64
}

type cacheRangeTelemetry struct {
	readRequests   uint64
	fullHits       uint64
	requestedBytes int64
	hitBytes       int64
	missBytes      int64
}

type cacheGhost struct {
	id   string
	size int64
}

const maxCacheGhostIDs = 4096

var cacheClassNames = [cacheClassCount]string{"speculative", "probation", "hot", "index"}

// Stats returns a point-in-time inventory and process-lifetime counters. It
// walks resident entries only when called; the read path updates counters
// without scanning the entry map.
func (c *Cache) Stats() iostats.CacheSummary {
	if c == nil {
		return iostats.CacheSummary{Status: "unknown"}
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	summary := iostats.CacheSummary{
		Status:                "measured",
		CapacityBytes:         c.max,
		UsedBytes:             c.used,
		ReservedBytes:         c.reserved,
		Entries:               uint64(len(c.entries)),
		IndexBudgetBytes:      c.indexBudget,
		IndexUsedBytes:        c.indexUsed,
		CapacityEvictions:     c.telemetry.capacityEvictions,
		CapacityEvictionBytes: c.telemetry.capacityEvictionBytes,
		ENOSPC:                c.telemetry.enospc,
		FillSuccesses:         c.telemetry.fillSuccesses,
		FillFailures:          c.telemetry.fillFailures,
		FillOutcomeVersion:    1,
		FillCancelled:         c.telemetry.fillCancelled,
		FillErrors:            c.telemetry.fillErrors,
		ExistingFillWaits:     c.telemetry.existingFillWaits,
		Refaults:              c.telemetry.refaults,
		Classes:               make([]iostats.CacheClassSummary, cacheClassCount),
		IndexKinds:            make([]iostats.CacheIndexKindSummary, indexKindCount),
	}
	for class := cacheClass(0); class <= cacheIndex; class++ {
		counter := c.telemetry.classes[class]
		summary.Classes[class] = iostats.CacheClassSummary{
			Class:                 cacheClassNames[class],
			CapacityEvictions:     counter.capacityEvictions,
			CapacityEvictionBytes: counter.capacityEvictionBytes,
		}
	}
	for _, entry := range c.entries {
		class := int(entry.class)
		if class < 0 || class >= len(summary.Classes) {
			continue
		}
		classSummary := &summary.Classes[class]
		classSummary.Entries++
		classSummary.Bytes += entry.size
		if entry.pins > 0 {
			classSummary.PinnedBytes += entry.size
			summary.PinnedBytes += entry.size
		}
		if entry.class == cacheIndex && entry.kind < indexKindCount {
			kindSummary := &summary.IndexKinds[entry.kind]
			kindSummary.Entries++
			kindSummary.Bytes += entry.size
			if entry.pins > 0 {
				kindSummary.PinnedBytes += entry.size
			}
		}
	}
	for kind := indexKind(0); kind < indexKindCount; kind++ {
		counter := c.telemetry.indexKinds[kind]
		summary.IndexKinds[kind].Kind = kind.name()
		summary.IndexKinds[kind].Demotions = counter.demotions
		summary.IndexKinds[kind].DemotionBytes = counter.demotionBytes
		summary.IndexKinds[kind].CapacityEvictions = counter.capacityEvictions
		summary.IndexKinds[kind].CapacityEvictionBytes = counter.capacityEvictionBytes
	}
	summary.Foreground = c.telemetry.rangeReads[0].summary()
	summary.Background = c.telemetry.rangeReads[1].summary()
	return summary
}

func (c *Cache) observeRangeRead(identity string, start, end int64, background, applicationRead bool) int64 {
	if end <= start {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	covered := c.coveredRangeLocked(identity, start, end)
	if applicationRead && c.statsReady {
		idx := 0
		if background {
			idx = 1
		}
		read := &c.telemetry.rangeReads[idx]
		requested := end - start
		read.readRequests++
		read.requestedBytes += requested
		read.hitBytes += covered
		read.missBytes += requested - covered
		if covered == requested {
			read.fullHits++
		}
	}
	return covered
}

func (c *Cache) coveredRangeLocked(identity string, start, end int64) int64 {
	cursor := start
	var covered int64
	for _, r := range c.ranges[identity] {
		if r.start >= end {
			break
		}
		if r.end <= start || c.entries[r.id] == nil {
			continue
		}
		segmentStart := max(cursor, r.start, start)
		segmentEnd := min(end, r.end)
		if segmentEnd > segmentStart {
			covered += segmentEnd - segmentStart
			cursor = segmentEnd
		}
		if cursor >= end {
			break
		}
	}
	return covered
}

func (t cacheRangeTelemetry) summary() iostats.CacheRangeSummary {
	return iostats.CacheRangeSummary{
		Status:         "measured",
		ReadRequests:   t.readRequests,
		FullHits:       t.fullHits,
		RequestedBytes: t.requestedBytes,
		HitBytes:       t.hitBytes,
		MissBytes:      t.missBytes,
	}
}

func (c *Cache) recordCapacityEvictionLocked(id string, e *cacheEntry) {
	if !c.statsReady {
		return
	}
	class := int(e.class)
	c.telemetry.capacityEvictions++
	c.telemetry.capacityEvictionBytes += e.size
	if class >= 0 && class < len(c.telemetry.classes) {
		counter := &c.telemetry.classes[class]
		counter.capacityEvictions++
		counter.capacityEvictionBytes += e.size
	}
	if e.class == cacheIndex && e.kind < indexKindCount {
		counter := &c.telemetry.indexKinds[e.kind]
		counter.capacityEvictions++
		counter.capacityEvictionBytes += e.size
	}
	c.rememberCacheGhostLocked(id, e.size)
}

// recordIndexDemotionLocked counts a protected object that left the index share
// because the share was already taken. The object stays cached as ordinary
// data, so this is a retention downgrade rather than a loss.
func (c *Cache) recordIndexDemotionLocked(e *cacheEntry) {
	if !c.statsReady || e.kind >= indexKindCount {
		return
	}
	c.countIndexDemotionLocked(e)
}

// recordRestoredIndexDemotionLocked counts a demotion performed while opening a
// persisted cache. It deliberately ignores statsReady so an operator can see
// that a smaller index budget pushed previously protected objects back to
// ordinary data.
func (c *Cache) recordRestoredIndexDemotionLocked(e *cacheEntry) {
	if e.kind >= indexKindCount {
		return
	}
	c.countIndexDemotionLocked(e)
}

func (c *Cache) countIndexDemotionLocked(e *cacheEntry) {
	counter := &c.telemetry.indexKinds[e.kind]
	counter.demotions++
	counter.demotionBytes += e.size
}

func (c *Cache) rememberCacheGhostLocked(id string, size int64) {
	if c.ghostLRU == nil {
		c.ghostLRU = list.New()
	}
	if c.ghost == nil {
		c.ghost = make(map[string]*list.Element)
	}
	if element := c.ghost[id]; element != nil {
		element.Value = cacheGhost{id: id, size: size}
		c.ghostLRU.MoveToFront(element)
		return
	}
	c.ghost[id] = c.ghostLRU.PushFront(cacheGhost{id: id, size: size})
	if c.ghostLRU.Len() > maxCacheGhostIDs {
		oldest := c.ghostLRU.Back()
		ghost := oldest.Value.(cacheGhost)
		delete(c.ghost, ghost.id)
		c.ghostLRU.Remove(oldest)
	}
}

// consumeCacheGhostLocked counts one refault per first post-eviction miss.
// Failed admissions also count because the cache was requested after eviction,
// even though it could not publish another copy.
func (c *Cache) consumeCacheGhostLocked(id string) {
	if !c.statsReady {
		return
	}
	element := c.ghost[id]
	if element == nil {
		return
	}
	ghost := element.Value.(cacheGhost)
	delete(c.ghost, id)
	c.ghostLRU.Remove(element)
	c.telemetry.refaults.Count++
	c.telemetry.refaults.Bytes += ghost.size
}

func (c *Cache) recordENOSPC() {
	c.mu.Lock()
	c.recordENOSPCLocked()
	c.mu.Unlock()
}

func (c *Cache) recordENOSPCLocked() {
	if c.statsReady {
		c.telemetry.enospc++
	}
}

func (c *Cache) recordExistingFillWaitLocked() {
	if c.statsReady {
		c.telemetry.existingFillWaits++
	}
}
