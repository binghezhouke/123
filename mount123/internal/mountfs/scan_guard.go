package mountfs

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/binghezhouke/123/mount123/internal/iostats"
)

// Bulk metadata traffic is a scan: a file manager building a tree, `find`, or a
// backup walk issues hundreds of foreground lookups per second, while
// interactive browsing stays in the low single digits. Enter and exit rates
// differ by 4x so a scan that slows down does not flap the state.
//
// The thresholds are initial, explainable values rather than hard limits.
// Shedding never rejects a request; it only lowers background load.
const (
	scanSampleInterval   = 2 * time.Second
	scanEnterRate        = 100.0
	scanExitRate         = 25.0
	scanEnterWindows     = 2
	scanExitWindows      = 3
	shedBackgroundBuilds = 1
	probeMaxConcurrency  = 2
	scanReasonBulkScan   = "bulk_metadata_scan"
)

// scanGuard turns a sampled metadata request rate into an explainable shedding
// state. Sampling happens on the request path, so an idle mount does no work
// and the state recovers on its own once traffic drops.
type scanGuard struct {
	mu       sync.Mutex
	interval time.Duration
	enter    float64
	exit     float64
	enterFor int
	exitFor  int
	now      func() time.Time
	onChange func(bool)

	sampled  time.Time
	previous iostats.OperationTotals
	high     int
	low      int
	rate     float64
	shedding bool
	since    time.Time
	next     atomic.Int64
}

func newScanGuard(onChange func(bool)) *scanGuard {
	return &scanGuard{
		interval: scanSampleInterval,
		enter:    scanEnterRate,
		exit:     scanExitRate,
		enterFor: scanEnterWindows,
		exitFor:  scanExitWindows,
		now:      time.Now,
		onChange: onChange,
	}
}

// due reports whether another rate sample is allowed. The metadata request
// path calls this on every lookup, so the common case is one atomic read.
func (g *scanGuard) due(now time.Time) bool {
	return !now.Before(time.Unix(0, g.next.Load()))
}

// observe folds a cumulative request count into the shedding state. Background
// traffic is ignored: probing, prefetch and refreshes must not sustain their
// own demotion.
func (g *scanGuard) observe(now time.Time, totals iostats.OperationTotals) {
	g.mu.Lock()
	if g.sampled.IsZero() {
		g.sampled, g.previous = now, totals
		g.next.Store(now.Add(g.interval).UnixNano())
		g.mu.Unlock()
		return
	}
	elapsed := now.Sub(g.sampled).Seconds()
	if elapsed <= 0 {
		g.mu.Unlock()
		return
	}
	var foreground uint64
	if totals.Foreground > g.previous.Foreground {
		foreground = totals.Foreground - g.previous.Foreground
	}
	g.rate = float64(foreground) / elapsed
	g.sampled, g.previous = now, totals
	g.next.Store(now.Add(g.interval).UnixNano())
	changed := false
	switch {
	case g.shedding && g.rate <= g.exit:
		g.low++
		if g.low >= g.exitFor {
			g.shedding, g.low, g.high, g.since = false, 0, 0, time.Time{}
			changed = true
		}
	case g.shedding:
		g.low = 0
	case g.rate >= g.enter:
		g.high++
		if g.high >= g.enterFor {
			g.shedding, g.high, g.low, g.since = true, 0, 0, now
			changed = true
		}
	default:
		g.high = 0
	}
	shedding := g.shedding
	g.mu.Unlock()
	if changed && g.onChange != nil {
		g.onChange(shedding)
	}
}

func (g *scanGuard) state() iostats.ScanSummary {
	g.mu.Lock()
	defer g.mu.Unlock()
	summary := iostats.ScanSummary{
		Status:                    "measured",
		Shedding:                  g.shedding,
		MetadataRequestsPerSecond: g.rate,
		EnterRequestsPerSecond:    g.enter,
		ExitRequestsPerSecond:     g.exit,
	}
	if g.shedding {
		summary.Reason = scanReasonBulkScan
		summary.Since = g.since
	}
	return summary
}

// observeScan samples metadata traffic for sustained bulk scanning.
func (t *Tree) observeScan() {
	if t == nil {
		return
	}
	guard := t.scan
	if guard == nil {
		return
	}
	now := guard.now()
	if !guard.due(now) {
		return
	}
	stats := t.ioStats()
	if stats == nil {
		return
	}
	guard.observe(now, stats.OperationTotals())
}

// scanShedding reports whether background work is currently shedding load.
func (t *Tree) scanShedding() bool {
	if t == nil || t.scan == nil {
		return false
	}
	t.scan.mu.Lock()
	defer t.scan.mu.Unlock()
	return t.scan.shedding
}

// probeConcurrency reports how many archive probes may run at once. While a
// bulk scan is running, probing keeps a single slot so it cannot compete with
// the foreground work the user asked for.
func (t *Tree) probeConcurrency() int {
	if t.scanShedding() {
		return 1
	}
	return probeMaxConcurrency
}

func (t *Tree) scanState() iostats.ScanSummary {
	if t == nil || t.scan == nil {
		return iostats.ScanSummary{Status: "measured"}
	}
	return t.scan.state()
}

// applyScanShedding reserves the build queue for foreground work and pauses
// image prefetch while a scan runs. Explicit opens, reads and refreshes keep
// their normal path and full capacity; background work resumes by itself when
// the observed rate drops.
func (t *Tree) applyScanShedding(shedding bool) {
	if t == nil {
		return
	}
	if t.buildGate != nil {
		if shedding {
			t.buildGate.SetBackgroundLimit(shedBackgroundBuilds)
		} else {
			t.buildGate.SetBackgroundLimit(0)
		}
	}
	if shedding && t.prefetch != nil {
		t.prefetch.interrupt()
	}
}
