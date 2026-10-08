package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/binghezhouke/123/mount123/internal/workqueue"
)

func TestCachePolicyRemoteScanKeepsHotExtentAndArchiveIndex(t *testing.T) {
	const scanSize = 5 * remoteBlockSize
	hotBytes := bytes.Repeat([]byte("H"), int(remoteBlockSize))
	scanBytes := bytes.Repeat([]byte("S"), int(scanSize))
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var data []byte
		switch req.URL.Path {
		case "/hot":
			data = hotBytes
		case "/scan":
			data = scanBytes
		default:
			http.NotFound(w, req)
			return
		}
		var start, end int64
		if _, err := fmt.Sscanf(req.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil || start < 0 || end < start || end >= int64(len(data)) {
			http.Error(w, "invalid range", http.StatusBadRequest)
			return
		}
		requests.Add(1)
		w.Header().Set("ETag", `"stable"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : end+1])
	}))
	defer server.Close()

	dir := t.TempDir()
	const cacheLimit = 2*remoteBlockSize + 8<<10
	c, err := NewCache(dir, cacheLimit)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	indexKey := "archive-index:opaque-test-key"
	indexData := bytes.Repeat([]byte("I"), 4<<10)
	if err := c.StoreArchiveIndex(ctx, indexKey, indexData); err != nil {
		t.Fatal(err)
	}
	hot, err := NewRemote(ctx, c, "hot-image", int64(len(hotBytes)), func(context.Context) (string, error) { return server.URL + "/hot", nil })
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, remoteCachePageSize)
	for i := 0; i < 2; i++ {
		if n, err := hot.ReadAt(buf, 0); err != nil || n != len(buf) {
			t.Fatalf("warm hot read %d: n=%d err=%v", i, n, err)
		}
	}
	hotID := cacheID(rangeKey(hot.rangeID, 0, remoteBlockSize))
	if got := c.entries[hotID]; got == nil || got.class != cacheHot {
		t.Fatalf("repeated foreground read class = %v, want hot", got)
	}

	scan, err := NewRemote(ctx, c, "large-scan", int64(len(scanBytes)), func(context.Context) (string, error) { return server.URL + "/scan", nil })
	if err != nil {
		t.Fatal(err)
	}
	for off := int64(0); off < scanSize; off += remoteCachePageSize {
		if n, err := scan.ReadAt(buf, off); err != nil || n != len(buf) {
			t.Fatalf("scan read at %d: n=%d err=%v", off, n, err)
		}
	}
	if c.used > c.max || c.reserved != 0 {
		t.Fatalf("capacity invariant: used=%d reserved=%d max=%d", c.used, c.reserved, c.max)
	}
	if c.entries[hotID] == nil || c.entries[hotID].class != cacheHot {
		t.Fatal("large sequential scan evicted the repeated hot extent")
	}
	if c.entries[cacheID(indexKey)] == nil || c.entries[cacheID(indexKey)].class != cacheIndex {
		t.Fatal("large sequential scan evicted the protected archive index")
	}
	firstScanID := cacheID(rangeKey(scan.rangeID, 0, remoteBlockSize))
	if c.entries[firstScanID] != nil {
		t.Fatal("old single-pass scan extent survived over-budget scan")
	}
	lastScanID := cacheID(rangeKey(scan.rangeID, 4*remoteBlockSize, scanSize))
	if e := c.entries[lastScanID]; e == nil || e.class == cacheHot {
		t.Fatalf("one-pass extent class = %v, want probationary", e)
	}
	before := requests.Load()
	if n, err := hot.ReadAt(buf, 0); err != nil || n != len(buf) {
		t.Fatalf("hot reread after scan: n=%d err=%v", n, err)
	}
	if requests.Load() != before {
		t.Fatal("hot reread after scan issued another HTTP range")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	// Class suffixes preserve the retention decisions across a durable restart.
	c, err = NewCache(dir, cacheLimit)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if e := c.entries[hotID]; e == nil || e.class != cacheHot {
		t.Fatalf("restarted hot class = %v, want hot", e)
	}
	if e := c.entries[cacheID(indexKey)]; e == nil || e.class != cacheIndex {
		t.Fatalf("restarted index class = %v, want index", e)
	}
	index, err := c.OpenArchiveIndex(indexKey)
	if err != nil {
		t.Fatalf("reopen persisted index: %v", err)
	}
	gotIndex := make([]byte, len(indexData))
	if n, err := index.ReadAt(gotIndex, 0); err != nil || n != len(gotIndex) || !bytes.Equal(gotIndex, indexData) {
		t.Fatalf("read persisted index: n=%d err=%v", n, err)
	}
	_ = index.Close()
	beforeProbe := requests.Load()
	hotAfterRestart, err := NewRemote(ctx, c, "hot-image", int64(len(hotBytes)), func(context.Context) (string, error) { return server.URL + "/hot", nil })
	if err != nil {
		t.Fatal(err)
	}
	probeCount := requests.Load()
	if probeCount <= beforeProbe {
		t.Fatal("restart did not issue the expected one-byte entity probe")
	}
	if n, err := hotAfterRestart.ReadAt(buf, 0); err != nil || n != len(buf) {
		t.Fatalf("reopened hot read: n=%d err=%v", n, err)
	}
	if requests.Load() != probeCount {
		t.Fatal("reopened hot extent did not reuse the durable cache")
	}
}

func TestCachePolicyOverBudgetIndexDegradesToOrdinaryClass(t *testing.T) {
	c, err := NewCache(t.TempDir(), 128)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.StoreArchiveIndex(context.Background(), "small-index", bytes.Repeat([]byte("i"), 8)); err != nil {
		t.Fatal(err)
	}
	if err := c.StoreArchiveIndex(context.Background(), "large-index", bytes.Repeat([]byte("i"), 20)); err != nil {
		t.Fatalf("index within total budget should still cache: %v", err)
	}
	if got := c.entries[cacheID("small-index")].class; got != cacheIndex {
		t.Fatalf("small index class = %v, want protected index", got)
	}
	if got := c.entries[cacheID("large-index")].class; got != cacheProbation {
		t.Fatalf("over-share index class = %v, want ordinary probationary", got)
	}
	if c.used != 28 || c.used > c.max {
		t.Fatalf("cache capacity = used %d max %d", c.used, c.max)
	}
}

func TestCachePolicyBackgroundAcquireStaysSpeculative(t *testing.T) {
	c, err := NewCache(t.TempDir(), 32)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	key := "background-member"
	h, err := c.Acquire(workqueue.Background(context.Background()), key, 8, func(_ context.Context, w io.Writer) error {
		_, err := io.WriteString(w, "12345678")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := h.ReadAt(make([]byte, 8), 0); err != nil || n != 8 {
		t.Fatalf("background handle read: %d %v", n, err)
	}
	_ = h.Close()
	if got := c.entries[cacheID(key)].class; got != cacheSpeculative {
		t.Fatalf("background acquisition class = %v, want speculative", got)
	}
}

func TestCachePolicyEphemeralCacheUsesClassAdmissionAndCleansUp(t *testing.T) {
	root := t.TempDir()
	c, err := NewEphemeralCache(root, 128)
	if err != nil {
		t.Fatal(err)
	}
	dir := c.Directory()
	if err := c.StoreArchiveIndex(context.Background(), "ephemeral-index", bytes.Repeat([]byte("i"), 8)); err != nil {
		t.Fatal(err)
	}
	if c.entries[cacheID("ephemeral-index")].class != cacheIndex || c.used > c.max {
		t.Fatalf("ephemeral admission class=%v used=%d max=%d", c.entries[cacheID("ephemeral-index")].class, c.used, c.max)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("ephemeral cache directory still exists: %v", err)
	}
}

func TestCachePolicyLoadsLegacyBlobAsProbation(t *testing.T) {
	dir := t.TempDir()
	key := "legacy-opaque-cache-key"
	data := []byte("legacy content")
	hash := sha256.Sum256([]byte(key))
	legacyPath := dir + "/" + hex.EncodeToString(hash[:]) + ".blob"
	if err := os.WriteFile(legacyPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	c, err := NewCache(dir, 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	entry := c.entries[cacheID(key)]
	if entry == nil || entry.class != cacheProbation {
		t.Fatalf("legacy blob entry = %+v, want probation", entry)
	}
	h, err := c.Open(key)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(data))
	if n, err := h.ReadAt(got, 0); err != nil || n != len(data) || !bytes.Equal(got, data) {
		t.Fatalf("legacy blob read: n=%d err=%v data=%q", n, err, got)
	}
	_ = h.Close()
	if entry.class != cacheProbation {
		t.Fatalf("first legacy read promoted class to %v", entry.class)
	}
}
