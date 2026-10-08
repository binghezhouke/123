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
	startedAt           time.Time
	foregroundRead      duration
	foregroundReadTime  duration
	transferQueue       duration
	httpBodyTTFB        duration
	httpTransfer        duration
	cachePublish        duration
	readAheadWait       duration
	downloaded          counter
	foregroundReadBytes counter
	cacheHit            counter
	readAheadScheduled  counter
	readAheadCompleted  counter
	readAheadConsumed   counter
	readAheadWasted     counter
}

// DurationSummary is unknown until at least one observation has been made.
// Percentiles are upper bounds of fixed logarithmic buckets, in nanoseconds.
type DurationSummary struct {
	Status     string  `json:"status"`
	Samples    *uint64 `json:"samples"`
	TotalNanos *uint64 `json:"total_nanos"`
	P50Nanos   *uint64 `json:"p50_nanos"`
	P95Nanos   *uint64 `json:"p95_nanos"`
	MaxNanos   *uint64 `json:"max_nanos"`
}

// CounterSummary distinguishes an observed zero from a metric with no hook.
type CounterSummary struct {
	Status string  `json:"status"`
	Events *uint64 `json:"events"`
	Bytes  *uint64 `json:"bytes"`
}

// Snapshot contains process-lifetime aggregate measurements only.
type Snapshot struct {
	StartedAt             time.Time        `json:"started_at"`
	CollectedAt           time.Time        `json:"collected_at"`
	UptimeSeconds         float64          `json:"uptime_seconds"`
	Cache                 CacheSummary     `json:"cache"`
	DirectoryCache        DirectorySummary `json:"directory_cache"`
	RemoteRecovery        RecoverySummary  `json:"remote_recovery"`
	Configuration         RuntimeConfig    `json:"configuration"`
	ForegroundReadLatency DurationSummary  `json:"foreground_read_latency"`
	ForegroundReadTime    DurationSummary  `json:"foreground_read_success_time"`
	TransferQueueLatency  DurationSummary  `json:"transfer_queue_latency"`
	HTTPBodyTTFB          DurationSummary  `json:"http_body_ttfb"`
	HTTPTransferLatency   DurationSummary  `json:"http_transfer_latency"`
	CachePublication      DurationSummary  `json:"cache_publication_latency"`
	ReadAheadWait         DurationSummary  `json:"read_ahead_foreground_wait"`
	DownloadedBytes       CounterSummary   `json:"downloaded_bytes"`
	ForegroundReadBytes   CounterSummary   `json:"foreground_read_bytes"`
	CacheHitBytes         CounterSummary   `json:"cache_hit_bytes"`
	ReadAheadScheduled    CounterSummary   `json:"read_ahead_scheduled_bytes"`
	ReadAheadCompleted    CounterSummary   `json:"read_ahead_completed_bytes"`
	ReadAheadConsumed     CounterSummary   `json:"read_ahead_consumed_bytes"`
	ReadAheadWasted       CounterSummary   `json:"read_ahead_wasted_bytes"`
	DownloadScheduler     SchedulerSummary `json:"download_scheduler"`
	DirectoryLookup       CounterSummary   `json:"directory_lookup"`
}

// SchedulerSummary is a point-in-time aggregate from the shared cache.
type SchedulerSummary struct {
	Status                   string `json:"status"`
	MaximumRequests          int    `json:"maximum_requests"`
	MaximumInFlightBytes     int64  `json:"maximum_in_flight_bytes"`
	ActiveRequests           int    `json:"active_requests"`
	ActiveBytes              int64  `json:"active_bytes"`
	AvailableBackgroundBytes int64  `json:"available_background_bytes"`
	WaitingForeground        int    `json:"waiting_foreground"`
	WaitingBackground        int    `json:"waiting_background"`
	StagingActiveBytes       int64  `json:"staging_active_bytes"`
	StagingPeakBytes         int64  `json:"staging_peak_bytes"`
	StagingWaitingForeground int    `json:"staging_waiting_foreground"`
	StagingWaitingBackground int    `json:"staging_waiting_background"`
}

type duration struct {
	count   atomic.Uint64
	max     atomic.Uint64
	total   atomic.Uint64
	buckets [histogramBuckets]atomic.Uint64
}

type counter struct {
	events atomic.Uint64
	bytes  atomic.Uint64
}

// New creates a tracker. Its storage is fixed-size regardless of request
// count or the number of files opened during the mount lifetime.
func New() *Tracker { return &Tracker{startedAt: time.Now()} }

func (t *Tracker) ObserveForegroundRead(d time.Duration) { t.foregroundRead.observe(d) }
func (t *Tracker) ObserveTransferQueue(d time.Duration)  { t.transferQueue.observe(d) }
func (t *Tracker) ObserveHTTPBodyTTFB(d time.Duration)   { t.httpBodyTTFB.observe(d) }
func (t *Tracker) ObserveHTTPTransfer(d time.Duration)   { t.httpTransfer.observe(d) }
func (t *Tracker) ObserveCachePublication(d time.Duration) {
	t.cachePublish.observe(d)
}
func (t *Tracker) ObserveReadAheadWait(d time.Duration) { t.readAheadWait.observe(d) }
func (t *Tracker) AddReadAheadScheduled(n uint64) {
	t.readAheadScheduled.events.Add(1)
	t.readAheadScheduled.bytes.Add(n)
}
func (t *Tracker) AddReadAheadCompleted(n uint64) {
	t.readAheadCompleted.events.Add(1)
	t.readAheadCompleted.bytes.Add(n)
}
func (t *Tracker) AddReadAheadConsumed(n uint64) {
	t.readAheadConsumed.events.Add(1)
	t.readAheadConsumed.bytes.Add(n)
}
func (t *Tracker) AddReadAheadWasted(n uint64) {
	t.readAheadWasted.events.Add(1)
	t.readAheadWasted.bytes.Add(n)
}

// SetDownloadScheduler attaches cache-wide capacity to a mount snapshot while
// keeping this package independent of storage.
func (s *Snapshot) SetDownloadScheduler(maximumRequests int, maximumInFlightBytes int64, activeRequests int, activeBytes, availableBackgroundBytes int64, waitingForeground, waitingBackground int) {
	s.DownloadScheduler = SchedulerSummary{Status: "measured", MaximumRequests: maximumRequests, MaximumInFlightBytes: maximumInFlightBytes, ActiveRequests: activeRequests, ActiveBytes: activeBytes, AvailableBackgroundBytes: availableBackgroundBytes, WaitingForeground: waitingForeground, WaitingBackground: waitingBackground}
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

// RecordForegroundRead records bytes and elapsed time returned by a successful
// filesystem read, allowing application-visible throughput to be derived.
func (t *Tracker) RecordForegroundRead(n uint64, elapsed time.Duration) {
	t.foregroundReadBytes.events.Add(1)
	t.foregroundReadBytes.bytes.Add(n)
	t.foregroundReadTime.observe(elapsed)
}

// Snapshot returns a coherent-enough point-in-time view of atomic aggregates.
// Individual counters can advance during the snapshot.
func (t *Tracker) Snapshot() Snapshot {
	now := time.Now()
	return Snapshot{
		StartedAt:             t.startedAt.UTC(),
		CollectedAt:           now.UTC(),
		UptimeSeconds:         max(0, now.Sub(t.startedAt).Seconds()),
		Cache:                 CacheSummary{Status: "unknown"},
		DirectoryCache:        DirectorySummary{Status: "unknown"},
		RemoteRecovery:        RecoverySummary{Status: "unknown"},
		ForegroundReadLatency: t.foregroundRead.snapshot(),
		ForegroundReadTime:    t.foregroundReadTime.snapshot(),
		TransferQueueLatency:  t.transferQueue.snapshot(),
		HTTPBodyTTFB:          t.httpBodyTTFB.snapshot(),
		HTTPTransferLatency:   t.httpTransfer.snapshot(),
		CachePublication:      t.cachePublish.snapshot(),
		ReadAheadWait:         t.readAheadWait.snapshot(),
		DownloadedBytes:       t.downloaded.snapshot(),
		ForegroundReadBytes:   t.foregroundReadBytes.snapshot(),
		CacheHitBytes:         t.cacheHit.snapshot(),
		ReadAheadScheduled:    t.readAheadScheduled.snapshot(),
		ReadAheadCompleted:    t.readAheadCompleted.snapshot(),
		ReadAheadConsumed:     t.readAheadConsumed.snapshot(),
		ReadAheadWasted:       t.readAheadWasted.snapshot(),
		DownloadScheduler:     SchedulerSummary{Status: "unknown"},
		DirectoryLookup:       CounterSummary{Status: "unknown"},
	}
}

func (d *duration) observe(value time.Duration) {
	nanos := uint64(max(value, 0))
	d.count.Add(1)
	d.total.Add(nanos)
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
		Status:     "measured",
		Samples:    ptr(count),
		TotalNanos: ptr(d.total.Load()),
		P50Nanos:   ptr(d.quantile(count, 0.50)),
		P95Nanos:   ptr(d.quantile(count, 0.95)),
		MaxNanos:   ptr(d.max.Load()),
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
