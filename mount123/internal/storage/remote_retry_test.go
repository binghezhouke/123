package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/faults"
)

func retryFixture(t *testing.T, size int64, handler http.Handler) (*Cache, *Remote, func()) {
	t.Helper()
	srv := httptest.NewServer(handler)
	c, err := NewCache(t.TempDir(), 4<<20)
	if err != nil {
		srv.Close()
		t.Fatal(err)
	}
	r, err := NewRemote(context.Background(), c, "retry-fixture", size, func(context.Context) (string, error) { return srv.URL, nil })
	if err != nil {
		c.Close()
		srv.Close()
		t.Fatal(err)
	}
	return c, r, func() { _ = c.Close(); srv.Close() }
}

func writeRange(w http.ResponseWriter, req *http.Request, data []byte, etag string) {
	var start, end int64
	if _, err := fmt.Sscanf(req.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
		http.Error(w, "bad range", http.StatusBadRequest)
		return
	}
	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(data[start : end+1])
}

func waitRemoteRecoveryStats(t *testing.T, cache *Cache, ready func(RemoteRecoveryStats) bool) RemoteRecoveryStats {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		stats := cache.RemoteRecoveryStats()
		if ready(stats) {
			return stats
		}
		if time.Now().After(deadline) {
			t.Fatalf("recovery stats did not settle: %+v", stats)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRemoteRecoveryRetries503And429(t *testing.T) {
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusTooManyRequests} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			data := []byte("recovered body")
			var calls atomic.Int32
			handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				call := calls.Add(1)
				if call == 2 {
					if status == http.StatusTooManyRequests {
						w.Header().Set("Retry-After", "0")
					}
					w.WriteHeader(status)
					return
				}
				writeRange(w, req, data, `"v1"`)
			})
			cache, remote, closeAll := retryFixture(t, int64(len(data)), handler)
			defer closeAll()
			got := make([]byte, len(data))
			if n, err := remote.ReadRangeAtContext(context.Background(), got, 0); err != nil || n != len(got) {
				t.Fatalf("read = %d, %v", n, err)
			}
			if !bytes.Equal(got, data) {
				t.Fatalf("read bytes = %q, want %q", got, data)
			}
			stats := waitRemoteRecoveryStats(t, cache, func(s RemoteRecoveryStats) bool { return s.Recovered > 0 })
			if stats.Retries != 1 || stats.Recovered != 1 {
				t.Fatalf("recovery stats = %+v", stats)
			}
		})
	}
}

func TestRemoteRecoveryCountsAuthRefreshSuccess(t *testing.T) {
	data := []byte("auth refresh recovered")
	var calls atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if calls.Add(1) == 2 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeRange(w, req, data, `"v1"`)
	})
	cache, remote, closeAll := retryFixture(t, int64(len(data)), handler)
	defer closeAll()
	got := make([]byte, len(data))
	if n, err := remote.ReadRangeAtContext(context.Background(), got, 0); err != nil || n != len(got) {
		t.Fatalf("read = %d, %v", n, err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("read bytes = %q, want %q", got, data)
	}
	stats := waitRemoteRecoveryStats(t, cache, func(s RemoteRecoveryStats) bool { return s.Recovered > 0 })
	if stats.Retries != 1 || stats.Recovered != 1 {
		t.Fatalf("auth recovery stats = %+v", stats)
	}
}

func TestRemoteAuthRefreshAndTransientResponsesShareRetryCount(t *testing.T) {
	data := []byte("content")
	var calls atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		call := calls.Add(1)
		if call == 1 {
			writeRange(w, req, data, `"v1"`)
			return
		}
		switch call {
		case 2, 4:
			w.WriteHeader(http.StatusUnauthorized)
		case 3, 5:
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			writeRange(w, req, data, `"v1"`)
		}
	})
	cache, remote, closeAll := retryFixture(t, int64(len(data)), handler)
	defer closeAll()
	_, err := remote.ReadRangeAtContext(context.Background(), make([]byte, len(data)), 0)
	if !faults.IsTransient(err) || faults.KindOf(err) != faults.Unavailable {
		t.Fatalf("read error = %v, kind=%q transient=%t", err, faults.KindOf(err), faults.IsTransient(err))
	}
	stats := cache.RemoteRecoveryStats()
	if stats.Attempts != 5 || stats.Retries != remoteRecoveryRetries {
		t.Fatalf("shared retry budget stats = %+v", stats)
	}
	if calls.Load() != 5 {
		t.Fatalf("HTTP calls = %d, want probe plus four bounded calls", calls.Load())
	}
}

func TestRemoteRecoveryResumesInterruptedBodyWithSameValidator(t *testing.T) {
	data := []byte("the response body resumes at its missing suffix")
	var calls atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		call := calls.Add(1)
		var start, end int64
		_, _ = fmt.Sscanf(req.Header.Get("Range"), "bytes=%d-%d", &start, &end)
		if call == 2 {
			w.Header().Set("ETag", `"v1"`)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
			w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(data[start : start+7])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			return
		}
		if call > 2 {
			if req.Header.Get("If-Match") != `"v1"` {
				t.Errorf("resume If-Match = %q", req.Header.Get("If-Match"))
			}
			if start != 7 {
				t.Errorf("resume range starts at %d, want 7", start)
			}
		}
		writeRange(w, req, data, `"v1"`)
	})
	cache, remote, closeAll := retryFixture(t, int64(len(data)), handler)
	defer closeAll()
	got := make([]byte, len(data))
	if n, err := remote.ReadRangeAtContext(context.Background(), got, 0); err != nil || n != len(got) {
		t.Fatalf("read = %d, %v", n, err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("read bytes = %q, want %q", got, data)
	}
	if stats := waitRemoteRecoveryStats(t, cache, func(s RemoteRecoveryStats) bool { return s.Recovered > 0 }); stats.SuffixBytes == 0 || stats.Recovered != 1 {
		t.Fatalf("recovery stats = %+v", stats)
	}
}

func TestRemoteRecoveryDoesNotCountUnvalidatedBodyAsRecovered(t *testing.T) {
	data := []byte("body must be complete and validated")
	var calls atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		call := calls.Add(1)
		var start, end int64
		_, _ = fmt.Sscanf(req.Header.Get("Range"), "bytes=%d-%d", &start, &end)
		if call == 2 {
			w.Header().Set("ETag", `"v1"`)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
			w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(data[start : start+6])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			return
		}
		if call > 2 {
			writeRange(w, req, data, `"v2"`)
			return
		}
		writeRange(w, req, data, `"v1"`)
	})
	cache, remote, closeAll := retryFixture(t, int64(len(data)), handler)
	defer closeAll()
	_, err := remote.ReadRangeAtContext(context.Background(), make([]byte, len(data)), 0)
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("read error = %v, want remote changed", err)
	}
	if got := cache.RemoteRecoveryStats().Recovered; got != 0 {
		t.Fatalf("recovered = %d after invalid body, want 0", got)
	}
}

func TestRemoteRecoveryRejectsChangedValidatorOnResume(t *testing.T) {
	data := []byte("entity version must remain stable")
	var calls atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		call := calls.Add(1)
		var start, end int64
		_, _ = fmt.Sscanf(req.Header.Get("Range"), "bytes=%d-%d", &start, &end)
		if call == 2 {
			w.Header().Set("ETag", `"v1"`)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
			w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(data[start : start+5])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			return
		}
		if call > 2 {
			writeRange(w, req, data, `"v2"`)
			return
		}
		writeRange(w, req, data, `"v1"`)
	})
	_, remote, closeAll := retryFixture(t, int64(len(data)), handler)
	defer closeAll()
	_, err := remote.ReadRangeAtContext(context.Background(), make([]byte, len(data)), 0)
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("read error = %v, want remote changed", err)
	}
}

func TestRemoteRecoveryDoesNotRequestEmptySuffixAfterExactBytes(t *testing.T) {
	data := []byte("all requested bytes arrive before reset")
	var calls atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		call := calls.Add(1)
		if call == 1 {
			writeRange(w, req, data, `"v1"`)
			return
		}
		var start, end int64
		_, _ = fmt.Sscanf(req.Header.Get("Range"), "bytes=%d-%d", &start, &end)
		if start != 0 || end != int64(len(data))-1 {
			t.Errorf("unexpected retry range %q", req.Header.Get("Range"))
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("HTTP server does not support hijacking")
			return
		}
		conn, rw, err := hijacker.Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_, _ = fmt.Fprintf(rw, "HTTP/1.1 206 Partial Content\r\nETag: \"v1\"\r\nContent-Range: bytes 0-%d/%d\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n%x\r\n", len(data)-1, len(data), len(data))
		_, _ = rw.Write(data)
		_, _ = rw.Write([]byte("\r\n"))
		_ = rw.Flush()
		if tcp, ok := conn.(interface{ SetLinger(int) error }); ok {
			_ = tcp.SetLinger(0)
		}
		_ = conn.Close()
	})
	_, remote, closeAll := retryFixture(t, int64(len(data)), handler)
	defer closeAll()
	n, err := remote.ReadRangeAtContext(context.Background(), make([]byte, len(data)), 0)
	if err != nil || n != len(data) {
		t.Fatalf("read = %d, %v", n, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("requests = %d, want no empty-suffix retry", calls.Load())
	}
}

func TestRemoteRecoveryWaitHonorsCancellationAndPermanentProbeError(t *testing.T) {
	t.Run("cancel-wait", func(t *testing.T) {
		started := make(chan struct{}, 1)
		var calls atomic.Int32
		handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if calls.Add(1) == 1 {
				writeRange(w, req, []byte("x"), `"v1"`)
				return
			}
			started <- struct{}{}
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(http.StatusServiceUnavailable)
		})
		c, remote, closeAll := retryFixture(t, 1, handler)
		defer closeAll()
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() { _, err := remote.ReadRangeAtContext(ctx, make([]byte, 1), 0); result <- err }()
		<-started
		deadline := time.Now().Add(time.Second)
		for c.RemoteRecoveryStats().Retries == 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if c.RemoteRecoveryStats().Retries == 0 {
			t.Fatal("request did not enter retry wait")
		}
		cancel()
		select {
		case err := <-result:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("read error = %v, want canceled", err)
			}
		case <-time.After(time.Second):
			t.Fatal("canceled retry wait did not return promptly")
		}
		deadline = time.Now().Add(time.Second)
		for c.RemoteRecoveryStats().Cancelled == 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if stats := c.RemoteRecoveryStats(); stats.Cancelled == 0 {
			t.Fatalf("cancellation not counted: %+v", stats)
		}
	})

	t.Run("permanent-probe-error", func(t *testing.T) {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.WriteHeader(http.StatusBadRequest)
		}))
		defer srv.Close()
		cache, err := NewCache(t.TempDir(), 1024)
		if err != nil {
			t.Fatal(err)
		}
		defer cache.Close()
		_, err = NewRemote(context.Background(), cache, "bad", 1, func(context.Context) (string, error) { return srv.URL, nil })
		if err == nil || calls.Load() != 1 {
			t.Fatalf("NewRemote calls=%d err=%v, want one permanent failure", calls.Load(), err)
		}
	})
}
