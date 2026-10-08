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
