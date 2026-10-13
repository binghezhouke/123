package mountfs

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/binghezhouke/123/mount123/internal/storage"
)

type cachedTailCountingStream struct {
	io.ReadCloser
	decoded *int64
	before  func(int64) error
}

func (r cachedTailCountingStream) Read(p []byte) (int, error) {
	if r.before != nil {
		if err := r.before(*r.decoded); err != nil {
			return 0, err
		}
	}
	n, err := r.ReadCloser.Read(p)
	*r.decoded += int64(n)
	return n, err
}

// Use the actual ZIP collector/checksum with tiny cache blocks. The EOF hook,
// rather than a fabricated checksum result, shows whether the stream verified.
func TestNestedCachedTailCompletionBoundsAndEligibility(t *testing.T) {
	cases := []struct {
		name         string
		enabled      bool
		cached       []int64
		off          int64
		length       int
		decoded      int64
		opens, valid int
	}{
		{"collector-near-eof", true, []int64{48}, 32, 32, 64, 1, 1},
		{"ordinary-leaf", false, []int64{48}, 32, 32, 48, 1, 0},
		{"pure-hit", true, []int64{48}, 48, 16, 0, 0, 0},
		{"far-from-eof", true, []int64{16, 32, 48}, 0, 64, 16, 1, 0},
		{"request-before-eof", true, []int64{48}, 32, 20, 48, 1, 0},
	}
	payload := bytes.Repeat([]byte("0123456789abcdef"), 4)
	compressed := zipCheckpointTestCompressed(t, payload, flate.NoCompression, false, false)
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			cache, err := storage.NewCache(t.TempDir(), 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer cache.Close()
			for _, start := range test.cached {
				if err := cache.Store(context.Background(), fmt.Sprintf("cached-tail:block:%d", start), payload[start:start+16]); err != nil {
					t.Fatal(err)
				}
			}
			var decoded int64
			opens, permits := 0, 0
			observed := new(zipCheckpointObservation)
			reader := &nestedMemberReader{ctx: context.Background(), cache: cache, key: "cached-tail", size: 64, blockBytes: 16, finishCachedTail: test.enabled,
				gate: func(context.Context) (func(), error) { permits++; return func() {}, nil },
				open: func(ctx context.Context) (io.ReadCloser, error) {
					opens++
					source, m, offset := zipCheckpointTestInput(compressed, payload)
					stream, err := openZIPDeflateCheckpointStream(ctx, source, m, offset, nil, observed.hooks())
					if err != nil {
						return nil, err
					}
					return cachedTailCountingStream{ReadCloser: stream, decoded: &decoded}, nil
				},
			}
			defer reader.Close()
			got := make([]byte, test.length)
			n, err := reader.ReadAt(got, test.off)
			if n != len(got) || err != nil || !bytes.Equal(got, payload[test.off:test.off+int64(len(got))]) {
				t.Fatalf("read n=%d error=%v", n, err)
			}
			if decoded != test.decoded || opens != test.opens || observed.valid != test.valid {
				t.Fatalf("decoded=%d opens=%d valid=%d, want %d/%d/%d", decoded, opens, observed.valid, test.decoded, test.opens, test.valid)
			}
			if test.opens == 0 && permits != 0 {
				t.Fatalf("disk hit acquired %d permits", permits)
			}
			if test.valid == 1 && (reader.stream != nil || observed.close != 1) {
				t.Fatalf("verified stream retained: close=%d", observed.close)
			}
		})
	}
}

func TestNestedCachedTailCompletionPreservesCRCAndCancellation(t *testing.T) {
	payload := bytes.Repeat([]byte("0123456789abcdef"), 4)
	compressed := zipCheckpointTestCompressed(t, payload, flate.NoCompression, false, false)
	sourceErr := errors.New("synthetic tail source failure")
	for _, fault := range []string{"crc", "cancel", "source"} {
		t.Run(fault, func(t *testing.T) {
			cache, err := storage.NewCache(t.TempDir(), 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer cache.Close()
			if err := cache.Store(context.Background(), "bad-tail:block:48", payload[48:]); err != nil {
				t.Fatal(err)
			}
			var decoded int64
			observed := new(zipCheckpointObservation)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reader := &nestedMemberReader{ctx: context.Background(), cache: cache, key: "bad-tail", size: 64, blockBytes: 16, finishCachedTail: true,
				gate: func(context.Context) (func(), error) { return func() {}, nil },
				open: func(ctx context.Context) (io.ReadCloser, error) {
					source, m, offset := zipCheckpointTestInput(compressed, payload)
					if fault == "crc" {
						m.crc ^= 1
					}
					stream, err := openZIPDeflateCheckpointStream(ctx, source, m, offset, nil, observed.hooks())
					if err != nil {
						return nil, err
					}
					return cachedTailCountingStream{ReadCloser: stream, decoded: &decoded, before: func(n int64) error {
						if fault == "cancel" && n == 48 {
							cancel()
						}
						if fault == "source" && n == 48 {
							return sourceErr
						}
						return nil
					}}, nil
				},
			}
			defer reader.Close()
			got := make([]byte, 32)
			n, err := reader.ReadAtContext(ctx, got, 32)
			want := error(zip.ErrChecksum)
			if fault == "cancel" {
				want = context.Canceled
			} else if fault == "source" {
				want = sourceErr
			}
			if !errors.Is(err, want) || n != len(got) || !bytes.Equal(got, payload[32:]) {
				t.Fatalf("cached bytes n=%d error=%v want=%v", n, err, want)
			}
			if reader.stream != nil || reader.position != 0 || observed.valid != 0 || observed.close != 1 {
				t.Fatalf("failed tail retained/published: pos=%d valid=%d close=%d", reader.position, observed.valid, observed.close)
			}
			// Failed optional completion does not delete existing correct blocks
			// or turn an entirely cached healthy request into decoder work.
			before := decoded
			if n, err := reader.ReadAtContext(context.Background(), got, 32); n != len(got) || err != nil || decoded != before {
				t.Fatalf("healthy cached retry n=%d error=%v decoded=%d", n, err, decoded)
			}
		})
	}
}
