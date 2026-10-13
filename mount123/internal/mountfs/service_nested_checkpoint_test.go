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

// Building the inner index necessarily verifies the outer member through EOF.
// A fresh Tree and cache process must reuse its verified checkpoints, rather
// than replay the outer Deflate prefix for each independent random seek.
func TestServiceNestedDeflateCheckpointsSurviveRestart(t *testing.T) {
	payload := nestedDeflateLeasePayload()
	inner := nestedDeflateLeaseZIP(t, "payload.bin", payload, zip.Store)
	outer := nestedDeflateLeaseZIP(t, "inner.zip", inner, zip.Deflate)
	var wire atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"synthetic-checkpoint"`)
		http.ServeContent(serviceAESCopyCountWriter{w, &wire}, r, "outer.zip", time.Unix(100, 0), bytes.NewReader(outer))
	}))
	defer server.Close()
	dir := t.TempDir()
	file := panapi.File{ID: 77, Name: "outer.zip", Size: int64(len(outer)), Version: "synthetic-checkpoint-v1"}
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
	rows, err := service.ListArchive(ctx, 77, "inner.zip")
	cancel()
	stop()
	if err != nil || len(rows) != 1 {
		t.Fatalf("seed index count=%d error=%v", len(rows), err)
	}
	service, cache, stop := start()
	defer stop()
	// Disable the in-memory continuation pool as an additional control: this
	// assertion specifically measures persistent checkpoints, not a live reader.
	service.tree.nestedDeflatePool.Close()
	before, wireBefore := cache.Stats().Foreground.RequestedBytes, wire.Load()
	for _, offset := range []int64{24 << 20, 8 << 20, 20 << 20, 4 << 20} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		reader, size, err := service.OpenArchiveMember(ctx, 77, "inner.zip/payload.bin")
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		got := make([]byte, 4096)
		n, readErr := reader.ReadAt(got, offset)
		closeErr := reader.(io.Closer).Close()
		cancel()
		if size != int64(len(payload)) || n != len(got) || readErr != nil || closeErr != nil || !bytes.Equal(got, payload[offset:offset+int64(len(got))]) {
			t.Fatalf("seek offset=%d n=%d error=%v close=%v", offset, n, readErr, closeErr)
		}
	}
	requested, downloaded := cache.Stats().Foreground.RequestedBytes-before, wire.Load()-wireBefore
	t.Logf("four seeks after restart: source_requested=%d HTTP_body=%d", requested, downloaded)
	if requested >= 24<<20 {
		t.Errorf("replayed source prefixes after restart: %d bytes, want <24 MiB", requested)
	}
	if downloaded != 0 {
		t.Errorf("warm persisted ranges downloaded %d bytes", downloaded)
	}
}
