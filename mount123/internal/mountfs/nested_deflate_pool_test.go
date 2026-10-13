package mountfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
)

type deflatePoolCloseTracker struct {
	calls atomic.Int64
	done  chan struct{}
}

func (c *deflatePoolCloseTracker) Close() error {
	if c.calls.Add(1) == 1 && c.done != nil {
		close(c.done)
	}
	return nil
}

type deflatePoolStream struct {
	io.Reader
	closer *deflatePoolCloseTracker
}

func (r *deflatePoolStream) Close() error { return r.closer.Close() }
func deflatePoolFixture(t *testing.T) (*storage.Cache, *nestedArchive, *deflatePoolCloseTracker) {
	t.Helper()
	cache, err := storage.NewCache(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cache.Close() })
	released := &deflatePoolCloseTracker{done: make(chan struct{})}
	parent := &nestedArchive{refs: 1, format: ".zip", indexBytes: 100, index: &zipIndex{prepared: &preparedArchiveInput{input: &storage.Remote{}}}, lazyInputs: []io.Closer{released}}
	return cache, parent, released
}
func deflatePoolReader(cache *storage.Cache, key string, ctx context.Context, open func(context.Context) (io.ReadCloser, error)) *nestedMemberReader {
	return &nestedMemberReader{ctx: ctx, cache: cache, key: key, size: 128, blockBytes: 16, gate: func(ctx context.Context) (func(), error) { return func() {}, ctx.Err() }, open: open}
}
func deflatePoolPlainReader(cache *storage.Cache, key string, closed *deflatePoolCloseTracker) *nestedMemberReader {
	return deflatePoolReader(cache, key, context.Background(), func(context.Context) (io.ReadCloser, error) {
		return &deflatePoolStream{Reader: bytes.NewReader(bytes.Repeat([]byte("x"), 128)), closer: closed}, nil
	})
}
func deflatePoolRead(t *testing.T, l *nestedDeflateLease, off int64) {
	t.Helper()
	p := make([]byte, 4)
	if n, e := l.ReadAtContext(context.Background(), p, off); n != 4 || e != nil || string(p) != "xxxx" {
		t.Fatalf("read n=%d err=%v bytes=%q", n, e, p)
	}
}
func waitDeflatePoolClose(t *testing.T, c *deflatePoolCloseTracker) {
	t.Helper()
	select {
	case <-c.done:
	case <-time.After(2 * time.Second):
		t.Fatal("retained parent did not close")
	}
}

func TestNestedDeflatePoolBoundsAndParentOwnership(t *testing.T) {
	for _, limits := range []struct {
		name  string
		items int
		bytes int64
	}{{"items", 2, 1000}, {"bytes", 8, 25}} {
		t.Run(limits.name, func(t *testing.T) {
			cache, _, _ := deflatePoolFixture(t)
			pool := newNestedDeflatePool(limits.items, limits.bytes, time.Hour)
			defer pool.Close()
			parents := make([]*nestedArchive, 3)
			resources := make([]*deflatePoolCloseTracker, 3)
			streams := make([]*deflatePoolCloseTracker, 3)
			for i, key := range []string{"old", "recent", "new"} {
				resources[i] = &deflatePoolCloseTracker{done: make(chan struct{})}
				parents[i] = &nestedArchive{refs: 1, lazyInputs: []io.Closer{resources[i]}}
				streams[i] = &deflatePoolCloseTracker{}
				lease, ok := pool.acquire(context.Background(), key, 10, parents[i], deflatePoolPlainReader(cache, key, streams[i]))
				if !ok {
					t.Fatal("bounded entry admission failed")
				}
				deflatePoolRead(t, lease, 0)
				parents[i].Close()
				lease.Close()
			}
			waitDeflatePoolClose(t, resources[0])
			if streams[0].calls.Load() != 1 {
				t.Fatal("evicted decoder did not close exactly once")
			}
			select {
			case <-resources[1].done:
				t.Fatal("recent parent unexpectedly evicted")
			default:
			}
			pool.Close()
			pool.Close()
			for i := range parents {
				waitDeflatePoolClose(t, resources[i])
				if resources[i].calls.Load() != 1 || streams[i].calls.Load() != 1 {
					t.Fatal("duplicate close or leaked parent/decoder")
				}
			}
		})
	}
}

func waitDeflatePoolEntered(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("source did not start")
	}
}

func TestNestedDeflatePoolBudgetBusyAndClosedFallback(t *testing.T) {
	cache, parent, _ := deflatePoolFixture(t)
	pool := newNestedDeflatePool(1, 10, time.Hour)
	defer pool.Close()
	tracker := &deflatePoolCloseTracker{}
	lease, ok := pool.acquire(context.Background(), "busy", 10, parent, deflatePoolPlainReader(cache, "busy", tracker))
	if !ok {
		t.Fatal("initial admission failed")
	}
	deflatePoolRead(t, lease, 0)
	for _, trial := range []struct {
		key  string
		cost int64
	}{{"busy", 10}, {"other", 10}, {"oversize", 11}} {
		candidate := deflatePoolPlainReader(cache, trial.key, &deflatePoolCloseTracker{})
		if _, ok := pool.acquire(context.Background(), trial.key, trial.cost, parent, candidate); ok {
			t.Fatal("busy or oversized entry admitted")
		}
		candidate.Close()
		deflatePoolRead(t, lease, 0)
		if tracker.calls.Load() != 0 {
			t.Fatal("active entry was evicted to admit fallback")
		}
	}
	pool.Close()
	candidate := deflatePoolPlainReader(cache, "closed", &deflatePoolCloseTracker{})
	if _, ok := pool.acquire(context.Background(), "closed", 10, parent, candidate); ok {
		t.Fatal("closed pool admitted entry")
	}
	candidate.Close()
	lease.Close()
	if tracker.calls.Load() != 1 {
		t.Fatal("active entry was not closed once")
	}
	parent.mu.Lock()
	refs := parent.refs
	parent.mu.Unlock()
	if refs != 1 {
		t.Fatalf("fallback leaked parent refs=%d", refs)
	}
}

func TestNestedDeflatePoolIdleGeneration(t *testing.T) {
	cache, parent, released := deflatePoolFixture(t)
	pool := newNestedDeflatePool(2, 100, time.Hour)
	defer pool.Close()
	stream := &deflatePoolCloseTracker{}
	first, ok := pool.acquire(context.Background(), "same", 10, parent, deflatePoolPlainReader(cache, "same", stream))
	if !ok {
		t.Fatal("admit")
	}
	deflatePoolRead(t, first, 0)
	first.Close()
	old := first.entry.generation
	next, ok := pool.acquire(context.Background(), "same", 10, parent, deflatePoolPlainReader(cache, "same", &deflatePoolCloseTracker{}))
	if !ok {
		t.Fatal("borrow")
	}
	pool.expire(first.entry, old)
	deflatePoolRead(t, next, 16)
	next.Close()
	pool.expire(first.entry, old)
	if stream.calls.Load() != 0 {
		t.Fatal("obsolete queued timer closed a newer continuation")
	}
	parent.Close()
	pool.expire(next.entry, next.entry.generation)
	waitDeflatePoolClose(t, released)
	if stream.calls.Load() != 1 {
		t.Fatal("current idle expiry did not close decoder")
	}
}

func TestNestedDeflatePoolRealIdleExpiry(t *testing.T) {
	cache, parent, released := deflatePoolFixture(t)
	pool := newNestedDeflatePool(1, 100, 20*time.Millisecond)
	defer pool.Close()
	lease, ok := pool.acquire(context.Background(), "idle", 10, parent, deflatePoolPlainReader(cache, "idle", &deflatePoolCloseTracker{}))
	if !ok {
		t.Fatal("admit")
	}
	deflatePoolRead(t, lease, 0)
	parent.Close()
	lease.Close()
	waitDeflatePoolClose(t, released)
}

type deflatePoolBlockingReader struct {
	ctx     context.Context
	entered chan struct{}
	resume  chan struct{}
	once    sync.Once
	body    *bytes.Reader
}

func (r *deflatePoolBlockingReader) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.entered) })
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	case <-r.resume:
		return r.body.Read(p)
	}
}

func TestNestedDeflatePoolCancelledWaiterDoesNotResetActiveRead(t *testing.T) {
	cache, parent, _ := deflatePoolFixture(t)
	pool := newNestedDeflatePool(2, 100, time.Hour)
	defer pool.Close()
	entered, resume := make(chan struct{}), make(chan struct{})
	var resumeOnce sync.Once
	unblock := func() { resumeOnce.Do(func() { close(resume) }) }
	defer unblock()
	stream := &deflatePoolCloseTracker{}
	reader := deflatePoolReader(cache, "blocking", context.Background(), func(ctx context.Context) (io.ReadCloser, error) {
		return &deflatePoolStream{Reader: &deflatePoolBlockingReader{ctx: ctx, entered: entered, resume: resume, body: bytes.NewReader(bytes.Repeat([]byte("x"), 128))}, closer: stream}, nil
	})
	lease, ok := pool.acquire(context.Background(), "blocking", 10, parent, reader)
	if !ok {
		t.Fatal("admit")
	}
	if err := cache.Store(context.Background(), "blocking:block:0", bytes.Repeat([]byte("x"), 16)); err != nil {
		t.Fatal(err)
	}
	active := make(chan error, 1)
	go func() { p := make([]byte, 4); _, e := lease.ReadAtContext(context.Background(), p, 16); active <- e }()
	waitDeflatePoolEntered(t, entered)
	candidate := deflatePoolPlainReader(cache, "blocking", &deflatePoolCloseTracker{})
	if _, ok := pool.acquire(context.Background(), "blocking", 10, parent, candidate); ok {
		t.Fatal("busy entry unexpectedly borrowed")
	}
	hit := make(chan error, 1)
	go func() {
		p := make([]byte, 4)
		n, e := candidate.ReadAtContext(context.Background(), p, 0)
		if e == nil && (n != 4 || string(p) != "xxxx") {
			e = errors.New("wrong cached bytes")
		}
		hit <- e
	}()
	select {
	case e := <-hit:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("complete cache hit waited on busy decoder")
	}
	candidate.Close()
	sameHit := make(chan error, 1)
	go func() {
		p := make([]byte, 4)
		n, e := lease.ReadAtContext(context.Background(), p, 0)
		if e == nil && (n != 4 || string(p) != "xxxx") {
			e = errors.New("wrong same-lease cached bytes")
		}
		sameHit <- e
	}()
	select {
	case e := <-sameHit:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("same-lease cached hit waited for active decoder")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	waiter := make(chan error, 1)
	go func() { _, e := lease.ReadAtContext(cancelled, make([]byte, 4), 16); waiter <- e }()
	select {
	case e := <-waiter:
		if !errors.Is(e, context.Canceled) {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled waiter blocked on another decoder")
	}
	unblock()
	if e := <-active; e != nil {
		t.Fatal(e)
	}
	deflatePoolRead(t, lease, 16)
	if stream.calls.Load() != 0 {
		t.Fatal("cancelled waiter reset healthy continuation")
	}
	lease.Close()
}

func TestNestedDeflatePoolTreeCancelActiveAndRecursiveParentRelease(t *testing.T) {
	cache, parent, released := deflatePoolFixture(t)
	lifetime, cancel := context.WithCancel(context.Background())
	pool := newNestedDeflatePool(4, 100, time.Hour)
	defer pool.Close()
	defer cancel()
	context.AfterFunc(lifetime, pool.Close)
	outer, ok := pool.acquire(context.Background(), "outer", 10, parent, deflatePoolPlainReader(cache, "outer", &deflatePoolCloseTracker{}))
	if !ok {
		t.Fatal("outer admit")
	}
	deflatePoolRead(t, outer, 0)
	releaseParent, e := parent.retain()
	if e != nil {
		t.Fatal(e)
	}
	child := &nestedArchive{refs: 1, parent: parent, parentRelease: releaseParent, lazyInputs: []io.Closer{outer}}
	entered, resume := make(chan struct{}), make(chan struct{})
	reader := deflatePoolReader(cache, "leaf", lifetime, func(ctx context.Context) (io.ReadCloser, error) {
		return &deflatePoolStream{Reader: &deflatePoolBlockingReader{ctx: ctx, entered: entered, resume: resume, body: bytes.NewReader(bytes.Repeat([]byte("x"), 128))}, closer: &deflatePoolCloseTracker{}}, nil
	})
	leaf, ok := pool.acquire(context.Background(), "leaf", 10, child, reader)
	if !ok {
		t.Fatal("leaf admit")
	}
	parent.Close()
	child.Close()
	finished := make(chan error, 1)
	go func() { _, e := leaf.ReadAtContext(context.Background(), make([]byte, 4), 0); finished <- e }()
	waitDeflatePoolEntered(t, entered)
	cancel()
	select {
	case e := <-finished:
		if !errors.Is(e, context.Canceled) {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("Tree cancel did not interrupt active source")
	}
	waitDeflatePoolClose(t, released)
	leaf.Close()
	outer.Close()
	parent.mu.Lock()
	refs, closed := parent.refs, parent.closed
	parent.mu.Unlock()
	if refs != 0 || !closed {
		t.Fatalf("parent refs=%d closed=%v", refs, closed)
	}
}

func TestNestedDeflatePoolEligibilityChargesVolumesAndRejectsHiddenDecoders(t *testing.T) {
	root := &nestedArchive{format: ".zip", indexBytes: 100, index: &zipIndex{prepared: &preparedArchiveInput{input: &storage.Remote{}}}}
	m := &member{method: 8}
	one, ok := nestedDeflateResidentBytes(root, m)
	if !ok {
		t.Fatal("clear root not eligible")
	}
	root.index.prepared = &preparedArchiveInput{input: &volumeReaderAt{parts: make([]panapi.File, 3), sizes: []int64{1, 1, 1}}, layout: archiveVolumeLayout{sizes: []int64{1, 1, 1}}}
	three, ok := nestedDeflateResidentBytes(root, m)
	if !ok || three-one != 2*nestedDeflateResidentUnit {
		t.Fatal("known volumes not charged individually")
	}
	child := &nestedArchive{format: ".zip", parent: root, index: &zipIndex{}, indexBytes: 200, lazyInputs: []io.Closer{&nestedVolumeInput{parent: root, member: &member{method: 8}}, &nestedVolumeInput{parent: root, member: &member{method: 0}}}}
	if cost, ok := nestedDeflateResidentBytes(child, m); !ok || cost-three != 2*nestedDeflateResidentUnit+100 {
		t.Fatal("nested inputs/index chain not charged")
	}
	for _, bad := range []*member{{method: 8, encrypted: true}, {method: 8, flags: 1}, {method: 8, aes: &aesMemberInfo{}}, {method: 99}} {
		child.lazyInputs = []io.Closer{&nestedVolumeInput{parent: root, member: bad}}
		if _, ok := nestedDeflateResidentBytes(child, m); ok {
			t.Fatal("unsupported ancestor encoding eligible")
		}
	}
	root.index.prepared.input = archivePlainInput{reader: bytes.NewReader(nil)}
	if _, ok := nestedDeflateResidentBytes(root, m); ok {
		t.Fatal("arbitrary root adapter can hide unbounded decoder")
	}
	root.format = ".7z"
	if _, ok := nestedDeflateResidentBytes(root, m); ok {
		t.Fatal("non-ZIP root eligible")
	}
}

func TestNestedDeflatePoolPreCancelledReadDoesNotWaitForCloseLock(t *testing.T) {
	cache, parent, _ := deflatePoolFixture(t)
	pool := newNestedDeflatePool(1, 100, time.Hour)
	defer pool.Close()
	lease, ok := pool.acquire(context.Background(), "pre-cancel-close", 10, parent, deflatePoolPlainReader(cache, "pre-cancel-close", &deflatePoolCloseTracker{}))
	if !ok {
		t.Fatal("admit")
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := make(chan error, 1)
	go func() { _, err := lease.ReadAtContext(ctx, make([]byte, 4), 0); result <- err }()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("pre-cancelled read waited for lease close lock")
	}
}

func TestNestedDeflatePoolClosingLeaseDoesNotBlockNewReads(t *testing.T) {
	cache, parent, _ := deflatePoolFixture(t)
	pool := newNestedDeflatePool(1, 100, time.Hour)
	defer pool.Close()
	entered, resume := make(chan struct{}), make(chan struct{})
	var resumeOnce sync.Once
	unblock := func() { resumeOnce.Do(func() { close(resume) }) }
	defer unblock()
	candidate := deflatePoolReader(cache, "closing", context.Background(), func(ctx context.Context) (io.ReadCloser, error) {
		return &deflatePoolStream{Reader: &deflatePoolBlockingReader{ctx: ctx, entered: entered, resume: resume, body: bytes.NewReader(bytes.Repeat([]byte("x"), 128))}, closer: &deflatePoolCloseTracker{}}, nil
	})
	lease, ok := pool.acquire(context.Background(), "closing", 10, parent, candidate)
	if !ok {
		t.Fatal("admit")
	}
	active := make(chan error, 1)
	go func() { _, err := lease.ReadAtContext(context.Background(), make([]byte, 4), 0); active <- err }()
	waitDeflatePoolEntered(t, entered)
	ended := make(chan struct{})
	go func() { lease.Close(); close(ended) }()
	markedClosed := make(chan struct{})
	go func() {
		for {
			lease.mu.Lock()
			closing := lease.closed
			lease.mu.Unlock()
			if closing {
				close(markedClosed)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	select {
	case <-markedClosed:
	case <-time.After(time.Second):
		t.Fatal("Close did not mark lease closed independently of active I/O")
	}
	later := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _, err := lease.ReadAtContext(ctx, make([]byte, 4), 16); later <- err }()
	cancel()
	select {
	case err := <-later:
		if !errors.Is(err, storage.ErrClosed) && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("new read waited behind Close and active source")
	}
	select {
	case <-ended:
		t.Fatal("Close returned before its active read finished")
	default:
	}
	unblock()
	if err := <-active; err != nil {
		t.Fatal(err)
	}
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("Close did not release after read finished")
	}
}
