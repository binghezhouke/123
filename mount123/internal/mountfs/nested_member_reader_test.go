package mountfs

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/binghezhouke/123/mount123/internal/storage"
)

type countedNestedStream struct {
	*bytes.Reader
	decoded *int64
}

func (s countedNestedStream) Read(p []byte) (int, error) {
	n, err := s.Reader.Read(p)
	*s.decoded += int64(n)
	return n, err
}
func (s countedNestedStream) Close() error { return nil }

func TestNestedMemberDemandAndContinuation(t *testing.T) {
	cache, err := storage.NewCache(t.TempDir(), 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	data := bytes.Repeat([]byte("0123456789abcdef"), 1<<18)
	opens := 0
	var decoded int64
	makeReader := func() *nestedMemberReader {
		return &nestedMemberReader{ctx: context.Background(), cache: cache, key: "archive/version/password/member", size: int64(len(data)), gate: func(context.Context) (func(), error) { return func() {}, nil }, open: func(context.Context) (io.ReadCloser, error) {
			opens++
			return countedNestedStream{bytes.NewReader(data), &decoded}, nil
		}}
	}
	r := makeReader()
	defer r.Close()
	head := make([]byte, 32)
	if _, err := r.ReadAt(head, 0); err != nil {
		t.Fatal(err)
	}
	if decoded != nestedMemberBlockBytes || opens != 1 {
		t.Fatalf("header decoded %d bytes with %d opens", decoded, opens)
	}
	// A neighbouring demand continues the same decoder, rather than decoding
	// its prefix a second time.
	if _, err := r.ReadAt(head, nestedMemberBlockBytes); err != nil {
		t.Fatal(err)
	}
	if decoded != 2*nestedMemberBlockBytes || opens != 1 {
		t.Fatalf("continuation decoded=%d opens=%d", decoded, opens)
	}
	if _, err := r.ReadAt(head, 0); err != nil {
		t.Fatal(err)
	}
	if decoded != 2*nestedMemberBlockBytes {
		t.Fatal("backward hit replayed the prefix")
	}
	r.Close()
	again := makeReader()
	defer again.Close()
	if _, err := again.ReadAt(head, 0); err != nil {
		t.Fatal(err)
	}
	if opens != 1 {
		t.Fatal("cached index block required a decoder after reopening")
	}
	if _, err := r.ReadAt(head, 0); err != storage.ErrClosed {
		t.Fatalf("read after close: %v", err)
	}
}

func TestNestedMemberTailDemandStopsAtRequestedBlock(t *testing.T) {
	cache, err := storage.NewCache(t.TempDir(), 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	data := bytes.Repeat([]byte("a"), 4<<20)
	var decoded int64
	r := &nestedMemberReader{ctx: context.Background(), cache: cache, key: "tail", size: int64(len(data)), gate: func(context.Context) (func(), error) { return func() {}, nil }, open: func(context.Context) (io.ReadCloser, error) {
		return countedNestedStream{bytes.NewReader(data), &decoded}, nil
	}}
	defer r.Close()
	if _, err := r.ReadAt(make([]byte, 32), 2<<20); err != nil {
		t.Fatal(err)
	}
	if decoded != (2<<20)+nestedMemberBlockBytes {
		t.Fatalf("decoded %d bytes, expected prefix only up to requested block", decoded)
	}
}

func TestNestedConcatDoesNotOpenUntouchedVolume(t *testing.T) {
	parts := []io.ReaderAt{bytes.NewReader([]byte("head")), untouchedNestedPart{t}, bytes.NewReader([]byte("tail"))}
	r := &nestedConcatReader{parts: parts, sizes: []int64{4, 4, 4}, total: 12}
	b := make([]byte, 4)
	if _, err := r.ReadAt(b, 0); err != nil || string(b) != "head" {
		t.Fatalf("head %q %v", b, err)
	}
	if _, err := r.ReadAt(b, 8); err != nil || string(b) != "tail" {
		t.Fatalf("tail %q %v", b, err)
	}
}

type untouchedNestedPart struct{ t *testing.T }

func (r untouchedNestedPart) ReadAt([]byte, int64) (int, error) {
	r.t.Fatal("index opened an untouched middle volume")
	return 0, io.EOF
}
