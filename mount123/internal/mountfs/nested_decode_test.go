package mountfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/storage"
)

type nestedTestReaderAt struct {
	ctx context.Context
	r   *nestedMemberReader
}

func (r nestedTestReaderAt) ReadAt(p []byte, off int64) (int, error) {
	return r.r.ReadAtContext(r.ctx, p, off)
}

func nestedTestTree(t *testing.T, builds int) *Tree {
	t.Helper()
	cache, err := storage.NewCache(t.TempDir(), 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	return &Tree{ctx: context.Background(), cache: cache, opts: Options{MaxConcurrentBuilds: builds}}
}

func TestNestedMemberRecursionSharesOneDecodePermit(t *testing.T) {
	tree := nestedTestTree(t, 1)
	data := bytes.Repeat([]byte("0123456789abcdef"), 1<<17)
	parent := &nestedMemberReader{ctx: tree.ctx, owner: tree, cache: tree.cache, key: "parent", size: int64(len(data)),
		open: func(ctx context.Context) (io.ReadCloser, error) {
			return io.NopCloser(&contextReader{ctx: ctx, r: bytes.NewReader(data)}), nil
		}}
	defer parent.Close()
	child := &nestedMemberReader{ctx: tree.ctx, owner: tree, cache: tree.cache, key: "child", size: int64(len(data)),
		open: func(ctx context.Context) (io.ReadCloser, error) {
			return io.NopCloser(io.NewSectionReader(nestedTestReaderAt{ctx: ctx, r: parent}, 0, int64(len(data)))), nil
		}}
	defer child.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, off := range []int64{0, nestedMemberBlockBytes} {
		buf := make([]byte, 32)
		if n, err := child.ReadAtContext(ctx, buf, off); n != len(buf) || err != nil || !bytes.Equal(buf, data[off:off+32]) {
			t.Fatalf("nested read at %d: %d %v", off, n, err)
		}
	}
}

func TestNestedMemberContinuationRebindsRequestContext(t *testing.T) {
	tree := nestedTestTree(t, 1)
	data := bytes.Repeat([]byte("a"), 3<<20)
	opens := 0
	r := &nestedMemberReader{ctx: tree.ctx, owner: tree, cache: tree.cache, key: "continuation-context", size: int64(len(data)),
		open: func(ctx context.Context) (io.ReadCloser, error) {
			opens++
			return io.NopCloser(&contextReader{ctx: ctx, r: bytes.NewReader(data)}), nil
		}}
	defer r.Close()
	first, cancel := context.WithCancel(context.Background())
	if _, err := r.ReadAtContext(first, make([]byte, 32), 0); err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := r.ReadAtContext(context.Background(), make([]byte, 32), nestedMemberBlockBytes); err != nil {
		t.Fatalf("new request inherited cancelled context: %v", err)
	}
	if opens != 1 {
		t.Fatalf("continuation reopened the decoder %d times", opens)
	}
}

type blockingNestedStream struct {
	ctx     context.Context
	started chan struct{}
}

func (r blockingNestedStream) Read([]byte) (int, error) {
	close(r.started)
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}
func (r blockingNestedStream) Close() error { return nil }

func TestNestedMemberCancellationDoesNotPoisonReader(t *testing.T) {
	tree := nestedTestTree(t, 2)
	started := make(chan struct{})
	opens := 0
	r := &nestedMemberReader{ctx: tree.ctx, owner: tree, cache: tree.cache, key: "cancel-retry", size: 16,
		open: func(ctx context.Context) (io.ReadCloser, error) {
			opens++
			if opens == 1 {
				return blockingNestedStream{ctx: ctx, started: started}, nil
			}
			return io.NopCloser(bytes.NewReader(make([]byte, 16))), nil
		}}
	defer r.Close()
	first, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := r.ReadAtContext(first, make([]byte, 16), 0); done <- err }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("first read did not start")
	}
	// A second request has a build permit, but must not get stuck waiting for
	// the first request's decoder lock after its own deadline expires.
	waiter, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stop()
	if _, err := r.ReadAtContext(waiter, make([]byte, 16), 0); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled waiter: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("first read: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled decoder did not stop")
	}
	if _, err := r.ReadAtContext(context.Background(), make([]byte, 16), 0); err != nil {
		t.Fatalf("retry after cancellation: %v", err)
	}
}

func TestNestedDecodeReleasedLeaseCannotBypassAdmission(t *testing.T) {
	tree := nestedTestTree(t, 1)
	stale, release, err := tree.acquireNestedDecode(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	release()
	_, busyRelease, err := tree.acquireNestedDecode(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer busyRelease()
	ctx, cancel := context.WithTimeout(stale, 20*time.Millisecond)
	defer cancel()
	if _, release, err := tree.acquireNestedDecode(ctx); !errors.Is(err, context.DeadlineExceeded) {
		if release != nil {
			release()
		}
		t.Fatalf("released lease bypassed admission: %v", err)
	}
}

func TestNestedMemberDiskHitBypassesBusyDecodeQueue(t *testing.T) {
	tree := nestedTestTree(t, 1)
	r := &nestedMemberReader{ctx: tree.ctx, owner: tree, cache: tree.cache, key: "warm-hit", size: 16,
		open: func(context.Context) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(make([]byte, 16))), nil
		}}
	defer r.Close()
	if _, err := r.ReadAtContext(context.Background(), make([]byte, 16), 0); err != nil {
		t.Fatal(err)
	}
	_, release, err := tree.acquireNestedDecode(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if n, err := r.ReadAtContext(ctx, make([]byte, 16), 0); n != 16 || err != nil {
		t.Fatalf("warm read waited for a decoder: %d %v", n, err)
	}
}

func TestNestedMemberMountCancellationStopsActiveRead(t *testing.T) {
	tree := nestedTestTree(t, 1)
	lifetime, shutdown := context.WithCancel(context.Background())
	defer shutdown()
	started := make(chan struct{})
	r := &nestedMemberReader{ctx: lifetime, owner: tree, cache: tree.cache, key: "mount-shutdown", size: 16,
		open: func(ctx context.Context) (io.ReadCloser, error) {
			return blockingNestedStream{ctx: ctx, started: started}, nil
		}}
	defer r.Close()
	done := make(chan error, 1)
	go func() { _, err := r.ReadAtContext(context.Background(), make([]byte, 16), 0); done <- err }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("read did not start")
	}
	shutdown()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("mount shutdown: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("mount shutdown did not interrupt source read")
	}
}
