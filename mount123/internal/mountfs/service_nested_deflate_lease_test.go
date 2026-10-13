package mountfs

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
)

type nestedDeflateLeaseAPI struct{ *serviceNestedAPI }

func (*nestedDeflateLeaseAPI) CacheIdentity() string { return "synthetic-deflate-lease-account" }

func nestedDeflateLeaseZIP(t *testing.T, name string, body []byte, method uint16) []byte {
	t.Helper()
	var encoded bytes.Buffer
	writer := zip.NewWriter(&encoded)
	member, err := writer.CreateHeader(&zip.FileHeader{Name: name, Method: method})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := member.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

// The source is intentionally incompressible. Source ReaderAt byte counts
// expose repeated Deflate prefix replay even though every HTTP range is warm;
// these counts are separate from actual decoder output and CPU time.
func TestServiceNestedDeflateLeaseAvoidsIndependentRequestPrefixReplay(t *testing.T) {
	payload := nestedDeflateLeasePayload()
	inner := nestedDeflateLeaseZIP(t, "payload.bin", payload, zip.Store)
	outer := nestedDeflateLeaseZIP(t, "inner.zip", inner, zip.Deflate)
	for _, shared := range []bool{false, true} {
		label := "independent_requests"
		if shared {
			label = "same_handle_control"
		}
		t.Run(label, func(t *testing.T) {
			service, cache, wire := nestedDeflateLeaseSource(t, outer)
			lifetime := context.Background()
			var err error
			seed, finishSeed := context.WithTimeout(context.Background(), 30*time.Second)
			rows, err := service.ListArchive(seed, 77, "inner.zip")
			finishSeed()
			if err != nil || len(rows) != 1 || rows[0].Name != "payload.bin" {
				t.Fatalf("seed nested listing count=%d error=%v", len(rows), err)
			}
			t.Logf("fixture payload=%d innerZIP=%d outerZIP=%d block=%d; index and source ranges are warm", len(payload), len(inner), len(outer), service.tree.nestedBlockBytes())
			totalBefore, wireBefore, started := cache.Stats().Foreground.RequestedBytes, wire.Load(), time.Now()
			var reader io.ReaderAt
			if shared {
				reader, _, err = service.OpenArchiveMember(lifetime, 77, "inner.zip/payload.bin")
				if err != nil {
					t.Fatal(err)
				}
				defer reader.(io.Closer).Close()
			}
			for _, offset := range []int64{8 << 20, 12 << 20, 16 << 20} {
				ctx, endRequest := context.WithTimeout(context.Background(), 30*time.Second)
				before, httpBefore, readStarted := cache.Stats().Foreground.RequestedBytes, wire.Load(), time.Now()
				if !shared {
					var size int64
					reader, size, err = service.OpenArchiveMember(ctx, 77, "inner.zip/payload.bin")
					if err != nil || size != int64(len(payload)) {
						endRequest()
						t.Fatalf("open offset=%d size=%d error=%v", offset, size, err)
					}
				}
				// The control also uses a new context for every read; retaining
				// one decoder must not retain the preceding canceled request.
				current := reader.(interface {
					ReadAtContext(context.Context, []byte, int64) (int, error)
				})
				got := make([]byte, 4096)
				n, readErr := current.ReadAtContext(ctx, got, offset)
				if !shared {
					_ = reader.(io.Closer).Close()
				}
				endRequest()
				if n != len(got) || readErr != nil || !bytes.Equal(got, payload[offset:offset+int64(len(got))]) {
					t.Fatalf("read offset=%d n=%d error=%v", offset, n, readErr)
				}
				t.Logf("offset=%d source_requested=%d HTTP_body=%d elapsed_us=%d", offset, cache.Stats().Foreground.RequestedBytes-before, wire.Load()-httpBefore, time.Since(readStarted).Microseconds())
			}
			requested, downloaded := cache.Stats().Foreground.RequestedBytes-totalBefore, wire.Load()-wireBefore
			t.Logf("total source_requested=%d HTTP_body=%d elapsed_us=%d", requested, downloaded, time.Since(started).Microseconds())
			if downloaded != 0 {
				t.Errorf("warmed reads downloaded %d HTTP bytes, want zero", downloaded)
			}
			// The same-handle control needs about 17 MiB including opening.
			// A bounded continuation between independent requests should have
			// similar source cost, rather than replaying about 39 MiB.
			if requested >= 20<<20 {
				t.Errorf("incremental reads requested %d source bytes, want <20 MiB after warming the index", requested)
			}
		})
	}
}

func nestedDeflateLeasePayload() []byte {
	payload := make([]byte, (32<<20)+13)
	state := uint64(0xabcdef987654321)
	for i := range payload {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		payload[i] = byte(state)
	}
	return payload
}

func nestedDeflateLeaseSource(t *testing.T, outer []byte) (*Service, *storage.Cache, *atomic.Int64) {
	t.Helper()
	wire := new(atomic.Int64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"synthetic-deflate-lease"`)
		http.ServeContent(serviceAESCopyCountWriter{w, wire}, r, "outer.zip", time.Unix(100, 0), bytes.NewReader(outer))
	}))
	t.Cleanup(server.Close)
	cache, err := storage.NewCache(t.TempDir(), 128<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	file := panapi.File{ID: 77, Name: "outer.zip", Size: int64(len(outer)), Version: "synthetic-v1"}
	api := &nestedDeflateLeaseAPI{&serviceNestedAPI{url: server.URL, files: []panapi.File{file}, detail: file}}
	lifetime, stopTree := context.WithCancel(context.Background())
	t.Cleanup(stopTree)
	service, err := NewService(lifetime, api, cache, 0, true, Options{DisableReadAhead: true, DisableStreamMembers: true, MaxConcurrentBuilds: 1})
	if err != nil {
		t.Fatal(err)
	}
	return service, cache, wire
}

func nestedDeflateLeaseReadRequest(ctx context.Context, service *Service, offset int64, payload []byte) error {
	reader, size, err := service.OpenArchiveMember(ctx, 77, "inner.zip/payload.bin")
	if err != nil {
		return err
	}
	defer reader.(io.Closer).Close()
	got := make([]byte, 4096)
	n, err := reader.ReadAt(got, offset)
	if err != nil {
		return err
	}
	if size != int64(len(payload)) || n != len(got) || !bytes.Equal(got, payload[offset:offset+int64(len(got))]) {
		return fmt.Errorf("payload mismatch offset=%d size=%d n=%d", offset, size, n)
	}
	return nil
}

func TestServiceNestedDeflateLeaseDoubleDeflateRequestsAndContexts(t *testing.T) {
	payload := nestedDeflateLeasePayload()
	inner := nestedDeflateLeaseZIP(t, "payload.bin", payload, zip.Deflate)
	outer := nestedDeflateLeaseZIP(t, "inner.zip", inner, zip.Deflate)
	service, cache, wire := nestedDeflateLeaseSource(t, outer)
	seed, finishSeed := context.WithTimeout(context.Background(), 30*time.Second)
	rows, err := service.ListArchive(seed, 77, "inner.zip")
	finishSeed()
	if err != nil || len(rows) != 1 || rows[0].Name != "payload.bin" {
		t.Fatalf("seed listing count=%d error=%v", len(rows), err)
	}
	before, httpBefore, started := cache.Stats().Foreground.RequestedBytes, wire.Load(), time.Now()
	for _, offset := range []int64{8 << 20, 12 << 20, 16 << 20} {
		ctx, endRequest := context.WithTimeout(context.Background(), 30*time.Second)
		stepBefore, stepHTTP, stepStarted := cache.Stats().Foreground.RequestedBytes, wire.Load(), time.Now()
		err := nestedDeflateLeaseReadRequest(ctx, service, offset, payload)
		endRequest()
		if err != nil {
			t.Fatalf("independent read offset=%d: %v", offset, err)
		}
		t.Logf("double_deflate offset=%d source_requested=%d HTTP_body=%d elapsed_us=%d", offset, cache.Stats().Foreground.RequestedBytes-stepBefore, wire.Load()-stepHTTP, time.Since(stepStarted).Microseconds())
	}
	requested, downloaded := cache.Stats().Foreground.RequestedBytes-before, wire.Load()-httpBefore
	t.Logf("double_deflate total source_requested=%d HTTP_body=%d elapsed_us=%d", requested, downloaded, time.Since(started).Microseconds())
	if requested >= 21<<20 {
		t.Errorf("double Deflate incremental reads requested %d source bytes, want <21 MiB", requested)
	}
	if downloaded != 0 {
		t.Errorf("warmed double Deflate reads downloaded %d HTTP bytes", downloaded)
	}
	// A canceled request must fail without poisoning the next independent
	// request or silently reviving the canceled context inside a continuation.
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := nestedDeflateLeaseReadRequest(canceled, service, 18<<20, payload); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled request error=%v", err)
	}
	ctx, endRequest := context.WithTimeout(context.Background(), 30*time.Second)
	err = nestedDeflateLeaseReadRequest(ctx, service, 18<<20, payload)
	endRequest()
	if err != nil {
		t.Fatalf("healthy request after cancellation: %v", err)
	}
	// Busy state may fall back to a private decoder. A single decode permit
	// must still allow every independent request to finish with correct bytes.
	errs := make(chan error, 4)
	for _, offset := range []int64{20 << 20, 22 << 20, 24 << 20, 26 << 20} {
		go func(off int64) {
			ctx, endRequest := context.WithTimeout(context.Background(), 30*time.Second)
			err := nestedDeflateLeaseReadRequest(ctx, service, off, payload)
			endRequest()
			errs <- err
		}(offset)
	}
	for i := 0; i < cap(errs); i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	// These earlier blocks were not requested before. A retained forward
	// decoder cannot rewind itself, and must use a safe fallback for the miss.
	for _, offset := range []int64{4 << 20, 2 << 20} {
		ctx, endRequest := context.WithTimeout(context.Background(), 30*time.Second)
		err := nestedDeflateLeaseReadRequest(ctx, service, offset, payload)
		endRequest()
		if err != nil {
			t.Fatalf("reverse miss offset=%d: %v", offset, err)
		}
	}
}

func TestServiceNestedDeflateLeaseDoesNotReuseCRCFailure(t *testing.T) {
	payload := nestedDeflateLeasePayload()[:(8<<20)+13]
	inner := nestedDeflateLeaseZIP(t, "payload.bin", payload, zip.Deflate)
	central := bytes.LastIndex(inner, []byte{'P', 'K', 1, 2})
	if central < 0 {
		t.Fatal("synthetic ZIP directory missing")
	}
	// Keep valid compressed bytes and outer CRC; only the inner member's
	// declared checksum is wrong, so sparse indexing remains successful.
	binary.LittleEndian.PutUint32(inner[central+16:], binary.LittleEndian.Uint32(inner[central+16:])^1)
	outer := nestedDeflateLeaseZIP(t, "inner.zip", inner, zip.Deflate)
	service, _, _ := nestedDeflateLeaseSource(t, outer)
	if rows, err := service.ListArchive(context.Background(), 77, "inner.zip"); err != nil || len(rows) != 1 {
		t.Fatalf("seed corrupt-member listing count=%d error=%v", len(rows), err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		ctx, endRequest := context.WithTimeout(context.Background(), 30*time.Second)
		reader, size, err := service.OpenArchiveMember(ctx, 77, "inner.zip/payload.bin")
		if err != nil {
			endRequest()
			t.Fatal(err)
		}
		if size != int64(len(payload)) {
			t.Fatalf("synthetic member size=%d", size)
		}
		prefix := make([]byte, 4096)
		if n, err := reader.ReadAt(prefix, 0); n != len(prefix) || err != nil || !bytes.Equal(prefix, payload[:len(prefix)]) {
			t.Fatalf("valid sparse prefix attempt=%d n=%d error=%v", attempt, n, err)
		}
		_, readErr := reader.ReadAt(make([]byte, len(payload)), 0)
		_ = reader.(io.Closer).Close()
		endRequest()
		if !errors.Is(readErr, zip.ErrChecksum) {
			t.Fatalf("complete read attempt=%d error=%v, want ZIP checksum failure", attempt, readErr)
		}
	}
}
