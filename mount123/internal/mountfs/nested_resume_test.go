package mountfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"syscall"
	"testing"

	"github.com/binghezhouke/123/mount123/internal/storage"
)

// The resume source is optional: a cached position can replace a live decoder
// only when it advances toward the demand, never past it. Cache hits bypass it.
func TestNestedMemberResumeForwardBackwardAndCacheHit(t *testing.T) {
	cache, err := storage.NewCache(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	payload := bytes.Repeat([]byte("0123456789abcdef"), 32)
	var decoded int64
	opens, resumes := 0, 0
	r := &nestedMemberReader{ctx: context.Background(), cache: cache, key: "resume", size: int64(len(payload)), blockBytes: 16,
		gate: func(context.Context) (func(), error) { return func() {}, nil },
		open: func(context.Context) (io.ReadCloser, error) {
			opens++
			return countedNestedStream{bytes.NewReader(payload), &decoded}, nil
		},
		resume: func(_ context.Context, target, after int64) (io.ReadCloser, int64, error) {
			resumes++
			point := target / 64 * 64
			if point <= after {
				return nil, 0, nil
			}
			return countedNestedStream{bytes.NewReader(payload[point:]), &decoded}, point, nil
		},
	}
	defer r.Close()
	for _, off := range []int64{0, 320, 128, 400, 128} {
		got := make([]byte, 4)
		if n, err := r.ReadAt(got, off); n != len(got) || err != nil || !bytes.Equal(got, payload[off:off+4]) {
			t.Fatalf("read off=%d n=%d error=%v", off, n, err)
		}
	}
	if opens != 1 || resumes != 3 || decoded != 80 {
		t.Fatalf("opens=%d resumes=%d decoded=%d, want 1/3/80", opens, resumes, decoded)
	}
}

func TestNestedMemberResumeMissAndInvalidPosition(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		cache, err := storage.NewCache(t.TempDir(), 1024)
		if err != nil {
			t.Fatal(err)
		}
		payload := bytes.Repeat([]byte("a"), 64)
		r := &nestedMemberReader{ctx: context.Background(), cache: cache, key: "resume-miss", size: 64, blockBytes: 16,
			gate: func(context.Context) (func(), error) { return func() {}, nil },
			open: func(context.Context) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(payload)), nil },
			resume: func(_ context.Context, target, _ int64) (io.ReadCloser, int64, error) {
				if invalid {
					return io.NopCloser(bytes.NewReader(payload)), target + 1, nil
				}
				return nil, 0, nil
			},
		}
		got := make([]byte, 4)
		_, err = r.ReadAt(got, 16)
		if invalid && !errors.Is(err, syscall.EIO) {
			t.Fatalf("invalid position accepted: %v", err)
		} else if !invalid && (err != nil || string(got) != "aaaa") {
			t.Fatalf("fallback: %q %v", got, err)
		}
		r.Close()
		cache.Close()
	}
}
