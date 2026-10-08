package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/workqueue"
)

func waitForDownloads(t *testing.T, cache *Cache, predicate func(DownloadStats) bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if predicate(cache.DownloadStats()) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("download scheduler state not reached: %+v", cache.DownloadStats())
}

func foregroundPriority() *downloadPriority {
	p := &downloadPriority{}
	p.promoted.Store(true)
	return p
}

func TestForegroundDownloadWhileBackgroundTransfersBlocked(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	started := make(chan struct{}, 7)
	data := bytes.Repeat([]byte("x"), 1<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/foreground" && r.Header.Get("Range") != "bytes=0-0" {
			started <- struct{}{}
			<-r.Context().Done()
			return
		}
		w.Header().Set("ETag", "\"stable\"")
		http.ServeContent(w, r, "data", time.Unix(1, 0), bytes.NewReader(data))
	}))
	config := DefaultDownloadConfig()
	config.MaxRequests = 8
	cache, err := NewCacheWithDownloadConfig(t.TempDir(), 16<<20, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); workers.Wait(); srv.Close(); cache.Close() })
	for i := 0; i < 7; i++ {
		url := fmt.Sprintf("%s/%d", srv.URL, i)
		remote, err := NewRemote(ctx, cache, url, int64(len(data)), func(context.Context) (string, error) { return url, nil })
		if err != nil {
			t.Fatal(err)
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			_, _ = remote.ReadAtContext(workqueue.Background(ctx), make([]byte, 1), 0)
		}()
	}
	for i := 0; i < 7; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("background transfer did not start")
		}
	}
	foreground, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	remote, err := NewRemoteContext(ctx, foreground, cache, "foreground", int64(len(data)), func(context.Context) (string, error) { return srv.URL + "/foreground", nil })
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1)
	if _, err := remote.ReadAtContext(foreground, buf, 0); err != nil || buf[0] != 'x' {
		t.Fatalf("foreground download: %q, %v", buf, err)
	}
}

func TestMixedFileCountsStayWithinByteBudgetAndForegroundReserve(t *testing.T) {
	for _, fileCount := range []int{1, 4, 16} {
		t.Run(fmt.Sprintf("files-%d", fileCount), func(t *testing.T) {
			data := bytes.Repeat([]byte("x"), 1<<20)
			started := make(chan struct{}, fileCount)
			continueBackground := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/foreground" || r.Header.Get("Range") == "bytes=0-0" {
					w.Header().Set("ETag", `"stable"`)
					http.ServeContent(w, r, "data", time.Unix(1, 0), bytes.NewReader(data))
					return
				}
				started <- struct{}{}
				select {
				case <-continueBackground:
				case <-r.Context().Done():
					return
				}
				w.Header().Set("ETag", `"stable"`)
				http.ServeContent(w, r, "data", time.Unix(1, 0), bytes.NewReader(data))
			}))
			defer srv.Close()
			config := DownloadConfig{MaxRequests: 8, MaxInFlightBytes: 4 << 20, ForegroundReservedBytes: 1 << 20, RequestChunkBytes: 4 << 20}
			cache, err := NewCacheWithDownloadConfig(t.TempDir(), 32<<20, config)
			if err != nil {
				t.Fatal(err)
			}
			defer cache.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			remotes := make([]*Remote, 0, fileCount)
			for i := 0; i < fileCount; i++ {
				url := fmt.Sprintf("%s/background-%d", srv.URL, i)
				remote, err := NewRemote(ctx, cache, url, int64(len(data)), func(context.Context) (string, error) { return url, nil })
				if err != nil {
					t.Fatal(err)
				}
				remotes = append(remotes, remote)
			}
			var workers sync.WaitGroup
			for _, remote := range remotes {
				workers.Add(1)
				go func(remote *Remote) {
					defer workers.Done()
					_, _ = remote.ReadAtContext(workqueue.Background(ctx), make([]byte, 1), 0)
				}(remote)
			}
			initial := min(fileCount, 3) // the 1 MiB reserve caps background at 3 MiB.
			for i := 0; i < initial; i++ {
				select {
				case <-started:
				case <-time.After(time.Second):
					t.Fatal("background reads did not occupy their byte share")
				}
			}
			foreground, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			foregroundRemote, err := NewRemoteContext(ctx, foreground, cache, "foreground", int64(len(data)), func(context.Context) (string, error) { return srv.URL + "/foreground", nil })
			if err != nil {
				t.Fatal(err)
			}
			if _, err = foregroundRemote.ReadAtContext(foreground, make([]byte, 1), 0); err != nil {
				t.Fatalf("foreground read waited behind background files: %v", err)
			}
			stats := cache.DownloadStats()
			if stats.PeakActiveBytes > config.MaxInFlightBytes {
				t.Fatalf("in-flight bytes exceeded budget: %+v", stats)
			}
			close(continueBackground)
			workers.Wait()
			waitForDownloads(t, cache, func(st DownloadStats) bool {
				return st.ActiveRequests == 0 && st.ActiveBytes == 0 && st.StagingActiveBytes == 0
			})
			if stats = cache.DownloadStats(); stats.ActiveRequests != 0 || stats.ActiveBytes != 0 || stats.PeakActiveBytes > config.MaxInFlightBytes {
				t.Fatalf("downloads did not release budget: %+v", stats)
			}
		})
	}
}

func TestDownloadSchedulerPromotesSharedWaiterAndHonorsByteBudget(t *testing.T) {
	cfg := DownloadConfig{MaxRequests: 3, MaxInFlightBytes: 8, ForegroundReservedBytes: 2, RequestChunkBytes: 8}
	s := newDownloadScheduler(cfg)
	bgPriority := &downloadPriority{}
	bgPriority.promoted.Store(false)
	bg, err := s.Acquire(context.Background(), 6, "archive-A", bgPriority)
	if err != nil {
		t.Fatal(err)
	}
	queuedCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan func(), 1)
	go func() { release, _ := s.Acquire(queuedCtx, 2, "archive-A", bgPriority); got <- release }()
	deadline := time.Now().Add(time.Second)
	for s.snapshot().WaitingBackground != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if stats := s.snapshot(); stats.ActiveBytes != 6 || stats.AvailableBackgroundBytes != 0 {
		t.Fatalf("unexpected active byte accounting: %+v", stats)
	}
	s.promote(bgPriority)
	select {
	case release := <-got:
		if release == nil {
			t.Fatal("promoted waiter failed to acquire reserved capacity")
		}
		release()
	case <-time.After(time.Second):
		t.Fatal("foreground promotion did not wake queued request")
	}
	bg()
	if stats := s.snapshot(); stats.ActiveBytes != 0 || stats.ActiveRequests != 0 || stats.Promotions != 1 {
		t.Fatalf("resources were not returned: %+v", stats)
	}
}

func TestDownloadSchedulerSingleSlotStillAllowsBackground(t *testing.T) {
	s := newDownloadScheduler(DownloadConfig{MaxRequests: 1, MaxInFlightBytes: 4, ForegroundReservedBytes: 1, RequestChunkBytes: 4})
	bg, err := s.Acquire(workqueue.Background(context.Background()), 2, "archive", nil)
	if err != nil {
		t.Fatalf("single-slot scheduler rejected background request: %v", err)
	}
	fgDone := make(chan func(), 1)
	go func() {
		release, _ := s.Acquire(context.Background(), 1, "interactive", foregroundPriority())
		fgDone <- release
	}()
	waitScheduler(t, s, func(st DownloadStats) bool { return st.WaitingForeground == 1 })
	bg()
	select {
	case release := <-fgDone:
		if release == nil {
			t.Fatal("foreground acquire failed")
		}
		release()
	case <-time.After(time.Second):
		t.Fatal("foreground request remained blocked after slot release")
	}
	if stats := s.snapshot(); stats.ActiveRequests != 0 || stats.ActiveBytes != 0 {
		t.Fatalf("single-slot budget leaked: %+v", stats)
	}
}

func TestDownloadSchedulerKeepsQueueAfterCanceledHeadCannotYetFit(t *testing.T) {
	s := newDownloadScheduler(DownloadConfig{MaxRequests: 2, MaxInFlightBytes: 4, ForegroundReservedBytes: 1, RequestChunkBytes: 4})
	active, err := s.Acquire(context.Background(), 4, "foreground", foregroundPriority())
	if err != nil {
		t.Fatal(err)
	}
	canceledCtx, cancel := context.WithCancel(workqueue.Background(context.Background()))
	queuedCtx := workqueue.Background(context.Background())
	canceled := &downloadWaiter{ctx: canceledCtx, bytes: 1, file: "same-file", done: make(chan struct{})}
	queued := &downloadWaiter{ctx: queuedCtx, bytes: 2, file: "same-file", done: make(chan struct{})}
	s.mu.Lock()
	s.enqueueLocked(canceled)
	s.enqueueLocked(queued)
	cancel()
	s.dispatchLocked() // prune the canceled head while active bytes still prevent the next grant
	if s.stats.WaitingBackground != 1 {
		t.Fatalf("remaining waiter lost from queue: %+v", s.stats)
	}
	s.mu.Unlock()
	active()
	select {
	case <-queued.done:
	case <-time.After(time.Second):
		t.Fatal("remaining waiter did not run after capacity returned")
	}
	s.mu.Lock()
	if !queued.granted || s.stats.WaitingBackground != 0 {
		t.Fatalf("bad queue state after grant: %+v", s.stats)
	}
	s.releaseLocked(queued)
	s.mu.Unlock()
}

func TestDownloadSchedulerCancellationAndBackgroundProgress(t *testing.T) {
	s := newDownloadScheduler(DownloadConfig{MaxRequests: 4, MaxInFlightBytes: 32, ForegroundReservedBytes: 8, RequestChunkBytes: 32})
	var releases []func()
	for i := 0; i < 3; i++ {
		release, err := s.Acquire(workqueue.Background(context.Background()), 1, fmt.Sprintf("bg-%d", i), nil)
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	fgActive, err := s.Acquire(context.Background(), 1, "fg-active", foregroundPriority())
	if err != nil {
		t.Fatal(err)
	}
	releases = append(releases, fgActive)
	ctx, cancel := context.WithCancel(context.Background())
	canceled := make(chan error, 1)
	go func() { _, err := s.Acquire(workqueue.Background(ctx), 1, "cancelled-file", nil); canceled <- err }()
	waitScheduler(t, s, func(st DownloadStats) bool { return st.WaitingBackground == 1 })
	cancel()
	if err := <-canceled; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled acquire = %v", err)
	}
	waitScheduler(t, s, func(st DownloadStats) bool { return st.WaitingBackground == 0 })

	queuedFG := make(chan func(), 4)
	foregroundCtx, cancelForeground := context.WithCancel(context.Background())
	defer cancelForeground()
	for i := 0; i < 4; i++ {
		go func(file string) {
			r, _ := s.Acquire(foregroundCtx, 1, file, foregroundPriority())
			queuedFG <- r
		}(fmt.Sprintf("foreground-%d", i))
	}
	bgProgress := make(chan func(), 1)
	go func() {
		r, _ := s.Acquire(workqueue.Background(context.Background()), 1, "background-progress", nil)
		bgProgress <- r
	}()
	waitScheduler(t, s, func(st DownloadStats) bool { return st.WaitingForeground == 4 && st.WaitingBackground == 1 })
	var foregroundReleases []func()
	for i := 0; i < 2; i++ {
		releases[i]()
		select {
		case r := <-queuedFG:
			if r == nil {
				t.Fatal("foreground waiter failed")
			}
			foregroundReleases = append(foregroundReleases, r)
		case <-time.After(time.Second):
			t.Fatalf("foreground waiter starved at %d: %+v", i, s.snapshot())
		}
	}
	releases[2]() // after two foreground grants, let the waiting background run
	select {
	case r := <-bgProgress:
		if r == nil {
			t.Fatal("background waiter failed")
		}
		r()
	case <-time.After(time.Second):
		t.Fatal("background waiter starved under sustained foreground load")
	}
	cancelForeground()
	for i := 0; i < 2; i++ {
		<-queuedFG
	}
	for _, release := range foregroundReleases {
		release()
	}
	releases[3]()
}

func TestDownloadSchedulerBackgroundTurnUsesReserveAndStillRunsLargeBackground(t *testing.T) {
	s := newDownloadScheduler(DownloadConfig{MaxRequests: 4, MaxInFlightBytes: 128, ForegroundReservedBytes: 16, RequestChunkBytes: 128})
	activeBG, err := s.Acquire(workqueue.Background(context.Background()), 112, "active-background", nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		release, acquireErr := s.Acquire(context.Background(), 1, fmt.Sprintf("burst-%d", i), foregroundPriority())
		if acquireErr != nil {
			t.Fatal(acquireErr)
		}
		release()
	}
	queuedBG := make(chan func(), 1)
	go func() {
		release, _ := s.Acquire(workqueue.Background(context.Background()), 112, "queued-background", nil)
		queuedBG <- release
	}()
	waitScheduler(t, s, func(st DownloadStats) bool { return st.WaitingBackground == 1 })

	firstFG := make(chan func(), 1)
	go func() {
		release, _ := s.Acquire(context.Background(), 8, "foreground-reserve", foregroundPriority())
		firstFG <- release
	}()
	var releaseFG func()
	select {
	case releaseFG = <-firstFG:
		if releaseFG == nil {
			t.Fatal("foreground reserve request failed")
		}
	case <-time.After(time.Second):
		t.Fatal("background fairness turn blocked foreground use of its reserved bytes")
	}
	secondFG := make(chan func(), 1)
	go func() {
		release, _ := s.Acquire(context.Background(), 9, "foreground-over-reserve", foregroundPriority())
		secondFG <- release
	}()
	waitScheduler(t, s, func(st DownloadStats) bool { return st.WaitingForeground == 1 && st.WaitingBackground == 1 })
	select {
	case <-secondFG:
		t.Fatal("foreground request consumed bytes needed beyond its reserve")
	default:
	}

	activeBG()
	var releaseQueuedBG func()
	select {
	case releaseQueuedBG = <-queuedBG:
		if releaseQueuedBG == nil {
			t.Fatal("large queued background request failed")
		}
	case <-time.After(time.Second):
		t.Fatalf("large background request starved after capacity returned: %+v", s.snapshot())
	}
	releaseQueuedBG()
	select {
	case release := <-secondFG:
		if release == nil {
			t.Fatal("foreground request failed after background completed")
		}
		release()
	case <-time.After(time.Second):
		t.Fatal("foreground queue did not resume after background completed")
	}
	releaseFG()
	if st := s.snapshot(); st.ActiveRequests != 0 || st.ActiveBytes != 0 || st.WaitingForeground != 0 || st.WaitingBackground != 0 {
		t.Fatalf("scheduler resources leaked: %+v", st)
	}
}

func TestRemoteSplitsOnlyRangesLargerThanBudget(t *testing.T) {
	data := []byte("123456789")
	var mu sync.Mutex
	var ranges []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ranges = append(ranges, r.Header.Get("Range"))
		mu.Unlock()
		w.Header().Set("ETag", `"stable"`)
		http.ServeContent(w, r, "data", time.Unix(1, 0), bytes.NewReader(data))
	}))
	defer srv.Close()
	cfg := DownloadConfig{MaxRequests: 4, MaxInFlightBytes: 4, ForegroundReservedBytes: 1, RequestChunkBytes: 4}
	cache, err := NewCacheWithDownloadConfig(t.TempDir(), 32, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	remote, err := NewRemote(context.Background(), cache, srv.URL, int64(len(data)), func(context.Context) (string, error) { return srv.URL, nil })
	if err != nil {
		t.Fatal(err)
	}
	if err = remote.PrefetchRangeAtContext(context.Background(), 0, int64(len(data))); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := append([]string(nil), ranges...)
	mu.Unlock()
	// The one-byte probe is followed by 4/4/1-byte requests. A foreground task
	// can use the full byte budget, while larger tasks are segmented.
	if len(got) != 4 || got[0] != "bytes=0-0" || got[1] != "bytes=0-3" || got[2] != "bytes=4-7" || got[3] != "bytes=8-8" {
		t.Fatalf("range requests = %v", got)
	}
	if stats := cache.DownloadStats(); stats.PeakActiveBytes > cfg.MaxInFlightBytes {
		t.Fatalf("byte budget exceeded: %+v", stats)
	}
}

func TestCanceledRangeOwnerDoesNotCancelSharedForegroundReader(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 64<<10)
	started := make(chan struct{}, 1)
	continueResponse := make(chan struct{})
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "bytes=0-0" {
			requests.Add(1)
			started <- struct{}{}
			select {
			case <-continueResponse:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("ETag", `"stable"`)
		http.ServeContent(w, r, "data", time.Unix(1, 0), bytes.NewReader(data))
	}))
	defer srv.Close()
	cache, err := NewCache(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	lifetime, stop := context.WithCancel(context.Background())
	defer stop()
	remote, err := NewRemoteContext(lifetime, lifetime, cache, srv.URL, int64(len(data)), func(context.Context) (string, error) { return srv.URL, nil })
	if err != nil {
		t.Fatal(err)
	}
	ownerCtx, cancelOwner := context.WithCancel(workqueue.Background(context.Background()))
	ownerDone := make(chan error, 1)
	go func() { _, err := remote.ReadAtContext(ownerCtx, make([]byte, 8), 0); ownerDone <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("range request did not start")
	}
	readerDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 8)
		_, err := remote.ReadAtContext(context.Background(), buf, 0)
		if err == nil && string(buf) != string(bytes.Repeat([]byte("x"), 8)) {
			err = fmt.Errorf("unexpected data %q", buf)
		}
		readerDone <- err
	}()
	waitForDownloads(t, cache, func(st DownloadStats) bool { return st.Promotions == 1 })
	cancelOwner()
	if err := <-ownerDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("owner read = %v, want cancellation", err)
	}
	close(continueResponse)
	select {
	case err := <-readerDone:
		if err != nil {
			t.Fatalf("shared reader failed after owner cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("shared foreground read did not finish")
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("shared range issued %d HTTP requests, want 1", got)
	}
}

func TestCacheCloseCancelsAndWaitsForActiveRangeRequest(t *testing.T) {
	started := make(chan struct{}, 1)
	serverCanceled := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "bytes=0-0" {
			w.Header().Set("ETag", `"stable"`)
			w.Header().Set("Content-Range", "bytes 0-0/2")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte("x"))
			return
		}
		started <- struct{}{}
		<-r.Context().Done()
		serverCanceled <- struct{}{}
	}))
	defer srv.Close()
	cache, err := NewCache(t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := NewRemote(context.Background(), cache, "close", 2, func(context.Context) (string, error) { return srv.URL, nil })
	if err != nil {
		t.Fatal(err)
	}
	readDone := make(chan error, 1)
	go func() { _, err := remote.ReadAt(make([]byte, 1), 0); readDone <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("range request did not start")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- cache.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("cache close failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cache close did not wait for canceled request cleanup")
	}
	select {
	case <-serverCanceled:
	case <-time.After(time.Second):
		t.Fatal("cache close did not cancel the HTTP request")
	}
	if err := <-readDone; err == nil {
		t.Fatal("read succeeded after cache close")
	}
	if stats := cache.DownloadStats(); stats.ActiveRequests != 0 || stats.ActiveBytes != 0 {
		t.Fatalf("cache close leaked transfer resources: %+v", stats)
	}
}

func waitScheduler(t *testing.T, s *downloadScheduler, predicate func(DownloadStats) bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if predicate(s.snapshot()) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("scheduler state not reached: %+v", s.snapshot())
}
