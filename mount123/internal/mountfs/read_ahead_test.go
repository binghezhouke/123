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
	// A second sequential read promotes the window from 1 MiB to 4 MiB.
	second := make([]byte, 64<<10)
	if result, errno := h.Read(context.Background(), second, 64<<10); errno != 0 || result.Size() != len(second) {
		t.Fatalf("second read = %v, %v", result, errno)
	}
	seenLarge := false
	deadline := time.After(3 * time.Second)
	for !seenLarge {
		select {
		case rg := <-requests:
			if rg.end-rg.start+1 >= 3<<20 {
				seenLarge = true
			}
		case <-deadline:
			t.Fatal("4 MiB sequential read-ahead range did not start")
		}
	}
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
	ra.observe(0, 4096)
	ra.observe(4096, 4096)
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

func TestReadAheadJumpShrinksWindowAndCancelsOldGeneration(t *testing.T) {
	data := bytes.Repeat([]byte("random-jump"), int((32<<20)/11+1))[:32<<20]
	remote, requests, _ := readAheadRemote(t, data, nil)
	ra := newReadAhead(context.Background(), remote, 0, uint64(len(data)), 16<<20)
	ra.observe(0, 4096)
	_ = waitRange(t, requests)
	waitReadAheadJobs(t, ra, 0)
	ra.observe(4096, 4096)
	_ = waitRange(t, requests)
	waitReadAheadJobs(t, ra, 0)
	ra.observe(8192, 4096)
	_ = waitRange(t, requests)
	waitReadAheadJobs(t, ra, 0)
	ra.mu.Lock()
	grown := ra.window
	oldCtx := ra.ctx
	ra.mu.Unlock()
	if grown != 16<<20 {
		t.Fatalf("grown window = %d, want 16 MiB", grown)
	}
	jump := 26 << 20
	ra.observe(int64(jump), 4096)
	if oldCtx.Err() == nil {
		t.Fatal("random jump did not cancel the previous generation")
	}
	ra.mu.Lock()
	window := ra.window
	ra.mu.Unlock()
	if window != initialReadAheadBytes {
		t.Fatalf("window after jump = %d, want 1 MiB", window)
	}
	jumpRange := waitRange(t, requests)
	if got := jumpRange.end - jumpRange.start + 1; got > initialReadAheadBytes {
		t.Fatalf("jump prefetch size = %d, want at most 1 MiB", got)
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
	ra.observe(0, 4096)
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
	ra.observe(0, 4096)
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
