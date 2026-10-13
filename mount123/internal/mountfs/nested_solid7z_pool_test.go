package mountfs

import (
	"bytes"
	"context"
	"errors"
	"hash/crc32"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/bodgit/sevenzip"
)

type solidPoolTestStream struct {
	io.Reader
	closed  atomic.Int32
	closeFn func()
}

func (s *solidPoolTestStream) Close() error {
	s.closed.Add(1)
	if s.closeFn != nil {
		s.closeFn()
	}
	return nil
}

func TestNestedSolid7zPoolLifetimeAndBudget(t *testing.T) {
	pool := newNestedSolid7zPool(2, 100)
	defer pool.Close()
	parent := &nestedArchive{}
	defer parent.Close()
	acquire := func(key string, charge int64) *nestedSolid7zEntry {
		t.Helper()
		entry, err := pool.acquire(context.Background(), key, charge, parent)
		if err != nil || entry == nil {
			t.Fatalf("acquire %s: %v", key, err)
		}
		return entry
	}
	a := acquire("a", 50)
	streamA := &solidPoolTestStream{Reader: bytes.NewReader(nil)}
	a.state = &nestedSolid7zState{stream: streamA}
	pool.release(a, true)
	b := acquire("b", 50)
	streamB := &solidPoolTestStream{Reader: bytes.NewReader(nil)}
	b.state = &nestedSolid7zState{stream: streamB}
	pool.release(b, true)
	// Refresh A's recency, so capacity pressure evicts B even when A is active.
	if got := acquire("a", 50); got != a {
		t.Fatal("idle continuation lost")
	}
	c := acquire("c", 50)
	if streamB.closed.Load() != 1 || streamA.closed.Load() != 0 {
		t.Fatal("LRU eviction closed the wrong decoder")
	}
	c.state = &nestedSolid7zState{stream: &solidPoolTestStream{Reader: bytes.NewReader(nil)}}
	if entry, err := pool.acquire(context.Background(), "d", 1, parent); entry != nil || err != nil {
		t.Fatal("active entries exceeded budget")
	}
	if entry, err := pool.acquire(context.Background(), "oversize", 101, parent); entry != nil || err != nil {
		t.Fatal("oversize was admitted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := pool.acquire(ctx, "a", 50, parent); done <- err }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("busy waiter ignored cancellation")
	}
	if streamA.closed.Load() != 0 {
		t.Fatal("waiter cancellation closed producer")
	}
	// Closing the creator does not invalidate the pool's retained parent.
	parent.Close()
	if parent.closed {
		t.Fatal("pooled state lost its parent")
	}
	pool.Close()
	if streamA.closed.Load() != 0 {
		t.Fatal("shutdown closed active decoder")
	}
	if _, err := pool.acquire(context.Background(), "a", 50, parent); !errors.Is(err, storage.ErrClosed) {
		t.Fatal(err)
	}
	pool.release(a, true)
	pool.release(c, false)
	if streamA.closed.Load() != 1 || !parent.closed || pool.used != 0 {
		t.Fatal("shutdown leaked decoder, parent or charge")
	}
	pool.Close()
	if streamA.closed.Load() != 1 {
		t.Fatal("decoder closed twice")
	}
}

func TestNestedSolid7zRetiringStateRemainsCharged(t *testing.T) {
	pool := newNestedSolid7zPool(1, 50)
	parent := &nestedArchive{}
	defer parent.Close()
	defer pool.Close()
	entry, err := pool.acquire(context.Background(), "a", 50, parent)
	if err != nil {
		t.Fatal(err)
	}
	started, unblock, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var unblockOnce sync.Once
	finish := func() { unblockOnce.Do(func() { close(unblock) }) }
	defer finish()
	stream := &solidPoolTestStream{Reader: bytes.NewReader(nil), closeFn: func() { close(started); <-unblock }}
	entry.state = &nestedSolid7zState{stream: stream}
	go func() { pool.release(entry, false); close(done) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("retirement did not start closing the decoder")
	}
	candidate, err := pool.acquire(context.Background(), "b", 50, parent)
	finish()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("retirement did not finish after decoder close")
	}
	if candidate != nil || err != nil {
		t.Fatal("retiring decoder's memory was reused before Close returned")
	}
	candidate, err = pool.acquire(context.Background(), "b", 50, parent)
	if candidate == nil || err != nil {
		t.Fatal("retired charge was not released")
	}
	pool.release(candidate, false)
}

func solidPoolState(parts ...[]byte) *nestedSolid7zState {
	var body []byte
	layout := &sevenzip.StreamLayout{}
	for i, part := range parts {
		layout.Members = append(layout.Members, sevenzip.StreamMemberInfo{Ordinal: i, Offset: int64(len(body)), Size: uint64(len(part)), CRC: sevenzip.CRCInfo{Defined: true, Value: crc32.ChecksumIEEE(part)}})
		body = append(body, part...)
	}
	layout.UncompressedSize = uint64(len(body))
	layout.CRC = sevenzip.CRCInfo{Defined: true, Value: crc32.ChecksumIEEE(body)}
	return &nestedSolid7zState{stream: io.NopCloser(bytes.NewReader(body)), layout: layout}
}

type solidPoolErrorReader struct {
	data []byte
	err  error
}

func (r *solidPoolErrorReader) Read(p []byte) (int, error) {
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, r.err
}

func TestNestedSolid7zConsumeIntegrityAndBoundaries(t *testing.T) {
	ctx := context.Background()
	t.Run("ordinary boundary does not drain suffix", func(t *testing.T) {
		state := solidPoolState([]byte("abcd"), []byte("efgh"))
		if err := state.consume(ctx, 4, io.Discard); err != nil {
			t.Fatal(err)
		}
		if state.position != 4 || state.finished {
			t.Fatal("member boundary drained group suffix")
		}
		var output bytes.Buffer
		if err := state.consume(ctx, 6, &output); err != nil || output.String() != "ef" {
			t.Fatal(err)
		}
		if err := state.consume(ctx, 8, io.Discard); err != nil || !state.finished {
			t.Fatal(err)
		}
	})
	t.Run("discarded prefix CRC", func(t *testing.T) {
		state := solidPoolState([]byte("abcd"), []byte("efgh"))
		state.layout.Members[0].CRC.Value++
		if err := state.consume(ctx, 6, io.Discard); !errors.Is(err, errArchiveIntegrity) {
			t.Fatal(err)
		}
		if state.position != 4 {
			t.Fatal("decoded beyond failed member boundary")
		}
	})
	for _, defined := range []bool{false, true} {
		name := "absent zero CRC"
		if defined {
			name = "declared zero CRC"
		}
		t.Run(name, func(t *testing.T) {
			state := solidPoolState([]byte("abcd"))
			state.layout.Members[0].CRC = sevenzip.CRCInfo{Defined: defined}
			err := state.consume(ctx, 4, io.Discard)
			if defined && !errors.Is(err, errArchiveIntegrity) || !defined && err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Run("declared zero group CRC", func(t *testing.T) {
		state := solidPoolState([]byte("abcd"))
		state.layout.CRC.Value = 0
		if err := state.consume(ctx, 4, io.Discard); !errors.Is(err, errArchiveIntegrity) {
			t.Fatal(err)
		}
	})
	t.Run("source error with final bytes", func(t *testing.T) {
		state := solidPoolState([]byte("abcd"))
		sourceErr := errors.New("synthetic source failure")
		state.stream = io.NopCloser(&solidPoolErrorReader{data: []byte("bad!"), err: sourceErr})
		if err := state.consume(ctx, 4, io.Discard); !errors.Is(err, sourceErr) {
			t.Fatal(err)
		}
	})
	t.Run("recorded source error takes precedence", func(t *testing.T) {
		state := solidPoolState([]byte("abcd"))
		sourceErr := errors.New("synthetic underlying source failure")
		state.source = &nestedSourceReader{}
		state.source.record(sourceErr)
		state.layout.Members[0].CRC.Value++
		if err := state.consume(ctx, 4, io.Discard); !errors.Is(err, sourceErr) {
			t.Fatal(err)
		}
	})
	t.Run("short body", func(t *testing.T) {
		state := solidPoolState([]byte("abcd"))
		state.stream = io.NopCloser(bytes.NewReader([]byte("ab")))
		if err := state.consume(ctx, 4, io.Discard); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatal(err)
		}
	})
	t.Run("cancel at final boundary", func(t *testing.T) {
		state := solidPoolState([]byte("abcd"))
		if err := state.consume(ctx, 2, io.Discard); err != nil {
			t.Fatal(err)
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if err := state.consume(canceled, 2, io.Discard); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if state.position != 2 {
			t.Fatal("canceled read advanced cursor")
		}
	})
}

func TestNestedSolid7zInvalidFinalBlockIsNeverPublished(t *testing.T) {
	cache, err := storage.NewCache(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	fills := 0
	reader := &nestedMemberReader{ctx: context.Background(), cache: cache, key: "synthetic-invalid-group", size: 4, blockBytes: 4,
		gate: func(context.Context) (func(), error) { return func() {}, nil },
		open: func(context.Context) (io.ReadCloser, error) {
			t.Fatal("integrity failure fell back to a different decoder")
			return nil, nil
		},
		fill: func(ctx context.Context, start, length int64, out io.Writer) (bool, error) {
			fills++
			state := solidPoolState([]byte("abcd"))
			state.layout.CRC.Value++
			return true, state.consume(ctx, start+length, out)
		},
	}
	defer reader.Close()
	for i := 0; i < 2; i++ {
		got := make([]byte, 4)
		if n, err := reader.ReadAt(got, 0); n != 0 || !errors.Is(err, errArchiveIntegrity) {
			t.Fatalf("failed fill became readable: %d %v", n, err)
		}
	}
	if fills != 2 {
		t.Fatal("failed block was cached")
	}
}
