package iostats

// RuntimeConfig accompanies measurements so parameter experiments can be
// compared without retaining command lines, mount paths or credentials.
type RuntimeConfig struct {
	DirectoryTTLSeconds float64 `json:"directory_ttl_seconds"`
	SourceTTLSeconds    float64 `json:"source_ttl_seconds"`
	MetadataBytes       int64   `json:"metadata_bytes"`
	ReadAheadMaxBytes   int64   `json:"read_ahead_max_bytes"`
	PrefetchFiles       int     `json:"prefetch_files"`
	PrefetchWorkers     int     `json:"prefetch_workers"`
	PrefetchBytes       int64   `json:"prefetch_bytes"`
}

// TuningReport compares two samples from one process. Ratios with no observed
// denominator stay null. Advice is deliberately observational, never an
// automatic change to capacity or concurrency.
type TuningReport struct {
	Status                      string              `json:"status"`
	DirectoryCache              DirectorySummary    `json:"directory_cache"`
	RemoteRecovery              RecoverySummary     `json:"remote_recovery"`
	WindowSeconds               float64             `json:"window_seconds"`
	CacheUtilization            *float64            `json:"cache_utilization"`
	Foreground                  CacheRangeSummary   `json:"foreground"`
	Background                  CacheRangeSummary   `json:"background"`
	ForegroundByteHitRatio      *float64            `json:"foreground_byte_hit_ratio"`
	BackgroundByteHitRatio      *float64            `json:"background_byte_hit_ratio"`
	CapacityEvictions           uint64              `json:"capacity_evictions"`
	CapacityEvictionBytes       int64               `json:"capacity_eviction_bytes"`
	Refaults                    CacheRefaultSummary `json:"refaults"`
	ENOSPC                      uint64              `json:"enospc"`
	FillFailures                uint64              `json:"fill_failures"`
	FillOutcomeStatus           string              `json:"fill_outcome_status"`
	FillCancelled               *uint64             `json:"fill_cancelled,omitempty"`
	FillErrors                  *uint64             `json:"fill_errors,omitempty"`
	ExistingFillWaits           uint64              `json:"existing_fill_waits"`
	DownloadedBytes             uint64              `json:"downloaded_bytes"`
	DownloadBytesPerSecond      float64             `json:"download_bytes_per_second"`
	MeanTransferQueueMillis     *float64            `json:"mean_transfer_queue_millis"`
	MeanForegroundReadMillis    *float64            `json:"mean_foreground_read_millis"`
	Stages                      StageDeltaSummary   `json:"stages"`
	ReadAheadConsumedBytes      uint64              `json:"read_ahead_consumed_bytes"`
	ReadAheadUnusedOnCloseBytes uint64              `json:"read_ahead_unused_on_close_bytes"`
	LifetimeReadAheadUseRatio   *float64            `json:"lifetime_read_ahead_use_ratio"`
	Recommendations             []TuningAdvice      `json:"recommendations"`
}

// DurationDelta reports interval samples and mean only. Cumulative percentile
// buckets cannot be subtracted into valid interval percentiles.
type DurationDelta struct {
	Status     string  `json:"status"`
	Samples    *uint64 `json:"samples,omitempty"`
	TotalNanos *uint64 `json:"total_nanos,omitempty"`
	MeanNanos  *uint64 `json:"mean_nanos,omitempty"`
}

type StageDeltaSummary struct {
	DirectoryLookup DurationDelta `json:"directory_lookup"`
	SourcePrepare   DurationDelta `json:"source_prepare"`
	URLResolve      DurationDelta `json:"url_resolve"`
	SourceProbe     DurationDelta `json:"source_probe"`
	BuildQueue      DurationDelta `json:"build_queue"`
	ArchiveIndex    DurationDelta `json:"archive_index"`
	Decompression   DurationDelta `json:"decompression"`
	FileOpen        DurationDelta `json:"file_open"`
}

type TuningAdvice struct {
	Parameter string `json:"parameter"`
	Action    string `json:"action"`
	Reason    string `json:"reason"`
}

// Analyze uses deltas, not differences of cumulative percentiles. It refuses
// to compare different processes, backwards time, or decreasing counters.
// Refaults are a bounded ghost-list signal, not a complete working-set model.
func Analyze(previous, current Snapshot) TuningReport {
	r := TuningReport{Status: "unavailable", Recommendations: []TuningAdvice{}}
	if previous.StartedAt.IsZero() || !previous.StartedAt.Equal(current.StartedAt) || !current.CollectedAt.After(previous.CollectedAt) {
		r.Status = "reset"
		return r
	}
	if previous.Cache.Status != "measured" || current.Cache.Status != "measured" {
		return r
	}
	r.WindowSeconds = current.CollectedAt.Sub(previous.CollectedAt).Seconds()
	p, c := previous.Cache, current.Cache
	valid := true
	r.DirectoryCache = directoryDelta(previous.DirectoryCache, current.DirectoryCache, &valid)
	r.RemoteRecovery = recoveryDelta(previous.RemoteRecovery, current.RemoteRecovery, &valid)
	r.Foreground = rangeDelta(p.Foreground, c.Foreground, &valid)
	r.Background = rangeDelta(p.Background, c.Background, &valid)
	r.CapacityEvictions = deltaUint(p.CapacityEvictions, c.CapacityEvictions, &valid)
	r.CapacityEvictionBytes = deltaInt(p.CapacityEvictionBytes, c.CapacityEvictionBytes, &valid)
	r.Refaults = CacheRefaultSummary{Count: deltaUint(p.Refaults.Count, c.Refaults.Count, &valid), Bytes: deltaInt(p.Refaults.Bytes, c.Refaults.Bytes, &valid)}
	r.ENOSPC = deltaUint(p.ENOSPC, c.ENOSPC, &valid)
	r.FillFailures = deltaUint(p.FillFailures, c.FillFailures, &valid)
	r.FillOutcomeStatus = "unknown"
	if p.FillOutcomeVersion == 1 && c.FillOutcomeVersion == 1 {
		r.FillOutcomeStatus = "measured"
		r.FillCancelled = ptr(deltaUint(p.FillCancelled, c.FillCancelled, &valid))
		r.FillErrors = ptr(deltaUint(p.FillErrors, c.FillErrors, &valid))
	}
	r.ExistingFillWaits = deltaUint(p.ExistingFillWaits, c.ExistingFillWaits, &valid)
	r.DownloadedBytes = deltaUint(counterBytes(previous.DownloadedBytes), counterBytes(current.DownloadedBytes), &valid)
	r.ReadAheadConsumedBytes = deltaUint(counterBytes(previous.ReadAheadConsumed), counterBytes(current.ReadAheadConsumed), &valid)
	r.ReadAheadUnusedOnCloseBytes = deltaUint(counterBytes(previous.ReadAheadWasted), counterBytes(current.ReadAheadWasted), &valid)
	r.MeanTransferQueueMillis = meanMillis(previous.TransferQueueLatency, current.TransferQueueLatency, &valid)
	r.MeanForegroundReadMillis = meanMillis(previous.ForegroundReadLatency, current.ForegroundReadLatency, &valid)
	if previous.StageSchemaVersion == 1 && current.StageSchemaVersion == 1 {
		r.Stages = StageDeltaSummary{
			DirectoryLookup: stageDelta(previous.Stages.DirectoryLookup, current.Stages.DirectoryLookup, &valid),
			SourcePrepare:   stageDelta(previous.Stages.SourcePrepare, current.Stages.SourcePrepare, &valid),
			URLResolve:      stageDelta(previous.Stages.URLResolve, current.Stages.URLResolve, &valid),
			SourceProbe:     stageDelta(previous.Stages.SourceProbe, current.Stages.SourceProbe, &valid),
			BuildQueue:      stageDelta(previous.Stages.BuildQueue, current.Stages.BuildQueue, &valid),
			ArchiveIndex:    stageDelta(previous.Stages.ArchiveIndex, current.Stages.ArchiveIndex, &valid),
			Decompression:   stageDelta(previous.Stages.Decompression, current.Stages.Decompression, &valid),
			FileOpen:        stageDelta(previous.Stages.FileOpen, current.Stages.FileOpen, &valid),
		}
	} else {
		r.Stages = unknownStageDeltaSummary()
	}
	if !valid {
		r.Status = "reset"
		return r
	}
	r.DownloadBytesPerSecond = float64(r.DownloadedBytes) / r.WindowSeconds
	r.CacheUtilization = ratio(float64(c.UsedBytes+c.ReservedBytes), float64(c.CapacityBytes))
	r.ForegroundByteHitRatio = ratio(float64(r.Foreground.HitBytes), float64(r.Foreground.RequestedBytes))
	r.BackgroundByteHitRatio = ratio(float64(r.Background.HitBytes), float64(r.Background.RequestedBytes))
	consumed, unused := counterBytes(current.ReadAheadConsumed), counterBytes(current.ReadAheadWasted)
	r.LifetimeReadAheadUseRatio = ratio(float64(consumed), float64(consumed)+float64(unused))
	r.Status = "measured"
	if r.Foreground.ReadRequests+r.Background.ReadRequests == 0 && r.DownloadedBytes == 0 && r.CapacityEvictions == 0 && r.ENOSPC == 0 && r.FillFailures == 0 && r.DirectoryCache.ListCalls == 0 && r.DirectoryCache.DiskRestores == 0 && r.DirectoryCache.StaleServed == 0 && r.RemoteRecovery.Attempts == 0 && !stageDeltaHasSamples(r.Stages) {
		r.Status = "idle"
	} else if r.Foreground.ReadRequests < 32 || r.Foreground.RequestedBytes < 8<<20 || r.WindowSeconds < 10 {
		r.Status = "insufficient_samples"
	}
	if r.ENOSPC > 0 {
		r.Recommendations = append(r.Recommendations, TuningAdvice{"cache-gib", "inspect_capacity", "Cache admission ran out of space. Check oversized members and pinned bytes as well as the capacity limit."})
	}
	if r.Status != "measured" {
		return r
	}
	if r.Refaults.Count >= 4 && r.CapacityEvictionBytes >= 8<<20 && r.CacheUtilization != nil && *r.CacheUtilization >= 0.9 {
		r.Recommendations = append(r.Recommendations, TuningAdvice{"cache-gib", "compare_larger_cache", "Recently evicted objects were requested again while the cache was full. Compare a larger limit using the same workload."})
	}
	// Consumption can occur before completion; unused completed ranges are
	// accounted on handle close. Use lifetime resolved accounting, with several
	// closes, rather than interpreting a quiet interval as wasted prefetch.
	if current.ReadAheadWasted.Events != nil && *current.ReadAheadWasted.Events >= 4 && float64(consumed)+float64(unused) >= 64<<20 && r.ReadAheadUnusedOnCloseBytes > 0 && r.LifetimeReadAheadUseRatio != nil && *r.LifetimeReadAheadUseRatio < 0.35 {
		r.Recommendations = append(r.Recommendations, TuningAdvice{"read-ahead-mib", "compare_smaller_window", "Several handles closed with mostly unused sequential read-ahead. Compare a smaller window; cached bytes may still help later readers."})
	}
	if r.MeanTransferQueueMillis != nil && *r.MeanTransferQueueMillis >= 5 && current.DownloadScheduler.WaitingForeground > 0 {
		r.Recommendations = append(r.Recommendations, TuningAdvice{"download-requests/download-bytes-mib", "inspect_scheduler", "Foreground transfers are queued. Check active request and byte limits, plus staging pressure, before increasing concurrency."})
	}
	return r
}

func rangeDelta(p, c CacheRangeSummary, valid *bool) CacheRangeSummary {
	r := CacheRangeSummary{Status: c.Status}
	r.ReadRequests = deltaUint(p.ReadRequests, c.ReadRequests, valid)
	r.FullHits = deltaUint(p.FullHits, c.FullHits, valid)
	r.RequestedBytes = deltaInt(p.RequestedBytes, c.RequestedBytes, valid)
	r.HitBytes = deltaInt(p.HitBytes, c.HitBytes, valid)
	r.MissBytes = deltaInt(p.MissBytes, c.MissBytes, valid)
	return r
}

func deltaUint(p, c uint64, valid *bool) uint64 {
	if c < p {
		*valid = false
		return 0
	}
	return c - p
}
func deltaInt(p, c int64, valid *bool) int64 {
	if p < 0 || c < p {
		*valid = false
		return 0
	}
	return c - p
}
func counterBytes(c CounterSummary) uint64 {
	if c.Bytes == nil {
		return 0
	}
	return *c.Bytes
}
func ratio(n, d float64) *float64 {
	if d <= 0 {
		return nil
	}
	r := n / d
	return &r
}
func meanMillis(p, c DurationSummary, valid *bool) *float64 {
	var pc, pn, cc, cn uint64
	if p.Samples != nil {
		pc = *p.Samples
	}
	if p.TotalNanos != nil {
		pn = *p.TotalNanos
	}
	if c.Samples != nil {
		cc = *c.Samples
	}
	if c.TotalNanos != nil {
		cn = *c.TotalNanos
	}
	count, nanos := deltaUint(pc, cc, valid), deltaUint(pn, cn, valid)
	return ratio(float64(nanos)/1e6, float64(count))
}

func stageDelta(previous, current DurationSummary, valid *bool) DurationDelta {
	var beforeCount, beforeTotal, afterCount, afterTotal uint64
	if previous.Samples != nil {
		beforeCount = *previous.Samples
	}
	if previous.TotalNanos != nil {
		beforeTotal = *previous.TotalNanos
	}
	if current.Samples != nil {
		afterCount = *current.Samples
	}
	if current.TotalNanos != nil {
		afterTotal = *current.TotalNanos
	}
	count := deltaUint(beforeCount, afterCount, valid)
	total := deltaUint(beforeTotal, afterTotal, valid)
	if count == 0 {
		return DurationDelta{Status: "unknown"}
	}
	return DurationDelta{Status: "measured", Samples: ptr(count), TotalNanos: ptr(total), MeanNanos: ptr(total / count)}
}

func stageDeltaHasSamples(stages StageDeltaSummary) bool {
	for _, stage := range [...]DurationDelta{stages.DirectoryLookup, stages.SourcePrepare, stages.URLResolve, stages.SourceProbe, stages.BuildQueue, stages.ArchiveIndex, stages.Decompression, stages.FileOpen} {
		if stage.Samples != nil && *stage.Samples > 0 {
			return true
		}
	}
	return false
}

func unknownStageDeltaSummary() StageDeltaSummary {
	unknown := DurationDelta{Status: "unknown"}
	return StageDeltaSummary{DirectoryLookup: unknown, SourcePrepare: unknown, URLResolve: unknown, SourceProbe: unknown, BuildQueue: unknown, ArchiveIndex: unknown, Decompression: unknown, FileOpen: unknown}
}
