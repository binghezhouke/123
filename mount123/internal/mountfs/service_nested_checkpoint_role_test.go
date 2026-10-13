package mountfs

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
)

// Opening the same member as an ordinary file first must not lend its standard
// decoder to the later child-index request: that request needs the collector.
// The demanded blocks remain shared across both roles.
func TestServiceNestedCheckpointCollectorAfterLeafLease(t *testing.T) {
	payload := nestedDeflateLeasePayload()
	inner := nestedDeflateLeaseZIP(t, "payload.bin", payload, zip.Store)
	middle := nestedDeflateLeaseZIP(t, "inner.zip", inner, zip.Deflate)
	outer := nestedDeflateLeaseZIP(t, "middle.zip", middle, zip.Store)
	for _, tailFirst := range []bool{false, true} {
		label := "head-first"
		if tailFirst {
			label = "tail-first"
		}
		t.Run(label, func(t *testing.T) {
			var wire atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", `"synthetic-checkpoint-role"`)
				http.ServeContent(serviceAESCopyCountWriter{w, &wire}, r, "outer.zip", time.Unix(100, 0), bytes.NewReader(outer))
			}))
			defer server.Close()
			dir := t.TempDir()
			file := panapi.File{ID: 77, Name: "outer.zip", Size: int64(len(outer)), Version: "synthetic-role-v1"}
			api := &nestedDeflateLeaseAPI{&serviceNestedAPI{url: server.URL, files: []panapi.File{file}, detail: file}}
			start := func() (*Service, *storage.Cache, func()) {
				t.Helper()
				cache, err := storage.NewCache(dir, 128<<20)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				service, err := NewService(ctx, api, cache, 0, true, Options{DisableReadAhead: true, DisableStreamMembers: true, MaxConcurrentBuilds: 1})
				if err != nil {
					cancel()
					cache.Close()
					t.Fatal(err)
				}
				return service, cache, func() {
					cancel()
					service.tree.nestedDeflatePool.Close()
					cache.Close()
				}
			}
			service, _, stop := start()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			leaf, size, err := service.OpenArchiveMember(ctx, 77, "middle.zip/inner.zip")
			if err != nil {
				cancel()
				stop()
				t.Fatal(err)
			}
			first := make([]byte, 32)
			leafOffset := int64(0)
			if tailFirst {
				leafOffset = size - int64(len(first))
			}
			n, readErr := leaf.ReadAt(first, leafOffset)
			closeErr := leaf.(io.Closer).Close()
			cancel()
			if size != int64(len(inner)) || n != len(first) || readErr != nil || closeErr != nil || !bytes.Equal(first, inner[leafOffset:leafOffset+int64(len(first))]) {
				stop()
				t.Fatalf("ordinary member read size=%d n=%d error=%v close=%v", size, n, readErr, closeErr)
			}
			ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
			rows, err := service.ListArchive(ctx, 77, "middle.zip/inner.zip")
			cancel()
			stop()
			if err != nil || len(rows) != 1 || rows[0].Name != "payload.bin" {
				t.Fatalf("child index count=%d error=%v", len(rows), err)
			}
			service, cache, stop := start()
			defer stop()
			service.tree.nestedDeflatePool.Close()
			before, wireBefore := cache.Stats().Foreground.RequestedBytes, wire.Load()
			for _, offset := range []int64{24 << 20, 8 << 20, 20 << 20, 4 << 20} {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				reader, size, err := service.OpenArchiveMember(ctx, 77, "middle.zip/inner.zip/payload.bin")
				if err != nil {
					cancel()
					t.Fatal(err)
				}
				got := make([]byte, 4096)
				n, readErr := reader.ReadAt(got, offset)
				closeErr := reader.(io.Closer).Close()
				cancel()
				if size != int64(len(payload)) || n != len(got) || readErr != nil || closeErr != nil || !bytes.Equal(got, payload[offset:offset+int64(len(got))]) {
					t.Fatalf("restart seek offset=%d size=%d n=%d error=%v close=%v", offset, size, n, readErr, closeErr)
				}
			}
			requested, downloaded := cache.Stats().Foreground.RequestedBytes-before, wire.Load()-wireBefore
			t.Logf("leaf then child index, restart seeks: source_requested=%d HTTP_body=%d", requested, downloaded)
			if requested >= 24<<20 {
				t.Errorf("child collector lost after ordinary member lease: requested %d bytes, want <24 MiB", requested)
			}
			if downloaded != 0 {
				t.Errorf("warm immutable ranges downloaded %d bytes", downloaded)
			}
		})
	}
}
