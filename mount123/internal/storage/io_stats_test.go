package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

type rangeScenario struct {
	Name              string `json:"name"`
	ElapsedNanos      int64  `json:"elapsed_nanos"`
	ReadBytes         uint64 `json:"read_bytes"`
	HTTPRequests      uint64 `json:"http_requests"`
	HTTPResponseBytes uint64 `json:"http_response_bytes"`
	ReadBytesPerSec   uint64 `json:"read_bytes_per_second"`
}

func measureScenario(name string, requests, responseBytes *atomic.Uint64, readBytes uint64, run func() error) (rangeScenario, error) {
	startRequests, startResponseBytes := requests.Load(), responseBytes.Load()
	started := time.Now()
	err := run()
	elapsed := time.Since(started)
	scenario := rangeScenario{
		Name:              name,
		ElapsedNanos:      elapsed.Nanoseconds(),
		ReadBytes:         readBytes,
		HTTPRequests:      requests.Load() - startRequests,
		HTTPResponseBytes: responseBytes.Load() - startResponseBytes,
	}
	if elapsed > 0 {
		scenario.ReadBytesPerSec = uint64(float64(readBytes) / elapsed.Seconds())
	}
	return scenario, err
}

func TestIOStatsLocalRangeColdHotMultiFileAndSlowConsumer(t *testing.T) {
	data := make([]byte, 4<<20)
	for i := range data {
		data[i] = byte(i * 31)
	}
	var requests, responseBytes atomic.Uint64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var start, end int64
		if _, err := fmt.Sscanf(req.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil || start < 0 || end < start || end >= int64(len(data)) {
			http.Error(w, "invalid range", http.StatusBadRequest)
			return
		}
		requests.Add(1)
		time.Sleep(2 * time.Millisecond) // stable, local response latency
		w.Header().Set("ETag", `"range-test-v1"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		n, _ := w.Write(data[start : end+1])
		responseBytes.Add(uint64(n))
	}))
	defer server.Close()

	cache, err := NewCache(t.TempDir(), 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	newRemote := func(key string) *Remote {
		t.Helper()
		remote, err := NewRemote(context.Background(), cache, key, int64(len(data)), func(context.Context) (string, error) { return server.URL, nil })
		if err != nil {
			t.Fatal(err)
		}
		return remote
	}

	// Cold read fills a range; repeating it should be served from the cache.
	remote := newRemote("cold-hot")
	first := make([]byte, 256<<10)
	var scenarios []rangeScenario
	cold, err := measureScenario("cold", &requests, &responseBytes, uint64(len(first)), func() error {
		_, readErr := remote.ReadAtContext(context.Background(), first, 0)
		return readErr
	})
	if err != nil {
		t.Fatal(err)
	}
	scenarios = append(scenarios, cold)
	afterCold := requests.Load()
	second := make([]byte, len(first))
	hot, err := measureScenario("hot", &requests, &responseBytes, uint64(len(second)), func() error {
		_, readErr := remote.ReadAtContext(context.Background(), second, 0)
		return readErr
	})
	if err != nil {
		t.Fatal(err)
	}
	scenarios = append(scenarios, hot)
	if requests.Load() != afterCold {
		t.Fatalf("hot read issued another HTTP request: %d -> %d", afterCold, requests.Load())
	}

	// Four distinct files contend on the same local Range source.
	var reads atomic.Uint64
	done := make(chan error, 4)
	multiStartRequests, multiStartBytes, multiStarted := requests.Load(), responseBytes.Load(), time.Now()
	for i := 0; i < 4; i++ {
		go func(i int) {
			buf := make([]byte, 128<<10)
			_, readErr := newRemote(fmt.Sprintf("parallel-%d", i)).ReadAtContext(context.Background(), buf, int64(i)*(1<<20))
			if readErr == nil {
				reads.Add(uint64(len(buf)))
			}
			done <- readErr
		}(i)
	}
	for range 4 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if reads.Load() != 4*(128<<10) {
		t.Fatalf("parallel bytes read = %d", reads.Load())
	}
	multiElapsed := time.Since(multiStarted)
	multi := rangeScenario{Name: "multi_file", ElapsedNanos: multiElapsed.Nanoseconds(), ReadBytes: reads.Load(), HTTPRequests: requests.Load() - multiStartRequests, HTTPResponseBytes: responseBytes.Load() - multiStartBytes}
	if multiElapsed > 0 {
		multi.ReadBytesPerSec = uint64(float64(multi.ReadBytes) / multiElapsed.Seconds())
	}
	scenarios = append(scenarios, multi)

	// A deliberately slow consumer gives a repeatable consumer-paced pattern.
	slow := newRemote("slow-consumer")
	var slowRead uint64
	slowScenario, err := measureScenario("slow_consumer", &requests, &responseBytes, 4*(64<<10), func() error {
		for i := 0; i < 4; i++ {
			buf := make([]byte, 64<<10)
			if _, readErr := slow.ReadAtContext(context.Background(), buf, int64(i)*(1<<20)); readErr != nil {
				return readErr
			}
			slowRead += uint64(len(buf))
			time.Sleep(3 * time.Millisecond)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slowScenario.ReadBytes = slowRead
	if slowScenario.ElapsedNanos > 0 {
		slowScenario.ReadBytesPerSec = uint64(float64(slowRead) / time.Duration(slowScenario.ElapsedNanos).Seconds())
	}
	scenarios = append(scenarios, slowScenario)

	stats := cache.IOStats().Snapshot()
	if stats.DownloadedBytes.Status != "measured" || stats.DownloadedBytes.Bytes == nil || *stats.DownloadedBytes.Bytes == 0 {
		t.Fatalf("download byte metric = %#v", stats.DownloadedBytes)
	}
	if stats.CacheHitBytes.Status != "measured" || stats.CacheHitBytes.Bytes == nil || *stats.CacheHitBytes.Bytes < uint64(len(first)) {
		t.Fatalf("cache hit byte metric = %#v", stats.CacheHitBytes)
	}
	if stats.HTTPBodyTTFB.Status != "measured" || stats.HTTPTransferLatency.Status != "measured" {
		t.Fatalf("HTTP timing metrics = body TTFB %#v, transfer %#v", stats.HTTPBodyTTFB, stats.HTTPTransferLatency)
	}
	if stats.CachePublication.Status != "measured" {
		t.Fatalf("cache publication metric = %#v", stats.CachePublication)
	}
	if requests.Load() == 0 || responseBytes.Load() == 0 {
		t.Fatal("local Range source did not serve any bytes")
	}
	encoded, err := json.Marshal(stats)
	if err != nil {
		t.Fatal(err)
	}
	metrics, err := json.Marshal(struct {
		Scenarios []rangeScenario `json:"scenarios"`
		Stats     any             `json:"stats"`
	}{Scenarios: scenarios, Stats: json.RawMessage(encoded)})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("local Range baseline: %s", metrics)
}

func TestIOStatsCountsIncompleteHTTPBodyBytes(t *testing.T) {
	var calls atomic.Uint64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("ETag", `"short-body"`)
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Range", "bytes 0-0/8")
			w.Header().Set("Content-Length", "1")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, "x")
			return
		}
		w.Header().Set("Content-Range", "bytes 0-7/8")
		w.Header().Set("Content-Length", "8")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "x")
	}))
	defer server.Close()
	cache, err := NewCache(t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	remote, err := NewRemote(context.Background(), cache, "short", 8, func(context.Context) (string, error) { return server.URL, nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = remote.ReadAtContext(context.Background(), make([]byte, 8), 0); err == nil {
		t.Fatal("ReadAt accepted a short response body")
	}
	stats := cache.IOStats().Snapshot()
	if stats.DownloadedBytes.Status != "measured" || stats.DownloadedBytes.Bytes == nil || *stats.DownloadedBytes.Bytes == 0 {
		t.Fatalf("partial body bytes were not measured: %#v", stats.DownloadedBytes)
	}
}
