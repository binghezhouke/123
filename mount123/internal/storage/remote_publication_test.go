package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestRemoteConcurrentRangeReadsHandoffCacheCapacity(t *testing.T) {
	testRemoteConcurrentReadsHandoffCacheCapacity(t, false, false)
}

func TestRemoteConcurrentReadAheadHandoffCacheCapacity(t *testing.T) {
	testRemoteConcurrentReadsHandoffCacheCapacity(t, true, false)
}

func TestRemoteReadAheadJoinsShorterRangeWithoutWaitingOnItself(t *testing.T) {
	testRemoteConcurrentReadsHandoffCacheCapacity(t, true, true)
}

func TestRemoteConcurrentSmallCacheReadAheadHandoffCacheCapacity(t *testing.T) {
	testRemoteConcurrentReadsHandoffCacheCapacity(t, true, false, 2*remoteCachePageSize)
}

func TestRemoteConcurrentReadAheadHandoffsActualTailFlight(t *testing.T) {
	old := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(old)
	const capacity = remoteBlockSize
	data := append(bytes.Repeat([]byte("a"), int(capacity)), bytes.Repeat([]byte("b"), int(capacity))...)
	c, r := newRemotePublicationTest(t, "range-short-tail-handoff", data, capacity)
	firstStarted, firstRelease, secondRelease := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var firstOnce, secondOnce sync.Once
	releaseFirst := func() { firstOnce.Do(func() { close(firstRelease) }) }
	releaseSecond := func() { secondOnce.Do(func() { close(secondRelease) }) }
	defer releaseFirst()
	defer releaseSecond()
	var calls atomic.Int32
	c.syncFile = func(f *os.File) error {
		switch calls.Add(1) {
		case 1:
			close(firstStarted)
			<-firstRelease
		case 2:
			<-secondRelease
		}
		return f.Sync()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 3)
	read := func(short bool) {
		want := len(data)
		if short {
			want = int(capacity / 2)
		}
		out := make([]byte, want)
		var n int
		var err error
		if short {
			n, err = r.ReadRangeAtContext(ctx, out, 0)
		} else {
			n, err = r.ReadAtContext(ctx, out, 0)
		}
		if err == nil && (n != want || !bytes.Equal(out, data[:want])) {
			err = fmt.Errorf("read %d/%d, bytes match=%v", n, want, bytes.Equal(out, data[:want]))
		}
		done <- err
	}
	go read(true)
	select {
	case <-firstStarted:
	case <-ctx.Done():
		t.Fatal("short first flight did not reach publication")
	}
	go read(false)
	go read(false)
	waitRemotePublicationReaders(t, ctx, c, r, 3, 0)
	releaseFirst()
	waitRemotePublicationReaders(t, ctx, c, r, 2, capacity/2)
	releaseSecond()
	for i := 0; i < 3; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-ctx.Done():
			t.Fatal("short-tail shared read did not finish")
		}
	}
	assertRemotePublicationResourcesReleased(t, c)
}

func testRemoteConcurrentReadsHandoffCacheCapacity(t *testing.T, readAhead, shortFirst bool, capacityArg ...int64) {
	t.Helper()
	old := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(old)
	capacity := remoteCachePageSize
	if readAhead {
		capacity = remoteBlockSize
	}
	if len(capacityArg) > 0 {
		capacity = capacityArg[0]
	}
	dataSize := 2 * capacity
	if readAhead {
		dataSize = 2 * remoteBlockSize
	}
	data := append(bytes.Repeat([]byte("a"), int(capacity)), bytes.Repeat([]byte("b"), int(dataSize-capacity))...)
	c, r := newRemotePublicationTest(t, "range-concurrent-handoff", data, capacity)
	publicationStarted, releasePublication := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releasePublication) }) }
	defer release()
	var calls atomic.Int32
	c.syncFile = func(f *os.File) error {
		if calls.Add(1) == 1 {
			close(publicationStarted)
			<-releasePublication
		}
		return f.Sync()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 2)
	read := func(short bool) {
		want := len(data)
		if short {
			want = int(capacity / 2)
		}
		out := make([]byte, want)
		var n int
		var err error
		if readAhead && !short {
			n, err = r.ReadAtContext(ctx, out, 0)
		} else {
			n, err = r.ReadRangeAtContext(ctx, out, 0)
		}
		if err == nil && (n != len(out) || !bytes.Equal(out, data[:want])) {
			err = fmt.Errorf("read %d/%d, bytes match=%v", n, len(out), bytes.Equal(out, data[:want]))
		}
		done <- err
	}
	go read(shortFirst)
	select {
	case <-publicationStarted:
	case <-ctx.Done():
		t.Fatal("first range did not reach publication")
	}
	go read(false)
	waitRemotePublicationReaders(t, ctx, c, r, 2)
	release()
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-ctx.Done():
			t.Fatal("concurrent range read did not finish")
		}
	}
	assertRemotePublicationResourcesReleased(t, c)
}

func waitRemotePublicationReaders(t *testing.T, ctx context.Context, c *Cache, r *Remote, refs int, startArg ...int64) {
	t.Helper()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	joined := false
	for !joined {
		c.mu.Lock()
		for _, f := range c.rangeFlights[r.rangeID] {
			joined = joined || (f.refs >= refs && (len(startArg) == 0 || f.start == startArg[0]))
		}
		c.mu.Unlock()
		if !joined {
			select {
			case <-ticker.C:
			case <-ctx.Done():
				t.Fatal("readers did not join the same flight")
			}
		}
	}
}

func TestRemoteRangeHandoffDoesNotWaitForExplicitWindow(t *testing.T) {
	data := bytes.Repeat([]byte("x"), int(2*remoteCachePageSize))
	c, r := newRemotePublicationTest(t, "range-window-handoff", data)
	releasePublication := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releasePublication) }) }
	defer release()
	c.syncFile = func(f *os.File) error { <-releasePublication; return f.Sync() }
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	w, err := r.OpenRangeWindow(ctx, 0, remoteCachePageSize)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	done := make(chan error, 1)
	go func() {
		n, err := r.ReadRangeAtContext(ctx, make([]byte, len(data)), 0)
		if !errors.Is(err, syscall.ENOSPC) || n != int(remoteCachePageSize) {
			done <- fmt.Errorf("window-held capacity read=%d/%v, want %d/ENOSPC", n, err, remoteCachePageSize)
			return
		}
		done <- nil
	}()
	waitRemotePublicationReaders(t, ctx, c, r, 2)
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("ordinary reader waited for explicit window Close")
	}
	if err := w.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	w.Close()
	assertRemotePublicationResourcesReleased(t, c)
}

// A staged prefix can be returned before its response is durably published.
// Its reservation must not make the next chunk of that same read fail ENOSPC.
func TestRemoteRangeReadWaitsForPreviousChunkPublication(t *testing.T) {
	data := append(bytes.Repeat([]byte("a"), int(remoteCachePageSize)), bytes.Repeat([]byte("b"), int(remoteCachePageSize))...)
	c, r := newRemotePublicationTest(t, "range-publication", data)
	publicationStarted, releasePublication := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releasePublication) }) }
	defer release()
	var syncCalls atomic.Int32
	c.syncFile = func(f *os.File) error {
		if syncCalls.Add(1) == 1 {
			close(publicationStarted)
			<-releasePublication
		}
		return f.Sync()
	}
	type result struct {
		n   int
		err error
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out := make([]byte, len(data))
	readDone := make(chan result, 1)
	go func() {
		n, err := r.ReadRangeAtContext(ctx, out, 0)
		readDone <- result{n, err}
	}()
	select {
	case <-publicationStarted:
	case <-ctx.Done():
		t.Fatal("first chunk did not reach publication")
	}
	// The gate deterministically holds the first reservation, rather than
	// relying on disk speed to race the next chunk's admission. A short bounded
	// observation only checks that a valid read does not return a premature error.
	select {
	case got := <-readDone:
		release()
		t.Fatalf("range read returned before prior chunk publication: %d/%d, %v", got.n, len(out), got.err)
	case <-time.After(20 * time.Millisecond):
	}
	release()
	select {
	case got := <-readDone:
		if got.err != nil || got.n != len(out) || !bytes.Equal(out, data) {
			t.Fatalf("range read = %d/%d, %v; bytes match=%v", got.n, len(out), got.err, bytes.Equal(out, data))
		}
	case <-ctx.Done():
		t.Fatal("range read did not resume after publication")
	}
	assertRemotePublicationResourcesReleased(t, c)
}

func TestRemoteExactRangeReportsPublicationFailure(t *testing.T) {
	data := bytes.Repeat([]byte("x"), int(remoteCachePageSize))
	c, r := newRemotePublicationTest(t, "range-publication-failure", data)
	publicationStarted, releasePublication := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releasePublication) }) }
	defer release()
	publicationErr := errors.New("cache publication failed")
	c.syncFile = func(*os.File) error {
		close(publicationStarted)
		<-releasePublication
		return publicationErr
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out := make([]byte, len(data))
	readDone := make(chan error, 1)
	go func() {
		_, err := r.ReadRangeAtContext(ctx, out, 0)
		readDone <- err
	}()
	select {
	case <-publicationStarted:
	case <-ctx.Done():
		t.Fatal("complete range did not reach publication")
	}
	release()
	select {
	case got := <-readDone:
		if !errors.Is(got, publicationErr) {
			t.Fatalf("complete range error = %v, want publication error %v", got, publicationErr)
		}
	case <-ctx.Done():
		t.Fatal("range read did not return publication failure")
	}
}

func TestRemoteExactRangeCancellationDuringPublication(t *testing.T) {
	data := bytes.Repeat([]byte("x"), int(remoteCachePageSize))
	c, r := newRemotePublicationTest(t, "range-publication-cancel", data)
	publicationStarted, releasePublication := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releasePublication) }) }
	defer release()
	c.syncFile = func(f *os.File) error {
		close(publicationStarted)
		<-releasePublication
		return f.Sync()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	readDone := make(chan error, 1)
	go func() {
		_, err := r.ReadRangeAtContext(ctx, make([]byte, len(data)), 0)
		readDone <- err
	}()
	select {
	case <-publicationStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("complete range did not reach publication")
	}
	cancel()
	select {
	case got := <-readDone:
		if !errors.Is(got, context.Canceled) {
			t.Fatalf("canceled range error = %v, want context.Canceled", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled range read waited for blocked publication")
	}
	// Close waits for the producer after the canceled reader has released its
	// reference. Start it while publication is blocked to exercise that handoff.
	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	select {
	case <-c.lifetimeCtx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("cache Close did not begin")
	}
	release()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cache Close did not finish after publication resumed")
	}
	assertRemotePublicationResourcesReleased(t, c)
}

func newRemotePublicationTest(t *testing.T, key string, data []byte, capacityArg ...int64) (*Cache, *Remote) {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var start, end int64
		if _, err := fmt.Sscanf(req.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}
		w.Header().Set("ETag", strconv.Quote(key))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
	}))
	t.Cleanup(s.Close)
	capacity := remoteCachePageSize
	if len(capacityArg) > 0 {
		capacity = capacityArg[0]
	}
	c, err := NewCache(t.TempDir(), capacity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	r, err := NewRemote(context.Background(), c, key, int64(len(data)), func(context.Context) (string, error) { return s.URL, nil })
	if err != nil {
		t.Fatal(err)
	}
	return c, r
}

func assertRemotePublicationResourcesReleased(t *testing.T, c *Cache) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reserved != 0 {
		t.Fatalf("cache retained %d reserved bytes after completed read", c.reserved)
	}
	for id, entry := range c.entries {
		if entry.pins != 0 {
			t.Fatalf("published extent %s retained %d pins after completed read", id, entry.pins)
		}
	}
}
