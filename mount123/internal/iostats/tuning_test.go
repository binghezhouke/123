package iostats

import (
	"testing"
	"time"
)

func tuningSamples() (Snapshot, Snapshot) {
	started := time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC)
	p := Snapshot{StartedAt: started, CollectedAt: started, Cache: CacheSummary{Status: "measured", CapacityBytes: 50 << 30, UsedBytes: 20 << 30}}
	c := p
	c.CollectedAt = started.Add(30 * time.Second)
	return p, c
}

func TestAnalyzeKeepsUnobservedRatiosNullAndRejectsRestart(t *testing.T) {
	p, c := tuningSamples()
	r := Analyze(p, c)
	if r.Status != "idle" || r.ForegroundByteHitRatio != nil || r.BackgroundByteHitRatio != nil || r.LifetimeReadAheadUseRatio != nil || len(r.Recommendations) != 0 {
		t.Fatalf("idle report = %+v", r)
	}
	c.StartedAt = c.StartedAt.Add(time.Second)
	if r := Analyze(p, c); r.Status != "reset" || len(r.Recommendations) != 0 {
		t.Fatalf("restart report = %+v", r)
	}
	c.StartedAt = p.StartedAt
	c.CollectedAt = p.CollectedAt
	if r := Analyze(p, c); r.Status != "reset" {
		t.Fatalf("same-time report = %+v", r)
	}
	c.CollectedAt = p.CollectedAt.Add(time.Second)
	p.Cache.CapacityEvictions = 10
	c.Cache.CapacityEvictions = 1
	if r := Analyze(p, c); r.Status != "reset" {
		t.Fatalf("decreasing counter report = %+v", r)
	}
}

func TestAnalyzeColdScanDoesNotRecommendLargerCacheWithoutRefaults(t *testing.T) {
	p, c := tuningSamples()
	c.Cache.UsedBytes = c.Cache.CapacityBytes
	c.Cache.Foreground = CacheRangeSummary{ReadRequests: 64, RequestedBytes: 64 << 20, MissBytes: 64 << 20}
	c.Cache.CapacityEvictions = 64
	c.Cache.CapacityEvictionBytes = 64 << 20
	r := Analyze(p, c)
	if r.Status != "measured" || r.ForegroundByteHitRatio == nil || *r.ForegroundByteHitRatio != 0 || len(r.Recommendations) != 0 {
		t.Fatalf("single-pass scan report = %+v", r)
	}
	c.Cache.Refaults = CacheRefaultSummary{Count: 8, Bytes: 8 << 20}
	r = Analyze(p, c)
	if len(r.Recommendations) != 1 || r.Recommendations[0].Action != "compare_larger_cache" {
		t.Fatalf("refault pressure report = %+v", r)
	}
}

func TestAnalyzeUsesIntervalCountersAndKeepsRatesInScope(t *testing.T) {
	p, c := tuningSamples()
	p.Cache.Foreground = CacheRangeSummary{ReadRequests: 100, FullHits: 50, RequestedBytes: 100 << 20, HitBytes: 50 << 20, MissBytes: 50 << 20}
	c.Cache.Foreground = CacheRangeSummary{ReadRequests: 164, FullHits: 98, RequestedBytes: 164 << 20, HitBytes: 98 << 20, MissBytes: 66 << 20}
	p.DownloadedBytes = CounterSummary{Bytes: ptr(uint64(10 << 20))}
	c.DownloadedBytes = CounterSummary{Bytes: ptr(uint64(40 << 20))}
	p.ForegroundReadLatency = DurationSummary{Samples: ptr(uint64(10)), TotalNanos: ptr(uint64(20e6))}
	c.ForegroundReadLatency = DurationSummary{Samples: ptr(uint64(30)), TotalNanos: ptr(uint64(100e6))}
	r := Analyze(p, c)
	if r.Foreground.ReadRequests != 64 || r.ForegroundByteHitRatio == nil || *r.ForegroundByteHitRatio != .75 || r.DownloadBytesPerSecond != 1<<20 || r.MeanForegroundReadMillis == nil || *r.MeanForegroundReadMillis != 4 {
		t.Fatalf("interval report = %+v", r)
	}
	if len(r.Recommendations) != 0 {
		t.Fatalf("healthy report advises change: %+v", r)
	}
}

func TestAnalyzeRequiresMeaningfulSampleBeforeReadAheadAdvice(t *testing.T) {
	p, c := tuningSamples()
	c.Cache.Foreground = CacheRangeSummary{ReadRequests: 64, RequestedBytes: 64 << 20, HitBytes: 64 << 20}
	c.ReadAheadConsumed = CounterSummary{Bytes: ptr(uint64(8 << 20))}
	c.ReadAheadWasted = CounterSummary{Events: ptr(uint64(4)), Bytes: ptr(uint64(120 << 20))}
	r := Analyze(p, c)
	if r.LifetimeReadAheadUseRatio == nil || *r.LifetimeReadAheadUseRatio != .0625 || len(r.Recommendations) != 1 || r.Recommendations[0].Action != "compare_smaller_window" {
		t.Fatalf("closed-handle read-ahead report = %+v", r)
	}
	c.CollectedAt = p.CollectedAt.Add(time.Second)
	r = Analyze(p, c)
	if r.Status != "insufficient_samples" || len(r.Recommendations) != 0 {
		t.Fatalf("short sample advice = %+v", r)
	}
	c.Cache.ENOSPC++
	r = Analyze(p, c)
	if len(r.Recommendations) != 1 || r.Recommendations[0].Action != "inspect_capacity" {
		t.Fatalf("capacity error went unreported: %+v", r)
	}
}

func TestTrackerTimestampsIdentifyOneProcessAcrossSnapshots(t *testing.T) {
	tracker := New()
	before, after := tracker.Snapshot(), tracker.Snapshot()
	if before.StartedAt.IsZero() || !before.StartedAt.Equal(after.StartedAt) || after.CollectedAt.Before(before.CollectedAt) || after.UptimeSeconds < before.UptimeSeconds {
		t.Fatalf("inconsistent tracker timestamps: before=%+v after=%+v", before, after)
	}
}

func TestAnalyzeIncludesDirectoryAndRecoveryActivity(t *testing.T) {
	p, c := tuningSamples()
	p.DirectoryCache = DirectorySummary{Status: "measured", ListCalls: 7, DiskRestores: 2}
	c.DirectoryCache = DirectorySummary{Status: "measured", ListCalls: 8, DiskRestores: 3, StaleFailures: 1, RetryBackoffs: 1}
	p.RemoteRecovery = RecoverySummary{Status: "measured", Attempts: 30, Retries: 3, Recovered: 2}
	c.RemoteRecovery = RecoverySummary{Status: "measured", Attempts: 33, Retries: 4, Recovered: 3}
	r := Analyze(p, c)
	if r.Status == "idle" || r.DirectoryCache.ListCalls != 1 || r.DirectoryCache.DiskRestores != 1 || r.DirectoryCache.StaleFailures != 1 || r.RemoteRecovery.Attempts != 3 || r.RemoteRecovery.Retries != 1 || r.RemoteRecovery.Recovered != 1 {
		t.Fatalf("metadata/recovery activity lost in interval: %+v", r)
	}
	if len(r.Recommendations) != 0 {
		t.Fatalf("metadata-only activity generated data-cache tuning advice: %+v", r.Recommendations)
	}
	c.DirectoryCache.ListCalls = 1
	if r := Analyze(p, c); r.Status != "reset" {
		t.Fatalf("decreasing directory counter report = %+v", r)
	}
}

func TestAnalyzeStageDeltasAndLegacyFillOutcomeUnknown(t *testing.T) {
	p, c := tuningSamples()
	p.Cache.FillOutcomeVersion = 1
	c.Cache.FillOutcomeVersion = 1
	p.StageSchemaVersion = 1
	c.StageSchemaVersion = 1
	p.Cache.FillCancelled, c.Cache.FillCancelled = 2, 5
	p.Cache.FillErrors, c.Cache.FillErrors = 1, 3
	p.Stages.BuildQueue = DurationSummary{Status: "measured", Samples: ptr(uint64(4)), TotalNanos: ptr(uint64(40))}
	c.Stages.BuildQueue = DurationSummary{Status: "measured", Samples: ptr(uint64(10)), TotalNanos: ptr(uint64(190))}
	r := Analyze(p, c)
	if r.FillOutcomeStatus != "measured" || r.FillCancelled == nil || *r.FillCancelled != 3 || r.FillErrors == nil || *r.FillErrors != 2 {
		t.Fatalf("fill outcomes = %+v", r)
	}
	if got := r.Stages.BuildQueue; got.Status != "measured" || got.Samples == nil || *got.Samples != 6 || got.TotalNanos == nil || *got.TotalNanos != 150 || got.MeanNanos == nil || *got.MeanNanos != 25 {
		t.Fatalf("build queue interval = %+v", got)
	}
	p.Cache.FillOutcomeVersion = 0 // old JSON logs omit this field.
	c.Cache.FillOutcomeVersion = 0
	r = Analyze(p, c)
	if r.FillOutcomeStatus != "unknown" || r.FillCancelled != nil || r.FillErrors != nil {
		t.Fatalf("legacy fill outcome was presented as measured: %+v", r)
	}
	p.StageSchemaVersion = 0
	r = Analyze(p, c)
	if r.Stages.BuildQueue.Status != "unknown" || r.Stages.BuildQueue.Samples != nil {
		t.Fatalf("legacy stage schema was treated as a valid baseline: %+v", r.Stages.BuildQueue)
	}
}

func TestAnalyzeReportsWindowedMetadataRates(t *testing.T) {
	p, c := tuningSamples()
	p.Readdir = OperationSummary{Status: "measured", Requests: 10, ForegroundRequests: 10}
	c.Readdir = OperationSummary{Status: "measured", Requests: 40, ForegroundRequests: 34, BackgroundRequests: 6, ActiveForeground: 2, ActiveBackground: 1}
	p.Lookup = OperationSummary{Status: "measured", Requests: 100, ForegroundRequests: 100}
	c.Lookup = OperationSummary{Status: "measured", Requests: 400, ForegroundRequests: 400}
	r := Analyze(p, c)
	if got := r.Operations.Readdir; got.Status != "measured" || got.Requests != 30 || got.ForegroundRequests != 24 || got.BackgroundRequests != 6 || got.RequestsPerSecond != 1 {
		t.Fatalf("readdir delta = %+v", got)
	}
	if got := r.Operations.Readdir; got.ActiveForeground != 2 || got.ActiveBackground != 1 {
		t.Fatalf("readdir active gauges = %+v", got)
	}
	if got := r.Operations.Lookup; got.Requests != 300 || got.RequestsPerSecond != 10 {
		t.Fatalf("lookup delta = %+v", got)
	}
	if got := r.Operations.Getattr; got.Status != "idle" || got.Requests != 0 {
		t.Fatalf("idle operation reported activity: %+v", got)
	}
	// A counter that moves backwards means the mount restarted: the whole
	// report is rejected instead of publishing a negative rate.
	c.Readdir.Requests = 1
	if r := Analyze(p, c); r.Status != "reset" {
		t.Fatalf("decreasing operation counter report = %+v", r)
	}
}

func TestAnalyzeDistinguishesIdleBrowseAndBulkScanWindows(t *testing.T) {
	idleStart, idleEnd := tuningSamples()
	if r := Analyze(idleStart, idleEnd); r.Operations.Readdir.Requests != 0 || r.Operations.Lookup.Requests != 0 || r.Operations.Readdir.RequestsPerSecond != 0 {
		t.Fatalf("idle window reported activity: %+v", r.Operations)
	}

	browseStart, browseEnd := tuningSamples()
	browseEnd.Readdir = OperationSummary{Status: "measured", Requests: 4, ForegroundRequests: 4}
	browseEnd.Lookup = OperationSummary{Status: "measured", Requests: 12, ForegroundRequests: 12}
	browse := Analyze(browseStart, browseEnd)
	if got := browse.Operations.Lookup; got.RequestsPerSecond != 0.4 || got.BackgroundRequests != 0 {
		t.Fatalf("browse window = %+v", got)
	}

	scanStart := browseEnd
	scanStart.CollectedAt = browseEnd.CollectedAt
	scanEnd := scanStart
	scanEnd.CollectedAt = scanStart.CollectedAt.Add(30 * time.Second)
	scanEnd.Readdir = OperationSummary{Status: "measured", Requests: 904, ForegroundRequests: 904}
	scanEnd.Lookup = OperationSummary{Status: "measured", Requests: 5412, ForegroundRequests: 5412}
	scan := Analyze(scanStart, scanEnd)
	if scan.Operations.Readdir.RequestsPerSecond <= browse.Operations.Readdir.RequestsPerSecond || scan.Operations.Lookup.RequestsPerSecond <= browse.Operations.Lookup.RequestsPerSecond {
		t.Fatalf("scan window is not distinguishable from browsing: %+v vs %+v", scan.Operations, browse.Operations)
	}
	if scan.Operations.Lookup.RequestsPerSecond != 180 {
		t.Fatalf("scan lookup rate = %v, want 180", scan.Operations.Lookup.RequestsPerSecond)
	}
}
