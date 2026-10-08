package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
)

const capacityBaselineObjectSize = 1 << 20
const capacityBaselineObjects = 24

type capacityBaselineMetrics struct {
	requests atomic.Int64
	body     atomic.Int64
}

type capacityBaselineSnapshot struct {
	requests int64
	body     int64
}

func (m *capacityBaselineMetrics) snapshot() capacityBaselineSnapshot {
	return capacityBaselineSnapshot{requests: m.requests.Load(), body: m.body.Load()}
}

func (s capacityBaselineSnapshot) since(before capacityBaselineSnapshot) capacityBaselineSnapshot {
	return capacityBaselineSnapshot{requests: s.requests - before.requests, body: s.body - before.body}
}

type capacityBaselineResult struct {
	firstPass  capacityBaselineSnapshot
	secondPass capacityBaselineSnapshot
}

// TestCacheCapacityWorkingSetBaseline compares capacities using a scaled
// working set: 24 distinct 1 MiB objects, with a complete foreground read of
// every object in each pass. The 20/50 MiB limits model only the capacity ratio
// of a GiB-scale cache; they do not predict cloud throughput or recommend a
// production cache size. This measures local Range HTTP traffic only.
func TestCacheCapacityWorkingSetBaseline(t *testing.T) {
	objects := make([][]byte, capacityBaselineObjects)
	for object := range objects {
		data := make([]byte, capacityBaselineObjectSize)
		for i := range data {
			data[i] = byte((object*37 + i*17 + i/251) % 256)
		}
		objects[object] = data
	}

	var metrics capacityBaselineMetrics
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var object int
		if _, err := fmt.Sscanf(r.URL.Path, "/object/%d", &object); err != nil || object < 0 || object >= len(objects) {
			http.NotFound(w, r)
			return
		}
		var start, end int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil || start < 0 || end < start || end >= int64(len(objects[object])) {
			http.Error(w, "invalid range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		body := objects[object][start : end+1]
		w.Header().Set("ETag", fmt.Sprintf("\"object-%02d-v1\"", object))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(objects[object])))
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusPartialContent)
		n, err := w.Write(body)
		metrics.requests.Add(1)
		metrics.body.Add(int64(n))
		if err != nil {
			return
		}
	}))
	defer server.Close()

	run := func(t *testing.T, cacheLimit int64) capacityBaselineResult {
		t.Helper()
		cache, err := NewCache(t.TempDir(), cacheLimit)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := cache.Close(); err != nil {
				t.Errorf("close cache: %v", err)
			}
		}()

		remotes := make([]*Remote, len(objects))
		for object := range remotes {
			object := object
			remote, err := NewRemote(context.Background(), cache, fmt.Sprintf("capacity-baseline-object-%02d", object), capacityBaselineObjectSize, func(context.Context) (string, error) {
				return fmt.Sprintf("%s/object/%d", server.URL, object), nil
			})
			if err != nil {
				t.Fatalf("create remote %d: %v", object, err)
			}
			remotes[object] = remote
		}

		// Exclude NewRemote's one-byte endpoint probes; the measured totals below
		// are only the payload bytes transferred for the repeated workload.
		measurements := make([]capacityBaselineSnapshot, 2)
		for pass := range measurements {
			passStart := metrics.snapshot()
			for object, remote := range remotes {
				if err := remote.PrefetchRangeAtContext(context.Background(), 0, capacityBaselineObjectSize); err != nil {
					t.Fatalf("pass %d prefetch object %d: %v", pass+1, object, err)
				}
				got := make([]byte, capacityBaselineObjectSize)
				n, err := remote.ReadRangeAtContext(context.Background(), got, 0)
				if err != nil && err != io.EOF {
					t.Fatalf("pass %d read object %d: n=%d err=%v", pass+1, object, n, err)
				}
				if n != len(got) || !bytes.Equal(got, objects[object]) {
					t.Fatalf("pass %d object %d content mismatch: read %d bytes", pass+1, object, n)
				}
			}
			measurements[pass] = metrics.snapshot().since(passStart)
		}
		return capacityBaselineResult{firstPass: measurements[0], secondPass: measurements[1]}
	}

	small := run(t, 20<<20)
	large := run(t, 50<<20)
	t.Logf("working set: %d x %d MiB objects (%d MiB total); small cache=20 MiB first=%+v second=%+v; large cache=50 MiB first=%+v second=%+v",
		capacityBaselineObjects, capacityBaselineObjectSize>>20, capacityBaselineObjects, small.firstPass, small.secondPass, large.firstPass, large.secondPass)

	wantOnePass := capacityBaselineSnapshot{requests: capacityBaselineObjects, body: int64(capacityBaselineObjects * capacityBaselineObjectSize)}
	if small.firstPass != wantOnePass || large.firstPass != wantOnePass {
		t.Fatalf("single-pass traffic: 20 MiB=%+v 50 MiB=%+v; want both %+v", small.firstPass, large.firstPass, wantOnePass)
	}
	if small.secondPass.body <= large.secondPass.body {
		t.Fatalf("second pass did not benefit from fitting the working set: 20 MiB=%+v 50 MiB=%+v", small.secondPass, large.secondPass)
	}
	if large.secondPass.body != 0 || large.secondPass.requests != 0 {
		t.Fatalf("50 MiB cache should retain this 24 MiB working set; second pass=%+v", large.secondPass)
	}
	if small.secondPass.body != int64(capacityBaselineObjects*capacityBaselineObjectSize) {
		t.Fatalf("expected 20 MiB cache to refetch the 24 MiB cyclic working set; second pass=%+v", small.secondPass)
	}
}
