// Package iostats provides bounded, low-cardinality I/O statistics for a
// running mount. It deliberately does not retain paths or request identities.
package iostats

import (
	"math/bits"
	"sync/atomic"
	"time"
)

const histogramBuckets = 64

// Tracker aggregates metrics without retaining individual samples. Duration
// histograms use fixed logarithmic buckets; byte counters are monotonic.
type Tracker struct {
	foregroundRead duration
	transferQueue  duration
	httpBodyTTFB   duration
	httpTransfer   duration
	cachePublish   duration
	downloaded     counter
	cacheHit       counter
}

// DurationSummary is unknown until at least one observation has been made.
// Percentiles are upper bounds of fixed logarithmic buckets, in nanoseconds.
type DurationSummary struct {
	Status   string  `json:"status"`
	Samples  *uint64 `json:"samples"`
	P50Nanos *uint64 `json:"p50_nanos"`
	P95Nanos *uint64 `json:"p95_nanos"`
	MaxNanos *uint64 `json:"max_nanos"`
}

// CounterSummary distinguishes an observed zero from a metric with no hook.
type CounterSummary struct {
	Status string  `json:"status"`
	Events *uint64 `json:"events"`
	Bytes  *uint64 `json:"bytes"`
}

// Snapshot contains process-lifetime aggregate measurements only.
type Snapshot struct {
	ForegroundReadLatency DurationSummary `json:"foreground_read_latency"`
	TransferQueueLatency  DurationSummary `json:"transfer_queue_latency"`
	HTTPBodyTTFB          DurationSummary `json:"http_body_ttfb"`
	HTTPTransferLatency   DurationSummary `json:"http_transfer_latency"`
	CachePublication      DurationSummary `json:"cache_publication_latency"`
	DownloadedBytes       CounterSummary  `json:"downloaded_bytes"`
	CacheHitBytes         CounterSummary  `json:"cache_hit_bytes"`
	DirectoryLookup       CounterSummary  `json:"directory_lookup"`
}

type duration struct {
	count   atomic.Uint64
	max     atomic.Uint64
	buckets [histogramBuckets]atomic.Uint64
}

type counter struct {
	events atomic.Uint64
	bytes  atomic.Uint64
}

// New creates a tracker. Its storage is fixed-size regardless of request
// count or the number of files opened during the mount lifetime.
func New() *Tracker { return &Tracker{} }

func (t *Tracker) ObserveForegroundRead(d time.Duration) { t.foregroundRead.observe(d) }
func (t *Tracker) ObserveTransferQueue(d time.Duration)  { t.transferQueue.observe(d) }
func (t *Tracker) ObserveHTTPBodyTTFB(d time.Duration)   { t.httpBodyTTFB.observe(d) }
func (t *Tracker) ObserveHTTPTransfer(d time.Duration)   { t.httpTransfer.observe(d) }
func (t *Tracker) ObserveCachePublication(d time.Duration) {
	t.cachePublish.observe(d)
}

// AddDownloadedBytes records bytes actually received into a range-cache fill.
// Calling it with zero marks the metric as measured for an empty/failed body.
func (t *Tracker) AddDownloadedBytes(n uint64) {
	t.downloaded.events.Add(1)
	t.downloaded.bytes.Add(n)
}

// AddCacheHitBytes records bytes served from extents that were already
// complete before the current read began. Calling with zero records a measured
// miss, rather than leaving the field indistinguishable from an unavailable one.
func (t *Tracker) AddCacheHitBytes(n uint64) {
	t.cacheHit.events.Add(1)
	t.cacheHit.bytes.Add(n)
}

// Snapshot returns a coherent-enough point-in-time view of atomic aggregates.
// Individual counters can advance during the snapshot.
func (t *Tracker) Snapshot() Snapshot {
	return Snapshot{
		ForegroundReadLatency: t.foregroundRead.snapshot(),
		TransferQueueLatency:  t.transferQueue.snapshot(),
		HTTPBodyTTFB:          t.httpBodyTTFB.snapshot(),
		HTTPTransferLatency:   t.httpTransfer.snapshot(),
		CachePublication:      t.cachePublish.snapshot(),
		DownloadedBytes:       t.downloaded.snapshot(),
		CacheHitBytes:         t.cacheHit.snapshot(),
		DirectoryLookup:       CounterSummary{Status: "unknown"},
	}
}

func (d *duration) observe(value time.Duration) {
	nanos := uint64(max(value, 0))
	d.count.Add(1)
	idx := bucketIndex(nanos)
	d.buckets[idx].Add(1)
	for old := d.max.Load(); nanos > old; old = d.max.Load() {
		if d.max.CompareAndSwap(old, nanos) {
			break
		}
	}
}

func (d *duration) snapshot() DurationSummary {
	count := d.count.Load()
	if count == 0 {
		return DurationSummary{Status: "unknown"}
	}
	return DurationSummary{
		Status:   "measured",
		Samples:  ptr(count),
		P50Nanos: ptr(d.quantile(count, 0.50)),
		P95Nanos: ptr(d.quantile(count, 0.95)),
		MaxNanos: ptr(d.max.Load()),
	}
}

func (d *duration) quantile(count uint64, q float64) uint64 {
	// ceil(q*count), expressed without retaining any per-sample state.
	target := uint64(float64(count)*q + 0.999999999)
	if target == 0 {
		target = 1
	}
	var seen uint64
	for i := range d.buckets {
		seen += d.buckets[i].Load()
		if seen >= target {
			if i == 0 {
				return 0
			}
			return (uint64(1) << i) - 1
		}
	}
	return d.max.Load()
}

func (c *counter) snapshot() CounterSummary {
	events := c.events.Load()
	if events == 0 {
		return CounterSummary{Status: "unknown"}
	}
	return CounterSummary{Status: "measured", Events: ptr(events), Bytes: ptr(c.bytes.Load())}
}

func ptr[T any](value T) *T { return &value }

func bucketIndex(nanos uint64) int {
	if nanos == 0 {
		return 0
	}
	return min(bits.Len64(nanos), histogramBuckets-1)
}
