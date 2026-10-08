package storage

import (
	"fmt"
	"testing"
)

func BenchmarkEvictFindsRareSpeculativeEntryAmongHotCache(b *testing.B) {
	const entries = 20_000
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		c := newEvictionBenchmarkCache(entries)
		b.StartTimer()
		c.mu.Lock()
		if err := c.evictLocked(1); err != nil {
			b.Fatal(err)
		}
		c.mu.Unlock()
	}
}

func newEvictionBenchmarkCache(count int) *Cache {
	c := &Cache{max: int64(count), used: int64(count), entries: make(map[string]*cacheEntry, count), lru: newClassLRUs()}
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("%064x", i+1)
		class := cacheHot
		if i == 0 {
			class = cacheSpeculative
		}
		e := &cacheEntry{path: "/nonexistent/cache-entry", size: 1, class: class}
		e.lru = c.lru[class].PushFront(id)
		c.entries[id] = e
	}
	return c
}
