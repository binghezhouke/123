package mountfs

import (
	"context"
	"syscall"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/iostats"
	"github.com/binghezhouke/123/mount123/internal/workqueue"
	"github.com/hanwen/go-fuse/v2/fs"
)

type scanTestClock struct{ now time.Time }

func (c *scanTestClock) advance(d time.Duration) { c.now = c.now.Add(d) }

func testScanGuard(clock *scanTestClock, changes *[]bool) *scanGuard {
	guard := newScanGuard(func(shedding bool) { *changes = append(*changes, shedding) })
	guard.now = func() time.Time { return clock.now }
	return guard
}

// driveScanWindow advances one sample interval and feeds the requests observed
// during it, mirroring what the metadata request path reports.
func driveScanWindow(clock *scanTestClock, guard *scanGuard, totals *iostats.OperationTotals, foreground int) {
	clock.advance(guard.interval)
	totals.Foreground += uint64(foreground)
	guard.observe(clock.now, *totals)
}

func TestScanGuardShedsDuringSustainedScanAndRecovers(t *testing.T) {
	clock := &scanTestClock{now: time.Unix(0, 0)}
	var changes []bool
	guard := testScanGuard(clock, &changes)
	var totals iostats.OperationTotals
	guard.observe(clock.now, totals) // the first sample only records a baseline

	burst := int(guard.enter * guard.interval.Seconds() * 2)
	for i := 0; i < guard.enterFor; i++ {
		driveScanWindow(clock, guard, &totals, burst)
	}
	state := guard.state()
	if !state.Shedding || state.Reason != scanReasonBulkScan || state.Status != "measured" {
		t.Fatalf("sustained scan did not shed background work: %+v", state)
	}
	if state.MetadataRequestsPerSecond < guard.enter || state.EnterRequestsPerSecond != guard.enter || state.ExitRequestsPerSecond != guard.exit {
		t.Fatalf("observed rate and thresholds are not reported: %+v", state)
	}
	if len(changes) != 1 || !changes[0] {
		t.Fatalf("state change notifications = %v, want one entry", changes)
	}

	// A scan that slows down but stays above the exit rate keeps shedding.
	slow := int(guard.exit * guard.interval.Seconds() * 2)
	for i := 0; i < guard.exitFor+1; i++ {
		driveScanWindow(clock, guard, &totals, slow)
	}
	if !guard.state().Shedding {
		t.Fatal("hysteresis stopped shedding while traffic stayed above the exit rate")
	}

	for i := 0; i < guard.exitFor; i++ {
		driveScanWindow(clock, guard, &totals, 0)
	}
	if guard.state().Shedding || guard.state().Reason != "" {
		t.Fatalf("idle traffic did not restore normal background work: %+v", guard.state())
	}
	if len(changes) != 2 || changes[1] {
		t.Fatalf("state change notifications = %v, want enter then exit", changes)
	}
}

func TestScanGuardIgnoresBackgroundTraffic(t *testing.T) {
	clock := &scanTestClock{now: time.Unix(0, 0)}
	var changes []bool
	guard := testScanGuard(clock, &changes)
	var totals iostats.OperationTotals
	guard.observe(clock.now, totals)
	for i := 0; i < guard.enterFor+2; i++ {
		clock.advance(guard.interval)
		totals.Background += uint64(guard.enter * guard.interval.Seconds() * 10)
		guard.observe(clock.now, totals)
	}
	if guard.state().Shedding {
		t.Fatalf("probing and prefetch traffic triggered shedding: %+v", guard.state())
	}
	if len(changes) != 0 {
		t.Fatalf("background-only traffic changed the shedding state: %v", changes)
	}
}

func TestSustainedScanShedsBackgroundWorkAndKeepsForegroundAvailable(t *testing.T) {
	root, _ := fixture(t)
	root.tree.prefetch = newImagePrefetch(root.tree)
	clock := &scanTestClock{now: time.Unix(0, 0)}
	root.tree.scan.now = func() time.Time { return clock.now }

	drive := func(requests int) {
		for i := 0; i < requests; i++ {
			root.tree.beginOperation(context.Background(), iostats.OperationLookup)()
		}
		clock.advance(scanSampleInterval)
		root.tree.observeScan()
	}
	drive(0) // baseline sample
	burst := int(scanEnterRate * scanSampleInterval.Seconds() * 2)
	for i := 0; i < scanEnterWindows; i++ {
		drive(burst)
	}
	if !root.tree.scanShedding() {
		t.Fatalf("sustained scan did not shed background work: %+v", root.IOStats().Scan)
	}
	if snapshot := root.IOStats().Scan; !snapshot.Shedding || snapshot.Reason != scanReasonBulkScan || snapshot.Status != "measured" {
		t.Fatalf("scan state is not visible in io-stats: %+v", snapshot)
	}

	entries, err := root.list(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	folder := &Node{tree: root.tree, item: entries["folder"], parent: root}
	children, err := folder.list(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	file := &Node{tree: root.tree, item: children["plain.txt"], parent: folder}

	root.tree.prefetch.observe(file)
	if got := root.ImagePrefetchStats().Planned; got != 0 {
		t.Fatalf("image prefetch planned work while a scan was running: %d", got)
	}

	// Users keep working during a scan: opening and reading a known path must
	// succeed with no artificial errors or extra waiting.
	handle, _, errno := file.Open(context.Background(), syscall.O_RDONLY)
	if errno != 0 {
		t.Fatalf("open during shedding = %v, want success", errno)
	}
	_ = handle.(fs.FileReleaser).Release(context.Background())

	for i := 0; i < scanExitWindows; i++ {
		drive(0)
	}
	if root.tree.scanShedding() {
		t.Fatalf("background work did not recover after the scan ended: %+v", root.IOStats().Scan)
	}
	root.tree.prefetch.observe(file)
	if got := root.ImagePrefetchStats().Planned; got != 1 {
		t.Fatalf("image prefetch did not resume: %d", got)
	}
	root.tree.prefetch.interrupt()
}

func TestSheddingDemotesProbeAndBackgroundBuildCapacity(t *testing.T) {
	root, _ := fixture(t)
	clock := &scanTestClock{now: time.Unix(0, 0)}
	root.tree.scan.now = func() time.Time { return clock.now }
	release, err := root.tree.acquireBuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	release()
	if got := root.tree.probeConcurrency(); got != probeMaxConcurrency {
		t.Fatalf("probe concurrency = %d, want %d", got, probeMaxConcurrency)
	}

	burst := int(scanEnterRate * scanSampleInterval.Seconds() * 2)
	root.tree.observeScan() // baseline sample
	for i := 0; i < scanEnterWindows; i++ {
		for j := 0; j < burst; j++ {
			root.tree.beginOperation(context.Background(), iostats.OperationLookup)()
		}
		clock.advance(scanSampleInterval)
		root.tree.observeScan()
	}
	if !root.tree.scanShedding() {
		t.Fatal("sustained scan did not start shedding background work")
	}
	if got := root.tree.probeConcurrency(); got != 1 {
		t.Fatalf("probe concurrency = %d during a scan, want 1", got)
	}

	background := workqueue.Background(context.Background())
	held, err := root.tree.acquireBuild(background)
	if err != nil {
		t.Fatal(err)
	}
	second := make(chan func(), 1)
	go func() {
		release, err := root.tree.acquireBuild(background)
		if err == nil {
			second <- release
		}
	}()
	select {
	case release := <-second:
		release()
		t.Fatal("background builds exceeded the share left during a scan")
	case <-time.After(20 * time.Millisecond):
	}
	foreground, err := root.tree.acquireBuild(context.Background())
	if err != nil {
		t.Fatalf("foreground build was denied capacity during a scan: %v", err)
	}
	foreground()
	held()
	select {
	case release := <-second:
		release()
	case <-time.After(time.Second):
		t.Fatal("background builds did not resume after the share was released")
	}
}
