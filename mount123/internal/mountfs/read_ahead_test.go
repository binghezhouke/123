package mountfs

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/hanwen/go-fuse/v2/fs"
)

type observedRange struct{ start, end int64 }

func readAheadRemote(t *testing.T, data []byte, block func(*http.Request, int64, int64)) (*storage.Remote, <-chan observedRange, *atomic.Int32) {
	t.Helper()
	requests := make(chan observedRange, 64)
	var count atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var start, end int64
		if _, err := fmt.Sscanf(req.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			http.Error(w, "bad range", 400)
			return
		}
		if !(start == 0 && end == 0) {
			count.Add(1)
			requests <- observedRange{start, end}
			if block != nil {
				block(req, start, end)
			}
		}
		w.Header().Set("ETag", `"read-ahead-v1"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
	}))
	t.Cleanup(server.Close)
	cache, err := storage.NewCache(t.TempDir(), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	remote, err := storage.NewRemote(context.Background(), cache, "read-ahead-test", int64(len(data)), func(context.Context) (string, error) { return server.URL, nil })
	if err != nil {
		t.Fatal(err)
	}
	return remote, requests, &count
}

func waitRange(t *testing.T, requests <-chan observedRange) observedRange {
	t.Helper()
	select {
	case r := <-requests:
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for range request")
		return observedRange{}
	}
}

func waitReadAheadJobs(t *testing.T, r *readAhead, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		jobs := r.jobs
		r.mu.Unlock()
		if jobs == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("read-ahead jobs did not reach %d", want)
}

func TestSequentialReadAheadPopulatesCacheForLaterRead(t *testing.T) {
	data := bytes.Repeat([]byte("sequential-data-"), int((8<<20)/16+1))[:8<<20]
	remote, requests, count := readAheadRemote(t, data, nil)
	h := &handle{remote: remote, size: uint64(len(data)), readAhead: newReadAhead(context.Background(), remote, 0, uint64(len(data)), 4<<20)}
	first := make([]byte, 64<<10)
	if result, errno := h.Read(context.Background(), first, 0); errno != 0 || result.Size() != len(first) {
		t.Fatalf("first read = %v, %v", result, errno)
	}
	// The foreground read fetches the ordinary 1 MiB extent.
	if rg := waitRange(t, requests); rg.start != 0 || rg.end != (1<<20)-1 {
		t.Fatalf("foreground range = %+v", rg)
	}
	// Consume sequentially. The controller grows its ahead window gradually,
	// while keeping requests at a separate, bounded chunk size.
	for off := int64(64 << 10); off < 1<<20; off += 64 << 10 {
		result, errno := h.Read(context.Background(), make([]byte, 64<<10), off)
		if errno != 0 || result.Size() != 64<<10 {
			t.Fatalf("sequential read at %d = %v, %v", off, result, errno)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		h.readAhead.mu.Lock()
		frontier, complete := h.readAhead.frontier, h.readAhead.ranges
		cachedThrough := int64(0)
		for _, rg := range complete {
			if rg.complete && rg.start <= 2<<20 && rg.end > cachedThrough {
				cachedThrough = rg.end
			}
		}
		h.readAhead.mu.Unlock()
		if frontier >= 1<<20 && cachedThrough >= 2<<20 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if time.Now().After(deadline) {
		t.Fatal("sequential consumption did not fill the ahead window through 2 MiB")
	}
	waitReadAheadJobs(t, h.readAhead, 0)
	before := count.Load()
	later := make([]byte, 4096)
	if n, err := remote.ReadAtContext(context.Background(), later, 2<<20); err != nil || n != len(later) {
		t.Fatalf("prefetched read = %d, %v", n, err)
	}
	if count.Load() != before {
		t.Fatalf("later read issued another HTTP request: %d -> %d", before, count.Load())
	}
	if errno := h.Release(context.Background()); errno != 0 {
		t.Fatal(errno)
	}
}

func TestReadAheadRunsDisjointRangesConcurrently(t *testing.T) {
	data := bytes.Repeat([]byte("parallel-read-ahead"), int((8<<20)/19+1))[:8<<20]
	started := make(chan struct{}, 2)
	release := make(chan struct{}, 2)
	remote, requests, _ := readAheadRemote(t, data, func(req *http.Request, _, _ int64) { started <- struct{}{}; <-release })
	ra := newReadAhead(context.Background(), remote, 0, uint64(len(data)), 4<<20)
	ra.observe(0, 4096, 0)
	ra.observe(4096, 4096, 0)
	first, second := waitRange(t, requests), waitRange(t, requests)
	if first.start < second.end && second.start < first.end {
		t.Fatalf("prefetch ranges overlap: %+v %+v", first, second)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			close(release)
			t.Fatal("non-overlapping background ranges were serialized")
		}
	}
	release <- struct{}{}
	release <- struct{}{}
	ra.Close()
}

func TestReadAheadBoundsReadyBytesForSlowConsumer(t *testing.T) {
	data := bytes.Repeat([]byte("bounded-ready"), int((8<<20)/13+1))[:8<<20]
	remote, requests, _ := readAheadRemote(t, data, nil)
	ra := newReadAhead(context.Background(), remote, 0, uint64(len(data)), 8<<20)
	ra.observe(0, 4096, 0)
	_ = waitRange(t, requests)
	waitReadAheadJobs(t, ra, 0)
	for i := 0; i < 100; i++ {
		// Repeated FUSE observations without advancing consumption must not
		// keep extending the ready window.
		ra.observe(0, 4096, 0)
	}
	ra.mu.Lock()
	frontier, maxEnd := ra.frontier, ra.frontier
	var ready, inFlight int64
	for _, rg := range ra.ranges {
		maxEnd = max(maxEnd, rg.end)
		if rg.complete {
			ready += rg.end - max(frontier, rg.start)
		} else {
			inFlight += rg.end - max(frontier, rg.start)
		}
	}
	window := ra.window
	ra.mu.Unlock()
	if maxEnd-frontier > window || ready+inFlight > window {
		t.Fatalf("unconsumed+in-flight lead ready=%d in-flight=%d end=%d frontier=%d window=%d", ready, inFlight, maxEnd, frontier, window)
	}
	if ready == 0 {
		t.Fatal("fast source did not publish a completed range")
	}
	ra.Close()
}

func TestPacedCachedReadsDoNotLookLikeFastConsumption(t *testing.T) {
	data := bytes.Repeat([]byte("paced-consumer"), (32<<20)/14+1)[:32<<20]
	remote, requests, _ := readAheadRemote(t, data, nil)
	// Keep the observed foreground reads hot so application pacing, rather
	// than a server delay, controls how quickly the application consumes data.
	if err := remote.PrefetchRangeAtContext(context.Background(), 0, 1<<20); err != nil {
		t.Fatal(err)
	}
	_ = waitRange(t, requests)
	h := &handle{remote: remote, size: uint64(len(data)), readAhead: newReadAhead(context.Background(), remote, 0, uint64(len(data)), 16<<20)}
	defer h.Release(context.Background())
	var reader fs.FileReader = h
	const count, size = 8, 64 << 10
	for i := 0; i < count; i++ {
		if i > 0 {
			time.Sleep(30 * time.Millisecond)
		}
		result, errno := reader.Read(context.Background(), make([]byte, size), int64(i*size))
		if errno != 0 || result.Size() != size {
			t.Fatalf("paced read %d: %v, %v", i, result, errno)
		}
	}
	waitReadAheadJobs(t, h.readAhead, 0)
	for {
		select {
		case rg := <-requests:
			if rg.end+1 > count*size+(4<<20) {
				t.Fatalf("paced consumer caused excessive read-ahead: %+v", rg)
			}
		default:
			return
		}
	}
}

func TestReadAheadOverlapDoesNotLookLikeSeek(t *testing.T) {
	data := bytes.Repeat([]byte("overlap"), int((4<<20)/7+1))[:4<<20]
	remote, requests, _ := readAheadRemote(t, data, nil)
	ra := newReadAhead(context.Background(), remote, 0, uint64(len(data)), 4<<20)
	ra.observe(0, 64<<10, 0)
	_ = waitRange(t, requests)
	old := ra.ctx
	ra.observe(32<<10, 64<<10, 0) // overlaps and advances the high-water mark
	if old.Err() != nil {
		t.Fatal("small overlapping read cancelled the current prediction generation")
	}
	ra.Close()
}

func TestReadAheadBackwardSeekResetsOnlyAfterLocalLookback(t *testing.T) {
	data := bytes.Repeat([]byte("backward-seek"), int((8<<20)/13+1))[:8<<20]
	remote, requests, _ := readAheadRemote(t, data, nil)
	ra := newReadAhead(context.Background(), remote, 0, uint64(len(data)), 8<<20)
	ra.observe(0, 4096, 0)
	_ = waitRange(t, requests)
	old := ra.ctx
	ra.observe(2<<20, 4096, 0)
	if old.Err() == nil {
		t.Fatal("distant forward seek did not cancel old prediction")
	}
	old = ra.ctx
	ra.observe((2<<20)-(2<<20), 4096, 0)
	if old.Err() == nil {
		t.Fatal("multi-megabyte backward seek did not cancel prior generation")
	}
	ra.mu.Lock()
	frontier, window := ra.frontier, ra.window
	ra.mu.Unlock()
	if frontier != 4096 || window != initialReadAheadBytes {
		t.Fatalf("backward seek state frontier=%d window=%d", frontier, window)
	}
	ra.Close()
}

func TestReadAheadConsumedAccountingDoesNotDoubleCountRereads(t *testing.T) {
	data := bytes.Repeat([]byte("unique-consumption"), int((4<<20)/18+1))[:4<<20]
	remote, requests, _ := readAheadRemote(t, data, nil)
	ra := newReadAhead(context.Background(), remote, 0, uint64(len(data)), 4<<20)
	ra.observe(0, 4096, 0)
	_ = waitRange(t, requests)
	waitReadAheadJobs(t, ra, 0)
	ra.observe(4096, 8192, 0)
	first := ra.snapshot().ConsumedBytes
	ra.observe(4096, 8192, 0)
	second := ra.snapshot().ConsumedBytes
	if first == 0 || second != first {
		t.Fatalf("unique consumed bytes first=%d repeated=%d", first, second)
	}
	if second > ra.snapshot().ScheduledBytes {
		t.Fatalf("consumed bytes %d exceed scheduled %d", second, ra.snapshot().ScheduledBytes)
	}
	ra.Close()
}

func TestReadAheadRetriesFailedHoleOnNextObservation(t *testing.T) {
	data := bytes.Repeat([]byte("retry-hole"), int((4<<20)/10+1))[:4<<20]
	var dataRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var start, end int64
		if _, err := fmt.Sscanf(req.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}
		if start != 0 || end != 0 {
			if dataRequests.Add(1) == 1 {
				http.Error(w, "temporary failure", http.StatusServiceUnavailable)
				return
			}
		}
		w.Header().Set("ETag", `"retry-hole-v1"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
	}))
	t.Cleanup(server.Close)
	cache, err := storage.NewCache(t.TempDir(), 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	remote, err := storage.NewRemote(context.Background(), cache, "read-ahead-retry", int64(len(data)), func(context.Context) (string, error) { return server.URL, nil })
	if err != nil {
		t.Fatal(err)
	}
	ra := newReadAhead(context.Background(), remote, 0, uint64(len(data)), 2<<20)
	ra.observe(0, 4096, 0)
	deadline := time.Now().Add(2 * time.Second)
	for dataRequests.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	waitReadAheadJobs(t, ra, 0)
	ra.observe(0, 4096, 0)
	deadline = time.Now().Add(2 * time.Second)
	for dataRequests.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if dataRequests.Load() < 2 {
		t.Fatal("failed prefetch range was not retried on the next observation")
	}
	waitReadAheadJobs(t, ra, 0)
	ra.mu.Lock()
	complete := false
	for _, rg := range ra.ranges {
		complete = complete || rg.complete
	}
	ra.mu.Unlock()
	if !complete {
		t.Fatal("retry did not complete the previously failed range")
	}
	ra.Close()
}

func TestReadAheadJumpShrinksWindowAndCancelsOldGeneration(t *testing.T) {
	data := bytes.Repeat([]byte("random-jump"), int((32<<20)/11+1))[:32<<20]
	remote, requests, _ := readAheadRemote(t, data, nil)
	ra := newReadAhead(context.Background(), remote, 0, uint64(len(data)), 16<<20)
	ra.observe(0, 4096, 0)
	_ = waitRange(t, requests)
	waitReadAheadJobs(t, ra, 0)
	ra.observe(4096, 4096, 0)
	_ = waitRange(t, requests)
	waitReadAheadJobs(t, ra, 0)
	ra.observe(8192, 4096, 0)
	_ = waitRange(t, requests)
	waitReadAheadJobs(t, ra, 0)
	ra.mu.Lock()
	grown := ra.window
	oldCtx := ra.ctx
	ra.mu.Unlock()
	if grown < 4<<20 || grown > 16<<20 {
		t.Fatalf("grown window = %d, want gradual growth bounded by 16 MiB", grown)
	}
	jump := 26 << 20
	ra.observe(int64(jump), 4096, 0)
	if oldCtx.Err() == nil {
		t.Fatal("random jump did not cancel the previous generation")
	}
	ra.mu.Lock()
	window := ra.window
	ra.mu.Unlock()
	if window != initialReadAheadBytes {
		t.Fatalf("window after jump = %d, want 1 MiB", window)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ra.mu.Lock()
		current := false
		for _, rg := range ra.ranges {
			if rg.generation == ra.generation {
				current = true
				if rg.end-rg.start > initialReadAheadBytes {
					ra.mu.Unlock()
					t.Fatalf("jump prefetch chunk = %d, want at most 1 MiB", rg.end-rg.start)
				}
			}
		}
		ra.mu.Unlock()
		if current {
			break
		}
		time.Sleep(time.Millisecond)
	}
	ra.mu.Lock()
	current := false
	for _, rg := range ra.ranges {
		current = current || rg.generation == ra.generation
	}
	ra.mu.Unlock()
	if !current {
		t.Fatal("new generation did not schedule after cancelling old requests")
	}
	ra.Close()
}

func TestReadAheadCloseCancelsBlockedBackgroundRequest(t *testing.T) {
	data := bytes.Repeat([]byte("blocked-read-ahead"), int((4<<20)/18+1))[:4<<20]
	requestStarted := make(chan struct{})
	requestCanceled := make(chan struct{})
	remote, _, _ := readAheadRemote(t, data, func(req *http.Request, _, _ int64) {
		close(requestStarted)
		<-req.Context().Done()
		close(requestCanceled)
	})
	ra := newReadAhead(context.Background(), remote, 0, uint64(len(data)), 4<<20)
	ra.observe(0, 4096, 0)
	<-requestStarted
	done := make(chan struct{})
	go func() { ra.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close left a blocked background range running")
	}
	select {
	case <-requestCanceled:
	case <-time.After(time.Second):
		t.Fatal("background HTTP context was not canceled")
	}
	if ra.jobs != 0 {
		t.Fatalf("jobs after Close = %d", ra.jobs)
	}
}

func TestReadAheadStaysInsideStoredMember(t *testing.T) {
	data := bytes.Repeat([]byte("member-boundary"), int((8<<20)/15+1))[:8<<20]
	remote, requests, _ := readAheadRemote(t, data, nil)
	base := int64(2<<20) + 123
	memberSize := uint64(1 << 20)
	ra := newReadAhead(context.Background(), remote, base, memberSize, 16<<20)
	ra.observe(0, 4096, 0)
	rg := waitRange(t, requests)
	memberEnd := base + int64(memberSize) - 1
	if rg.start < base || rg.end > memberEnd {
		t.Fatalf("read-ahead range %+v escaped Store member [%d,%d]", rg, base, memberEnd)
	}
	ra.Close()
}

func TestRemoteHandleConstructionDoesNotStartReadAhead(t *testing.T) {
	data := bytes.Repeat([]byte("handle-open"), int((2<<20)/11+1))[:2<<20]
	remote, requests, count := readAheadRemote(t, data, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tree := &Tree{ctx: ctx, opts: defaults(Options{})}
	n := &Node{tree: tree}
	h := n.newRemoteHandle(remote, 0, uint64(len(data)))
	if h.readAhead == nil {
		t.Fatal("remote handle did not receive a read-ahead session")
	}
	select {
	case rg := <-requests:
		t.Fatalf("handle construction started speculative range %+v", rg)
	default:
	}
	if count.Load() != 0 {
		t.Fatalf("handle construction caused %d data ranges", count.Load())
	}
	if errno := h.Release(context.Background()); errno != 0 {
		t.Fatal(errno)
	}
}

var _ fs.FileHandle = (*handle)(nil)
