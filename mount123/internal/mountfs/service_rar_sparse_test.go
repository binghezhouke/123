package mountfs

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"syscall"
	"testing"
	"time"

	"github.com/nwaples/rardecode/v2"
)

func rarSparseStoredZIP(t *testing.T, name string, payload []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// A stored RAR member has no compression dependency on its prefix. Seeking to
// the inner ZIP directory should therefore not scan the complete member.
func TestServiceStoredRARNestedZIPUsesSparseSourceReads(t *testing.T) {
	inner := rarSparseStoredZIP(t, "payload.bin", bytes.Repeat([]byte("s"), (8<<20)+13))
	data := multilevelFixture(t, "rar", map[string][]byte{"inner.zip": inner})
	s := multilevelService(t, "rar", data, Options{DisableReadAhead: true}, 32<<20)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if rows, err := s.ListArchive(ctx, 77, ""); err != nil || len(rows) != 1 {
		t.Fatalf("outer listing count=%d err=%v", len(rows), err)
	}
	before := s.tree.cache.Stats().Foreground
	downloaded := func() uint64 {
		counter := s.tree.cache.IOStats().Snapshot().DownloadedBytes.Bytes
		if counter == nil {
			return 0
		}
		return *counter
	}
	beforeDownloaded := downloaded()
	rows, err := s.ListArchive(ctx, 77, "inner.zip")
	if err != nil || len(rows) != 1 || rows[0].Name != "payload.bin" {
		t.Fatalf("inner listing count=%d err=%v", len(rows), err)
	}
	after := s.tree.cache.Stats().Foreground
	requested := after.RequestedBytes - before.RequestedBytes
	t.Logf("inner index source requested=%d bytes; source misses=%d bytes; HTTP downloaded=%d bytes", requested, after.MissBytes-before.MissBytes, downloaded()-beforeDownloaded)
	if requested >= 256<<10 {
		t.Errorf("stored RAR index requested %d source bytes, want <256 KiB", requested)
	}
	if wire := downloaded() - beforeDownloaded; wire >= 3<<20 {
		t.Errorf("stored RAR index downloaded %d bytes, want <3 MiB", wire)
	}
}

func TestServiceStoredRARNestedRangesAndChecksum(t *testing.T) {
	want := make([]byte, (8<<20)+13)
	for i := range want {
		want[i] = byte(i*29 + i/251)
	}
	rarData := multilevelFixture(t, "rar", map[string][]byte{"payload.bin": want})
	data := rarSparseStoredZIP(t, "inner.rar", rarData)
	s := multilevelService(t, "zip", data, Options{DisableReadAhead: true}, 32<<20)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, size, err := s.OpenArchiveMember(ctx, 77, "inner.rar/payload.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer r.(io.Closer).Close()
	if size != int64(len(want)) {
		t.Fatalf("size=%d, want=%d", size, len(want))
	}
	before := s.tree.cache.Stats().Foreground.RequestedBytes
	for _, off := range []int64{size - 4096, 0, size / 2, 17, size - 19, size - 4096} {
		got := make([]byte, 4096)
		n, err := r.ReadAt(got, off)
		length := min(len(got), len(want)-int(off))
		if n != length || (err != nil && !errors.Is(err, io.EOF)) || !bytes.Equal(got[:n], want[off:off+int64(n)]) {
			t.Fatalf("range off=%d, n=%d, err=%v", off, n, err)
		}
	}
	if requested := s.tree.cache.Stats().Foreground.RequestedBytes - before; requested >= 256<<10 {
		t.Errorf("sparse member reads requested %d source bytes, want <256 KiB", requested)
	}
	if n, err := r.ReadAt(make([]byte, 1), size); n != 0 || err != io.EOF {
		t.Fatalf("EOF n=%d, err=%v", n, err)
	}
	// Full reads after out-of-order reads restart native checksum tracking.
	got := make([]byte, len(want))
	if n, err := r.ReadAt(got, 0); n != len(got) || err != nil || !bytes.Equal(got, want) {
		t.Fatalf("full checked read n=%d, err=%v", n, err)
	}
	for off := 0; off < len(want); off += 1 << 20 {
		end := min(off+1<<20, len(want))
		if n, err := r.ReadAt(got[off:end], int64(off)); n != end-off || err != nil {
			t.Fatalf("sequential checked read off=%d, n=%d, err=%v", off, n, err)
		}
	}
}

func TestServiceStoredRARNestedCorruptFullReadFails(t *testing.T) {
	want := bytes.Repeat([]byte("synthetic payload for native RAR checksum\n"), 2000)
	rarData := multilevelFixture(t, "rar", map[string][]byte{"payload.bin": want})
	start := bytes.Index(rarData, want[:128])
	if start < 0 {
		t.Fatal("synthetic RAR payload not found")
	}
	rarData[start+len(want)-1] ^= 1
	data := rarSparseStoredZIP(t, "inner.rar", rarData)
	s := multilevelService(t, "zip", data, Options{DisableReadAhead: true}, 32<<20)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for attempt := 0; attempt < 2; attempt++ {
		r, size, err := s.OpenArchiveMember(ctx, 77, "inner.rar/payload.bin")
		if err != nil {
			t.Fatal(err)
		}
		if n, err := r.ReadAt(make([]byte, size), 0); n != int(size) || !errors.Is(err, rardecode.ErrBadFileChecksum) {
			t.Fatalf("full read attempt=%d, n=%d, err=%v, want native RAR checksum failure", attempt, n, err)
		}
		_ = r.(io.Closer).Close()
	}
}

type rarSparseInputFunc func(context.Context, []byte, int64) (int, error)

func (f rarSparseInputFunc) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	return f(ctx, p, off)
}

func TestNestedRARStoredInputRebindsRequestsAndPreservesErrors(t *testing.T) {
	want := bytes.Repeat([]byte("synthetic source bytes"), 100)
	data := multilevelFixture(t, "rar", map[string][]byte{"payload.bin": want})
	members, err := indexNestedRAR(context.Background(), bytes.NewReader(data), int64(len(data)), nil, defaultNestedArchivePolicy)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := nestedIndexFromMembers(members, defaultNestedArchivePolicy)
	if err != nil {
		t.Fatal(err)
	}
	m := idx.members["payload.bin"]
	type requestTag struct{}
	var seen any
	var sourceErr error
	input := rarSparseInputFunc(func(ctx context.Context, p []byte, off int64) (int, error) {
		seen = ctx.Value(requestTag{})
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if sourceErr != nil {
			return 0, sourceErr
		}
		return bytes.NewReader(data).ReadAt(p, off)
	})
	first, cancel := context.WithCancel(context.WithValue(context.Background(), requestTag{}, 1))
	direct, ok, err := nestedRARStoredMemberReader(first, input, int64(len(data)), m, nil)
	if err != nil || !ok {
		t.Fatalf("open direct=%t, err=%v", ok, err)
	}
	defer direct.Close()
	cancel()
	second := context.WithValue(context.Background(), requestTag{}, 2)
	got := make([]byte, 37)
	if n, err := direct.ReadAtContext(second, got, 19); n != len(got) || err != nil || !bytes.Equal(got, want[19:56]) || seen != 2 {
		t.Fatalf("next request n=%d, err=%v, tag=%v", n, err, seen)
	}
	if _, err := direct.ReadAtContext(first, got, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation err=%v", err)
	}
	sourceErr = errors.New("synthetic source transport failure")
	if _, err := direct.ReadAtContext(second, got, 0); !errors.Is(err, sourceErr) {
		t.Fatalf("source error=%v", err)
	}
	if _, _, err := nestedRARStoredMemberReader(second, input, int64(len(data)), m, nil); !errors.Is(err, sourceErr) {
		t.Fatalf("header source error=%v", err)
	}
	sourceErr = nil
	if n, err := direct.ReadAtContext(second, got, 0); n != len(got) || err != nil || !bytes.Equal(got, want[:len(got)]) {
		t.Fatalf("retry n=%d, err=%v", n, err)
	}
	stale := *m
	stale.name = "different.bin"
	if _, _, err := nestedRARStoredMemberReader(second, input, int64(len(data)), &stale, nil); !errors.Is(err, syscall.ESTALE) {
		t.Fatalf("stale member error=%v", err)
	}
	_ = direct.Close()
	if _, err := direct.ReadAtContext(second, got, 0); err == nil {
		t.Fatal("read after Close succeeded")
	}
}
