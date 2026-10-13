package mountfs

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"hash/crc32"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type checkedInputFixture struct {
	data    []byte
	mu      sync.Mutex
	offsets []int64
	read    func(context.Context, []byte, int64) (int, error)
}

func (r *checkedInputFixture) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	r.mu.Lock()
	r.offsets = append(r.offsets, off)
	r.mu.Unlock()
	if r.read != nil {
		return r.read(ctx, p, off)
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return bytes.NewReader(r.data).ReadAt(p, off)
}

func TestArchiveCheckedInputCompleteReads(t *testing.T) {
	want := []byte("direct member bytes")
	for _, corrupt := range []bool{false, true} {
		for _, integrityErr := range []error{zip.ErrChecksum, errArchiveIntegrity} {
			data := append([]byte(nil), want...)
			if corrupt {
				data[len(data)-1] ^= 1
			}
			input := newArchiveCheckedInput(&checkedInputFixture{data: data}, int64(len(data)), crc32.ChecksumIEEE(want), true, integrityErr)
			for attempt := 0; attempt < 2; attempt++ {
				got := make([]byte, len(data))
				n, err := input.ReadAtContext(context.Background(), got, 0)
				if n != len(data) || !bytes.Equal(got, data) {
					t.Fatalf("returned bytes = %d, %q", n, got)
				}
				if corrupt && !errors.Is(err, integrityErr) {
					t.Fatalf("corrupt complete read: %v", err)
				}
				if !corrupt && err != nil {
					t.Fatalf("valid complete read: %v", err)
				}
			}
		}
	}
}

func TestArchiveCheckedInputSequentialReadsVerifyCRC(t *testing.T) {
	want := []byte("sequential direct member")
	for _, corrupt := range []bool{false, true} {
		data := append([]byte(nil), want...)
		if corrupt {
			data[len(data)-1] ^= 1
		}
		input := newArchiveCheckedInput(&checkedInputFixture{data: data}, int64(len(data)), crc32.ChecksumIEEE(want), true, zip.ErrChecksum)
		for off := 0; off < len(data); {
			length := min(5, len(data)-off)
			n, err := input.ReadAtContext(context.Background(), make([]byte, length), int64(off))
			off += n
			if n != length {
				t.Fatalf("short direct chunk: %d", n)
			}
			if corrupt && off == len(data) {
				if !errors.Is(err, zip.ErrChecksum) {
					t.Fatalf("contiguous EOF did not check CRC: %v", err)
				}
			} else if err != nil {
				t.Fatalf("valid or partial chunk produced checksum error: %v", err)
			}
		}
	}
}

func TestArchiveCheckedInputTailDoesNotReadPrefix(t *testing.T) {
	data := bytes.Repeat([]byte("a"), 4<<20)
	source := &checkedInputFixture{data: data}
	input := newArchiveCheckedInput(source, int64(len(data)), 123, true, zip.ErrChecksum)
	off := int64(len(data) - 32)
	got := make([]byte, 32)
	for attempt := 0; attempt < 2; attempt++ {
		if n, err := input.ReadAtContext(context.Background(), got, off); n != len(got) || err != nil || !bytes.Equal(got, data[off:]) {
			t.Fatalf("direct tail = %d, %v", n, err)
		}
	}
	if len(source.offsets) != 2 || source.offsets[0] != off || source.offsets[1] != off {
		t.Fatalf("tail read touched prefix: %v", source.offsets)
	}
}

func TestArchiveCheckedInputGapsAndOverlapsStopSequentialVerification(t *testing.T) {
	for _, offsets := range [][]int64{{0, 4, 6}, {0, 0, 2, 4, 6}, {0, 2, 1, 3, 5, 6}} {
		input := newArchiveCheckedInput(&checkedInputFixture{data: []byte("abcdefgh")}, 8, 123, true, zip.ErrChecksum)
		for _, off := range offsets {
			_, err := input.ReadAtContext(context.Background(), make([]byte, 2), off)
			// A repeated zero offset starts a fresh complete pass; this sequence
			// is contiguous from its second read and must reject the bad CRC.
			complete := len(offsets) == 5 && off == 6
			if complete && !errors.Is(err, zip.ErrChecksum) {
				t.Fatalf("restarted pass did not verify: %v", err)
			}
			if !complete && err != nil {
				t.Fatalf("discontinuous pass produced checksum error: %v", err)
			}
		}
	}
}

func TestArchiveCheckedInputDistinguishesUnknownAndZeroCRC(t *testing.T) {
	for _, known := range []bool{false, true} {
		input := newArchiveCheckedInput(&checkedInputFixture{data: []byte("not a zero CRC")}, 14, 0, known, zip.ErrChecksum)
		_, err := input.ReadAtContext(context.Background(), make([]byte, 14), 0)
		if known && !errors.Is(err, zip.ErrChecksum) {
			t.Fatalf("known zero CRC was skipped: %v", err)
		}
		if !known && err != nil {
			t.Fatalf("optional unknown CRC was checked: %v", err)
		}
	}
}

func TestArchiveCheckedInputCancellationDoesNotPoisonRepeat(t *testing.T) {
	data := []byte("retry a cancelled direct read")
	started := make(chan struct{})
	var calls atomic.Int32
	source := &checkedInputFixture{data: data, read: func(ctx context.Context, p []byte, off int64) (int, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-ctx.Done()
			return 0, ctx.Err()
		}
		return bytes.NewReader(data).ReadAt(p, off)
	}}
	input := newArchiveCheckedInput(source, int64(len(data)), crc32.ChecksumIEEE(data), true, zip.ErrChecksum)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := input.ReadAtContext(ctx, make([]byte, len(data)), 0); done <- err }()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled I/O: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("direct I/O did not observe cancellation")
	}
	if n, err := input.ReadAtContext(context.Background(), make([]byte, len(data)), 0); n != len(data) || err != nil {
		t.Fatalf("repeat after cancellation: %d, %v", n, err)
	}
}

func TestArchiveCheckedInputConcurrentReadsDoNotSerializeIO(t *testing.T) {
	data := []byte("abcdefgh")
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	source := &checkedInputFixture{data: data, read: func(ctx context.Context, p []byte, off int64) (int, error) {
		if off == 0 {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		}
		return bytes.NewReader(data).ReadAt(p, off)
	}}
	input := newArchiveCheckedInput(source, int64(len(data)), 123, true, zip.ErrChecksum)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	first := make(chan error, 1)
	go func() { _, err := input.ReadAtContext(ctx, make([]byte, 4), 0); first <- err }()
	<-started
	if n, err := input.ReadAtContext(ctx, make([]byte, 4), 4); n != 4 || err != nil {
		t.Fatalf("parallel tail waited for prefix or falsely verified: %d, %v", n, err)
	}
	unblock()
	if err := <-first; err != nil {
		t.Fatalf("out-of-order prefix falsely verified: %v", err)
	}
	// Two overlapping reads covered the whole member, but do not establish
	// an ordered CRC pass. A later tail must remain an ordinary direct read.
	if _, err := input.ReadAtContext(ctx, make([]byte, 4), 4); err != nil {
		t.Fatalf("concurrent operations left a false sequential pass: %v", err)
	}
}

func TestArchiveCheckedInputFullReadVerifiesDespiteConcurrentTail(t *testing.T) {
	data := []byte("abcdefgh")
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	source := &checkedInputFixture{data: data, read: func(ctx context.Context, p []byte, off int64) (int, error) {
		if off == 0 {
			close(started)
			<-release
		}
		return bytes.NewReader(data).ReadAt(p, off)
	}}
	input := newArchiveCheckedInput(source, int64(len(data)), 123, true, zip.ErrChecksum)
	done := make(chan error, 1)
	go func() { _, err := input.ReadAtContext(context.Background(), make([]byte, len(data)), 0); done <- err }()
	<-started
	if _, err := input.ReadAtContext(context.Background(), make([]byte, 2), 6); err != nil {
		t.Fatal(err)
	}
	unblock()
	if err := <-done; !errors.Is(err, zip.ErrChecksum) {
		t.Fatalf("parallel read suppressed independent full-response verification: %v", err)
	}
}

func TestArchiveCheckedInputPreservesEOFForOversizedRead(t *testing.T) {
	data := []byte("short")
	input := newArchiveCheckedInput(&checkedInputFixture{data: data}, int64(len(data)), crc32.ChecksumIEEE(data), true, zip.ErrChecksum)
	if n, err := input.ReadAtContext(context.Background(), make([]byte, 10), 0); n != len(data) || err != io.EOF {
		t.Fatalf("oversized read = %d, %v", n, err)
	}
}
