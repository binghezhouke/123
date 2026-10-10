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
	stages              [stageCount]duration
	operations          [operationCount]operation
}

// Operation identifies a FUSE metadata operation. The set is deliberately
// fixed so a busy mount cannot grow the diagnostic output with path names.
type Operation uint8

const (
	OperationReaddir Operation = iota
	OperationLookup
	OperationGetattr
	OperationOpen
	operationCount
)

type operation struct {
	requests         atomic.Uint64
	foreground       atomic.Uint64
	background       atomic.Uint64
	activeForeground atomic.Int64
	activeBackground atomic.Int64
}

// OperationSummary is a bounded point-in-time view of metadata pressure.
// Requests that wait for capacity are reported by QueueSummary, because
// metadata handlers share one build queue rather than queueing per operation.
type OperationSummary struct {
	Status             string `json:"status"`
	Requests           uint64 `json:"requests"`
	ForegroundRequests uint64 `json:"foreground_requests"`
	BackgroundRequests uint64 `json:"background_requests"`
	ActiveForeground   int    `json:"active_foreground"`
	ActiveBackground   int    `json:"active_background"`
}

// OperationTotals is the cumulative metadata request count by scheduling
// priority. Samplers that watch for bulk scans read one aggregate instead of
// walking every operation family.
type OperationTotals struct {
	Foreground uint64
	Background uint64
}

// OperationTotals reports cumulative metadata requests by priority.
func (t *Tracker) OperationTotals() OperationTotals {
	var totals OperationTotals
	for i := range t.operations {
		totals.Foreground += t.operations[i].foreground.Load()
		totals.Background += t.operations[i].background.Load()
	}
	return totals
}

// ScanSummary reports whether sustained metadata traffic has put the mount
// into background load shedding. The rates are observations, not limits
// enforced on callers: no request is rejected while shedding.
type ScanSummary struct {
	Status                    string    `json:"status"`
	Shedding                  bool      `json:"shedding"`
	Reason                    string    `json:"reason,omitempty"`
	MetadataRequestsPerSecond float64   `json:"metadata_requests_per_second"`
	EnterRequestsPerSecond    float64   `json:"enter_requests_per_second"`
	ExitRequestsPerSecond     float64   `json:"exit_requests_per_second"`
	Since                     time.Time `json:"since,omitempty"`
}

// QueueSummary is the shared archive-index build queue, which is where
// metadata requests queue while they wait for capacity.
type QueueSummary struct {
	Status           string `json:"status"`
	Limit            int    `json:"limit"`
	Active           int    `json:"active"`
	BackgroundActive int    `json:"background_active"`
	Waiting          int    `json:"waiting"`
	Requests         uint64 `json:"requests"`
}

// Stage is a fixed-cardinality operation family. Stage durations are
// inclusive and can overlap with HTTP, cache, or other stage timings.
type Stage uint8

const (
	StageDirectoryLookup Stage = iota
	StageSourcePrepare
	StageURLResolve
	StageSourceProbe
	StageBuildQueue
	StageArchiveIndex
	StageDecompression
	StageFileOpen
	stageCount
)

// StageSummary is deliberately a fixed shape so metrics cannot grow with
// paths, URLs, or operation names.
type StageSummary struct {
	DirectoryLookup DurationSummary `json:"directory_lookup"`
	SourcePrepare   DurationSummary `json:"source_prepare"`
	URLResolve      DurationSummary `json:"url_resolve"`
	SourceProbe     DurationSummary `json:"source_probe"`
	BuildQueue      DurationSummary `json:"build_queue"`
	ArchiveIndex    DurationSummary `json:"archive_index"`
	Decompression   DurationSummary `json:"decompression"`
	FileOpen        DurationSummary `json:"file_open"`
}

// ImagePrefetchSummary is filled by mountfs when the feature is enabled and
// its bounded tracker is available. Status is unknown when disabled or when
// the tracker is unavailable; measured zero counts mean it is enabled but no
// work has started yet.
type ImagePrefetchSummary struct {
	Status             string `json:"status"`
	Planned            uint64 `json:"planned"`
	Started            uint64 `json:"started"`
	Reused             uint64 `json:"reused"`
	Completed          uint64 `json:"completed"`
	Cancelled          uint64 `json:"cancelled"`
	Failed             uint64 `json:"failed"`
	ForegroundReady    uint64 `json:"foreground_ready"`
	ForegroundInFlight uint64 `json:"foreground_in_flight"`
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
	StartedAt             time.Time            `json:"started_at"`
	CollectedAt           time.Time            `json:"collected_at"`
	UptimeSeconds         float64              `json:"uptime_seconds"`
	Cache                 CacheSummary         `json:"cache"`
	DirectoryCache        DirectorySummary     `json:"directory_cache"`
	RemoteRecovery        RecoverySummary      `json:"remote_recovery"`
	Configuration         RuntimeConfig        `json:"configuration"`
	ForegroundReadLatency DurationSummary      `json:"foreground_read_latency"`
	ForegroundReadTime    DurationSummary      `json:"foreground_read_success_time"`
	TransferQueueLatency  DurationSummary      `json:"transfer_queue_latency"`
	HTTPBodyTTFB          DurationSummary      `json:"http_body_ttfb"`
	HTTPTransferLatency   DurationSummary      `json:"http_transfer_latency"`
	CachePublication      DurationSummary      `json:"cache_publication_latency"`
	ReadAheadWait         DurationSummary      `json:"read_ahead_foreground_wait"`
	DownloadedBytes       CounterSummary       `json:"downloaded_bytes"`
	ForegroundReadBytes   CounterSummary       `json:"foreground_read_bytes"`
	CacheHitBytes         CounterSummary       `json:"cache_hit_bytes"`
	ReadAheadScheduled    CounterSummary       `json:"read_ahead_scheduled_bytes"`
	ReadAheadCompleted    CounterSummary       `json:"read_ahead_completed_bytes"`
	ReadAheadConsumed     CounterSummary       `json:"read_ahead_consumed_bytes"`
	ReadAheadWasted       CounterSummary       `json:"read_ahead_wasted_bytes"`
	DownloadScheduler     SchedulerSummary     `json:"download_scheduler"`
	DirectoryLookup       CounterSummary       `json:"directory_lookup"`
	Stages                StageSummary         `json:"stages"`
	StageSchemaVersion    uint8                `json:"stage_schema_version,omitempty"`
	ImagePrefetch         ImagePrefetchSummary `json:"image_prefetch"`
	Readdir               OperationSummary     `json:"readdir"`
	Lookup                OperationSummary     `json:"lookup"`
	Getattr               OperationSummary     `json:"getattr"`
	Open                  OperationSummary     `json:"open"`
	IndexQueue            QueueSummary         `json:"index_queue"`
	Scan                  ScanSummary          `json:"scan"`
}

func (s *Snapshot) SetIndexQueue(limit, active, background, waiting int, requests uint64) {
	s.IndexQueue = QueueSummary{Status: "measured", Limit: limit, Active: active, BackgroundActive: background, Waiting: waiting, Requests: requests}
}

// SetScan attaches the scan-shedding state observed by the mount.
func (s *Snapshot) SetScan(summary ScanSummary) {
	s.Scan = summary
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
	CompletedRequests        uint64 `json:"completed_requests"`
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

// ObserveStage adds one inclusive stage-duration observation. Invalid stage
// values are ignored to keep metrics cardinality fixed.
func (t *Tracker) ObserveStage(stage Stage, d time.Duration) {
	if stage < stageCount {
		t.stages[stage].observe(d)
	}
}

// BeginOperation records a FUSE operation and returns a completion function
// that releases its active gauge. Context priority is passed explicitly by
// mountfs so this package remains independent of workqueue.
func (t *Tracker) BeginOperation(op Operation, background bool) func() {
	if op >= operationCount {
		return func() {}
	}
	o := &t.operations[op]
	o.requests.Add(1)
	if background {
		o.background.Add(1)
		o.activeBackground.Add(1)
	} else {
		o.foreground.Add(1)
		o.activeForeground.Add(1)
	}
	var released atomic.Bool
	return func() {
		if released.Swap(true) {
			return
		}
		if background {
			o.activeBackground.Add(-1)
		} else {
			o.activeForeground.Add(-1)
		}
	}
}

func (t *Tracker) ObserveTransferQueue(d time.Duration) { t.transferQueue.observe(d) }
func (t *Tracker) ObserveHTTPBodyTTFB(d time.Duration)  { t.httpBodyTTFB.observe(d) }
func (t *Tracker) ObserveHTTPTransfer(d time.Duration)  { t.httpTransfer.observe(d) }
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
func (s *Snapshot) SetDownloadScheduler(maximumRequests int, maximumInFlightBytes int64, activeRequests int, activeBytes, availableBackgroundBytes int64, waitingForeground, waitingBackground int, completedRequests uint64) {
	s.DownloadScheduler = SchedulerSummary{Status: "measured", MaximumRequests: maximumRequests, MaximumInFlightBytes: maximumInFlightBytes, ActiveRequests: activeRequests, ActiveBytes: activeBytes, AvailableBackgroundBytes: availableBackgroundBytes, WaitingForeground: waitingForeground, WaitingBackground: waitingBackground, CompletedRequests: completedRequests}
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
		Stages: StageSummary{
			DirectoryLookup: t.stages[StageDirectoryLookup].snapshot(),
			SourcePrepare:   t.stages[StageSourcePrepare].snapshot(),
			URLResolve:      t.stages[StageURLResolve].snapshot(),
			SourceProbe:     t.stages[StageSourceProbe].snapshot(),
			BuildQueue:      t.stages[StageBuildQueue].snapshot(),
			ArchiveIndex:    t.stages[StageArchiveIndex].snapshot(),
			Decompression:   t.stages[StageDecompression].snapshot(),
			FileOpen:        t.stages[StageFileOpen].snapshot(),
		},
		StageSchemaVersion: 1,
		ImagePrefetch:      ImagePrefetchSummary{Status: "unknown"},
		Readdir:            t.operations[OperationReaddir].snapshot(),
		Lookup:             t.operations[OperationLookup].snapshot(),
		Getattr:            t.operations[OperationGetattr].snapshot(),
		Open:               t.operations[OperationOpen].snapshot(),
	}
}

func (o *operation) snapshot() OperationSummary {
	requests := o.requests.Load()
	s := OperationSummary{Status: "idle", Requests: requests, ForegroundRequests: o.foreground.Load(), BackgroundRequests: o.background.Load(), ActiveForeground: maxInt64(o.activeForeground.Load()), ActiveBackground: maxInt64(o.activeBackground.Load())}
	if requests > 0 {
		s.Status = "measured"
	}
	return s
}

func maxInt64(v int64) int {
	if v < 0 {
		return 0
	}
	if v > int64(^uint(0)>>1) {
		return int(^uint(0) >> 1)
	}
	return int(v)
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
