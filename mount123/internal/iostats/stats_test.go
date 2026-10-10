package iostats

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSnapshotMarksUnobservedMetricsUnknown(t *testing.T) {
	snapshot := New().Snapshot()
	if snapshot.ForegroundReadLatency.Status != "unknown" || snapshot.ForegroundReadLatency.Samples != nil || snapshot.DownloadedBytes.Status != "unknown" || snapshot.DownloadedBytes.Bytes != nil || snapshot.DirectoryLookup.Status != "unknown" || snapshot.Stages.ArchiveIndex.Status != "unknown" || snapshot.ImagePrefetch.Status != "unknown" {
		t.Fatalf("unobserved snapshot = %#v", snapshot)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"status":"unknown"`) {
		t.Fatalf("JSON does not make unavailable metrics explicit: %s", encoded)
	}
}

func TestSnapshotAggregatesIntoFixedHistogramAndCounters(t *testing.T) {
	stats := New()
	for _, d := range []time.Duration{time.Nanosecond, 2 * time.Nanosecond, 3 * time.Nanosecond, 20 * time.Nanosecond} {
		stats.ObserveForegroundRead(d)
	}
	stats.AddDownloadedBytes(0)
	stats.AddDownloadedBytes(12)
	snapshot := stats.Snapshot()
	if got := snapshot.ForegroundReadLatency; got.Status != "measured" || got.Samples == nil || *got.Samples != 4 || got.TotalNanos == nil || *got.TotalNanos != 26 || got.P50Nanos == nil || *got.P50Nanos == 0 || got.P95Nanos == nil || *got.P95Nanos == 0 || got.MaxNanos == nil || *got.MaxNanos != 20 {
		t.Fatalf("foreground histogram = %#v", got)
	}
	if got := snapshot.DownloadedBytes; got.Status != "measured" || got.Events == nil || *got.Events != 2 || got.Bytes == nil || *got.Bytes != 12 {
		t.Fatalf("download counter = %#v", got)
	}
	if len(stats.foregroundRead.buckets) != histogramBuckets {
		t.Fatalf("histogram bucket count = %d", len(stats.foregroundRead.buckets))
	}
}

func TestReadAheadAndSchedulerStatsAreBoundedAggregates(t *testing.T) {
	stats := New()
	stats.ObserveReadAheadWait(12 * time.Millisecond)
	stats.AddReadAheadScheduled(4096)
	stats.AddReadAheadCompleted(2048)
	stats.AddReadAheadConsumed(1024)
	stats.AddReadAheadWasted(1024)
	snapshot := stats.Snapshot()
	snapshot.SetDownloadScheduler(8, 64<<20, 2, 1<<20, 48<<20, 1, 3, 17)
	if snapshot.ReadAheadWait.Status != "measured" || snapshot.ReadAheadScheduled.Bytes == nil || *snapshot.ReadAheadScheduled.Bytes != 4096 || snapshot.ReadAheadCompleted.Bytes == nil || *snapshot.ReadAheadCompleted.Bytes != 2048 || snapshot.ReadAheadConsumed.Bytes == nil || *snapshot.ReadAheadConsumed.Bytes != 1024 || snapshot.ReadAheadWasted.Bytes == nil || *snapshot.ReadAheadWasted.Bytes != 1024 {
		t.Fatalf("read-ahead aggregate = %#v", snapshot)
	}
	if snapshot.DownloadScheduler.Status != "measured" || snapshot.DownloadScheduler.MaximumRequests != 8 || snapshot.DownloadScheduler.MaximumInFlightBytes != 64<<20 || snapshot.DownloadScheduler.WaitingForeground != 1 || snapshot.DownloadScheduler.CompletedRequests != 17 {
		t.Fatalf("scheduler aggregate = %#v", snapshot.DownloadScheduler)
	}
}

func TestStageTrackerUsesFixedShapeAndUnknownUntilObserved(t *testing.T) {
	stats := New()
	if got := stats.Snapshot().Stages; got.DirectoryLookup.Status != "unknown" || got.Decompression.Status != "unknown" {
		t.Fatalf("empty stage summary = %+v", got)
	}
	stats.ObserveStage(StageDecompression, 11*time.Millisecond)
	stats.ObserveStage(stageCount, time.Second) // invalid stage is ignored.
	snapshot := stats.Snapshot()
	if got := snapshot.Stages.Decompression; got.Status != "measured" || got.Samples == nil || *got.Samples != 1 || got.TotalNanos == nil || *got.TotalNanos != uint64(11*time.Millisecond) {
		t.Fatalf("decompression stage = %+v", got)
	}
	if snapshot.Stages.DirectoryLookup.Status != "unknown" {
		t.Fatalf("unobserved stage changed: %+v", snapshot.Stages.DirectoryLookup)
	}
}

func TestOperationCountersSplitForegroundAndBackground(t *testing.T) {
	stats := New()
	if got := stats.Snapshot(); got.Readdir.Status != "idle" || got.Lookup.Requests != 0 || got.Open.Status != "idle" {
		t.Fatalf("unobserved operation summary = %+v", got)
	}
	foreground := stats.BeginOperation(OperationLookup, false)
	background := stats.BeginOperation(OperationLookup, true)
	background()
	background() // releasing twice must not double-count the active gauge.
	snapshot := stats.Snapshot()
	if got := snapshot.Lookup; got.Status != "measured" || got.Requests != 2 || got.ForegroundRequests != 1 || got.BackgroundRequests != 1 || got.ActiveForeground != 1 || got.ActiveBackground != 0 {
		t.Fatalf("lookup summary = %+v", got)
	}
	foreground()
	stats.BeginOperation(operationCount, true)() // invalid operation is ignored.
	if got := stats.Snapshot().Lookup.ActiveForeground; got != 0 {
		t.Fatalf("active foreground = %d, want 0", got)
	}
}

func TestSnapshotIndexQueueReportsWaitingRequests(t *testing.T) {
	snapshot := New().Snapshot()
	snapshot.SetIndexQueue(4, 2, 1, 3, 12)
	if got := snapshot.IndexQueue; got.Status != "measured" || got.Limit != 4 || got.Active != 2 || got.BackgroundActive != 1 || got.Waiting != 3 || got.Requests != 12 {
		t.Fatalf("index queue = %+v", got)
	}
}

func TestSnapshotScanStateIsExplicitAndBounded(t *testing.T) {
	snapshot := New().Snapshot()
	if got := snapshot.Scan; got.Status != "" || got.Shedding {
		t.Fatalf("unset scan state = %+v", got)
	}
	snapshot.SetScan(ScanSummary{Status: "measured", Shedding: true, Reason: "bulk_metadata_scan", MetadataRequestsPerSecond: 240, EnterRequestsPerSecond: 100, ExitRequestsPerSecond: 25, Since: time.Unix(5, 0)})
	got := snapshot.Scan
	if !got.Shedding || got.Reason != "bulk_metadata_scan" || got.MetadataRequestsPerSecond != 240 || got.EnterRequestsPerSecond != 100 || got.ExitRequestsPerSecond != 25 || !got.Since.Equal(time.Unix(5, 0)) {
		t.Fatalf("scan state = %+v", got)
	}
}
