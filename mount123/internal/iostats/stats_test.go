package iostats

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSnapshotMarksUnobservedMetricsUnknown(t *testing.T) {
	snapshot := New().Snapshot()
	if snapshot.ForegroundReadLatency.Status != "unknown" || snapshot.ForegroundReadLatency.Samples != nil || snapshot.DownloadedBytes.Status != "unknown" || snapshot.DownloadedBytes.Bytes != nil || snapshot.DirectoryLookup.Status != "unknown" {
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
	if got := snapshot.ForegroundReadLatency; got.Status != "measured" || got.Samples == nil || *got.Samples != 4 || got.P50Nanos == nil || *got.P50Nanos == 0 || got.P95Nanos == nil || *got.P95Nanos == 0 || got.MaxNanos == nil || *got.MaxNanos != 20 {
		t.Fatalf("foreground histogram = %#v", got)
	}
	if got := snapshot.DownloadedBytes; got.Status != "measured" || got.Events == nil || *got.Events != 2 || got.Bytes == nil || *got.Bytes != 12 {
		t.Fatalf("download counter = %#v", got)
	}
	if len(stats.foregroundRead.buckets) != histogramBuckets {
		t.Fatalf("histogram bucket count = %d", len(stats.foregroundRead.buckets))
	}
}
