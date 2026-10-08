package panapi

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/binghezhouke/123/mount123/internal/faults"
)

func TestListRetriesTransientHTTPStatuses(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 1 {
					if status == http.StatusTooManyRequests {
						w.Header().Set("Retry-After", "0")
					}
					w.WriteHeader(status)
					return
				}
				_, _ = w.Write([]byte(`{"code":0,"data":{"fileList":[],"lastFileId":-1}}`))
			}))
			defer srv.Close()
			c, err := New(Config{AccessToken: "secret-token", BaseURL: srv.URL})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.List(context.Background(), 0); err != nil {
				t.Fatalf("List() error = %v", err)
			}
			if calls.Load() != 2 {
				t.Fatalf("calls = %d, want one retry", calls.Load())
			}
		})
	}
}

func TestListRecoversAfterTransportDrop(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if calls.Add(1) == 1 {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Error("HTTP server does not support hijacking")
				return
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"fileList":[],"lastFileId":-1}}`))
	}))
	defer srv.Close()
	c, _ := New(Config{AccessToken: "token", BaseURL: srv.URL})
	if _, err := c.List(context.Background(), 0); err != nil {
		t.Fatalf("List() after connection drop: %v", err)
	}
	if calls.Load() < 2 {
		t.Fatalf("calls = %d, want retry after dropped connection", calls.Load())
	}
}

func TestListReturnsTypedTransientAfterRetryBudget(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c, _ := New(Config{AccessToken: "token", BaseURL: srv.URL})
	_, err := c.List(context.Background(), 0)
	if !faults.IsTransient(err) || faults.KindOf(err) != faults.Unavailable {
		t.Fatalf("List error = %v, kind=%q transient=%t", err, faults.KindOf(err), faults.IsTransient(err))
	}
	if calls.Load() != 5 {
		t.Fatalf("calls = %d, want initial plus four retries", calls.Load())
	}
}

func TestAPIStatusCodesHaveStableKinds(t *testing.T) {
	for _, code := range []int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		if got := apiCodeKind(code); got != faults.Unavailable {
			t.Errorf("apiCodeKind(%d) = %q, want unavailable", code, got)
		}
	}
}

func TestListDoesNotRetryPermanentHTTPError(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	c, _ := New(Config{AccessToken: "token", BaseURL: srv.URL})
	_, err := c.List(context.Background(), 0)
	if err == nil || faults.IsTransient(err) || faults.KindOf(err) != faults.InvalidResponse {
		t.Fatalf("List error = %v, kind=%q transient=%t", err, faults.KindOf(err), faults.IsTransient(err))
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want one", calls.Load())
	}
}

func TestSaveZIPPasswordDoesNotRetryTransientMutation(t *testing.T) {
	var createCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/api/v2/file/list":
			_, _ = w.Write([]byte(`{"code":0,"data":{"fileList":[],"lastFileId":-1}}`))
		case "/upload/v2/file/create":
			createCalls.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			http.NotFound(w, req)
		}
	}))
	defer srv.Close()
	c, _ := New(Config{AccessToken: "token", BaseURL: srv.URL})
	_, err := c.SaveZIPPassword(context.Background(), File{ID: 1, ParentID: 0, Name: "archive.zip"}, []byte("pw"))
	if err == nil {
		t.Fatal("SaveZIPPassword unexpectedly succeeded")
	}
	if createCalls.Load() != 1 {
		t.Fatalf("upload create calls = %d, want one", createCalls.Load())
	}
}

func TestSaveZIPPasswordPreservesExplicitThrottleRetry(t *testing.T) {
	var createCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/api/v2/file/list":
			_, _ = w.Write([]byte(`{"code":0,"data":{"fileList":[],"lastFileId":-1}}`))
		case "/upload/v2/file/create":
			if createCalls.Add(1) == 1 {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"reuse":true,"fileID":77}}`))
		default:
			http.NotFound(w, req)
		}
	}))
	defer srv.Close()
	c, _ := New(Config{AccessToken: "token", BaseURL: srv.URL})
	file, err := c.SaveZIPPassword(context.Background(), File{ID: 1, ParentID: 0, Name: "archive.zip"}, []byte("pw"))
	if err != nil || file.ID != 77 {
		t.Fatalf("SaveZIPPassword() = %#v, %v", file, err)
	}
	if createCalls.Load() != 2 {
		t.Fatalf("upload create calls = %d, want one explicit throttling retry", createCalls.Load())
	}
}

func TestContextDeadlineAndCancellationClassification(t *testing.T) {
	if !faults.IsTransient(context.DeadlineExceeded) {
		t.Fatal("deadline should be transient")
	}
	if faults.IsTransient(context.Canceled) {
		t.Fatal("cancellation should not be transient")
	}
	var netErr net.Error = timeoutError{}
	if !faults.IsTransient(netErr) {
		t.Fatal("net.Error should be transient")
	}
	if faults.IsTransient(errors.New("generic failure")) {
		t.Fatal("unknown errors should not be transient")
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }
