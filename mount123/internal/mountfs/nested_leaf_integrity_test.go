package mountfs

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

// The outer ZIP has a valid CRC over the complete inner archive. Corruption
// changes only the inner Store payload, leaving both archives' metadata valid.
func nestedStoredLeafService(t *testing.T, payload []byte, corrupt bool) *Service {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "leaf.bin", Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	inner := buf.Bytes()
	if corrupt {
		zr, err := zip.NewReader(bytes.NewReader(inner), int64(len(inner)))
		if err != nil {
			t.Fatal(err)
		}
		offset, err := zr.File[0].DataOffset()
		if err != nil {
			t.Fatal(err)
		}
		inner[offset+int64(len(payload))-1] ^= 1
	}
	outer := multilevelFixture(t, "zip", map[string][]byte{"inner.zip": inner})
	return multilevelService(t, "zip", outer, Options{}, 8<<20)
}

func openNestedStoredLeaf(t *testing.T, s *Service) (io.ReaderAt, int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	r, size, err := s.OpenArchiveMember(ctx, 77, "inner.zip/leaf.bin")
	if err != nil {
		t.Fatal(err)
	}
	if closer, ok := r.(io.Closer); ok {
		t.Cleanup(func() { _ = closer.Close() })
	}
	return r, size
}

func TestNestedStoredLeafFullReadRejectsChecksumFailure(t *testing.T) {
	payload := []byte("CRC protected stored leaf")
	r, size := openNestedStoredLeaf(t, nestedStoredLeafService(t, payload, true))
	if size != int64(len(payload)) {
		t.Fatalf("size = %d, want %d", size, len(payload))
	}
	for attempt := 0; attempt < 2; attempt++ {
		if n, err := r.ReadAt(make([]byte, size), 0); !errors.Is(err, zip.ErrChecksum) {
			t.Fatalf("attempt %d: corrupt member read = %d, %v; want checksum error", attempt, n, err)
		}
	}
}

func TestNestedStoredLeafFullReadVerifiesValidBytes(t *testing.T) {
	payload := []byte("CRC protected stored leaf")
	r, size := openNestedStoredLeaf(t, nestedStoredLeafService(t, payload, false))
	got := make([]byte, size)
	if n, err := r.ReadAt(got, 0); err != nil || n != len(payload) || !bytes.Equal(got, payload) {
		t.Fatalf("valid member read = %d, %v; bytes match = %v", n, err, bytes.Equal(got, payload))
	}
}

func TestNestedStoredLeafPrefixDoesNotValidateUnreadTail(t *testing.T) {
	// Reading a prefix fills one demanded block, not the whole member. A bad
	// CRC in the later tail must surface only when the full stream is consumed.
	payload := bytes.Repeat([]byte("a"), int(nestedMemberBlockBytes)+(64<<10))
	r, size := openNestedStoredLeaf(t, nestedStoredLeafService(t, payload, true))
	prefix := make([]byte, 32)
	if n, err := r.ReadAt(prefix, 0); err != nil || n != len(prefix) || !bytes.Equal(prefix, payload[:len(prefix)]) {
		t.Fatalf("prefix read = %d, %v; bytes match = %v", n, err, bytes.Equal(prefix, payload[:len(prefix)]))
	}
	if n, err := r.ReadAt(make([]byte, size), 0); !errors.Is(err, zip.ErrChecksum) {
		t.Fatalf("complete read after prefix = %d, %v; want checksum error", n, err)
	}
}
