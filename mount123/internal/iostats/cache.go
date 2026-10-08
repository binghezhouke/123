package iostats

// CacheSummary is a point-in-time cache inventory plus process-lifetime
// operation counters. Capacity and resident-byte values are measured directly
// from the cache index; counters reset when the cache is reopened.
type CacheSummary struct {
	Status                string              `json:"status"`
	CapacityBytes         int64               `json:"capacity_bytes"`
	UsedBytes             int64               `json:"used_bytes"`
	ReservedBytes         int64               `json:"reserved_bytes"`
	Entries               uint64              `json:"entries"`
	PinnedBytes           int64               `json:"pinned_bytes"`
	IndexBudgetBytes      int64               `json:"index_budget_bytes"`
	IndexUsedBytes        int64               `json:"index_used_bytes"`
	Classes               []CacheClassSummary `json:"classes"`
	CapacityEvictions     uint64              `json:"capacity_evictions"`
	CapacityEvictionBytes int64               `json:"capacity_eviction_bytes"`
	ENOSPC                uint64              `json:"enospc"`
	FillSuccesses         uint64              `json:"fill_successes"`
	FillFailures          uint64              `json:"fill_failures"`
	// FillOutcomeVersion is 1 when FillCancelled and FillErrors are populated.
	// Older snapshots omit this field and must treat the new counters as unknown.
	FillOutcomeVersion uint8               `json:"fill_outcome_version,omitempty"`
	FillCancelled      uint64              `json:"fill_cancelled,omitempty"`
	FillErrors         uint64              `json:"fill_errors,omitempty"`
	ExistingFillWaits  uint64              `json:"existing_fill_waits"`
	Foreground         CacheRangeSummary   `json:"foreground"`
	Background         CacheRangeSummary   `json:"background"`
	Refaults           CacheRefaultSummary `json:"refaults"`
}

// CacheClassSummary describes current residency and capacity evictions for one
// retention class. PinnedBytes counts each pinned entry once, regardless of
// how many handles refer to it.
type CacheClassSummary struct {
	Class                 string `json:"class"`
	Entries               uint64 `json:"entries"`
	Bytes                 int64  `json:"bytes"`
	PinnedBytes           int64  `json:"pinned_bytes"`
	CapacityEvictions     uint64 `json:"capacity_evictions"`
	CapacityEvictionBytes int64  `json:"capacity_eviction_bytes"`
}

// CacheRangeSummary counts valid bytes requested through Remote.ReadAtContext.
// Reads through metadata/range scanning APIs and direct cache handles are
// outside this boundary. A hit is range coverage published before ReadAt began;
// partial reads contribute hit bytes but not FullHits.
type CacheRangeSummary struct {
	Status         string `json:"status"`
	ReadRequests   uint64 `json:"read_requests"`
	FullHits       uint64 `json:"full_hits"`
	RequestedBytes int64  `json:"requested_bytes"`
	HitBytes       int64  `json:"hit_bytes"`
	MissBytes      int64  `json:"miss_bytes"`
}

// CacheRefaultSummary counts the first miss after a capacity eviction for an
// ID still present in the bounded in-memory ghost list. Bytes are the size of
// the evicted object, not bytes re-downloaded.
type CacheRefaultSummary struct {
	Count uint64 `json:"count"`
	Bytes int64  `json:"bytes"`
}
