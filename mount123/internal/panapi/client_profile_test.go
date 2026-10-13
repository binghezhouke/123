package panapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/iostats"
)

func profileClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := New(Config{AccessToken: "token", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func assertProfileCall(t *testing.T, got iostats.APIEndpointSummary, success bool, retries uint64) {
	t.Helper()
	wantSuccess, wantErrors := uint64(0), uint64(1)
	if success {
		wantSuccess, wantErrors = 1, 0
	}
	if got.Status != "measured" || got.Requests != 1 || got.Successes != wantSuccess || got.Errors != wantErrors || got.Retries != retries || got.Latency.Samples == nil || *got.Latency.Samples != 1 {
		t.Fatalf("profile = %+v, want one success=%v call and %d retries", got, success, retries)
	}
}

func TestAPIProfileResponseFailures(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"missing data", `{"code":0}`, 200},
		{"null data", `{"code":0,"data":null}`, 200},
		{"malformed data", `{"code":0,"data":[]}`, 200},
		{"malformed envelope", `{`, 200},
		{"API error without message", `{"code":403}`, 200},
		{"API error with message", `{"code":403,"message":"denied"}`, 200},
		{"HTTP auth error", ``, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := profileClient(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); fmt.Fprint(w, tc.body) })
			var out struct{ Name string }
			if err := c.getJSON(context.Background(), "/api/v2/file/list", nil, &out); err == nil {
				t.Fatal("expected error")
			}
			assertProfileCall(t, c.APIProfile().List, false, 0)
		})
	}
}

func TestAPIProfileRetryIncludesBackoffAndRateWait(t *testing.T) {
	var calls atomic.Uint64
	c := profileClient(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(503)
			return
		}
		fmt.Fprint(w, `{"code":0,"data":{}}`)
	})
	// A 200ms retry backoff is followed by another scheduler wait.
	c.scheduler.intervals[categoryV2List] = 350 * time.Millisecond
	c.scheduler.next[categoryV2List] = time.Now().Add(100 * time.Millisecond)
	started := time.Now()
	var out struct{}
	if err := c.getJSON(context.Background(), "/api/v2/file/list", nil, &out); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	got := c.APIProfile().List
	assertProfileCall(t, got, true, 1)
	if calls.Load() != 2 || got.Latency.TotalNanos == nil || time.Duration(*got.Latency.TotalNanos) < 440*time.Millisecond || time.Duration(*got.Latency.TotalNanos) < elapsed-10*time.Millisecond {
		t.Fatalf("calls=%d profile latency=%v, wall elapsed=%s", calls.Load(), got.Latency, elapsed)
	}
}

func TestAPIProfileCancellation(t *testing.T) {
	for _, phase := range []string{"before request", "rate wait", "HTTP request", "retry backoff"} {
		t.Run(phase, func(t *testing.T) {
			entered := make(chan struct{}, 1)
			c := profileClient(t, func(w http.ResponseWriter, r *http.Request) {
				entered <- struct{}{}
				if phase == "HTTP request" {
					<-r.Context().Done()
					return
				}
				w.WriteHeader(503)
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if phase == "before request" {
				cancel()
			}
			if phase == "rate wait" {
				c.scheduler.next[categoryV2List] = time.Now().Add(time.Hour)
			}
			result := make(chan error, 1)
			go func() { var out struct{}; result <- c.getJSON(ctx, "/api/v2/file/list", nil, &out) }()
			retries := uint64(0)
			switch phase {
			case "HTTP request":
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("request did not start")
				}
				cancel()
			case "retry backoff":
				deadline := time.Now().Add(time.Second)
				for c.APIProfile().List.Retries == 0 && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				if c.APIProfile().List.Retries != 1 {
					t.Fatal("retry not scheduled")
				}
				retries = 1
				cancel()
			case "rate wait":
				// Wait for the scheduler to enqueue the call before cancelling it.
				deadline := time.Now().Add(time.Second)
				for {
					c.scheduler.mu.Lock()
					waiting := len(c.scheduler.queues[categoryV2List]) > 0
					c.scheduler.mu.Unlock()
					if waiting {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("rate wait did not start")
					}
					time.Sleep(time.Millisecond)
				}
				cancel()
			}
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("error=%v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("cancel did not complete")
			}
			assertProfileCall(t, c.APIProfile().List, false, retries)
		})
	}
}

func TestAPIProfileTokenFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) }))
	defer srv.Close()
	c, err := New(Config{ClientID: "id", ClientSecret: "secret", BaseURL: srv.URL, TokenCache: filepath.Join(t.TempDir(), "token")})
	if err != nil {
		t.Fatal(err)
	}
	var out struct{}
	if err := c.getJSON(context.Background(), "/api/v2/file/list", nil, &out); err == nil {
		t.Fatal("expected token error")
	}
	assertProfileCall(t, c.APIProfile().List, false, 0)
	assertProfileCall(t, c.APIProfile().AccessToken, false, 0)
}

func TestAPIProfileBodyReadRetry(t *testing.T) {
	var calls atomic.Uint64
	c := profileClient(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Length", "100")
			fmt.Fprint(w, `{}`)
			return
		}
		fmt.Fprint(w, `{"code":0,"data":{}}`)
	})
	c.scheduler.intervals[categoryV2List] = 0
	var out struct{}
	if err := c.getJSON(context.Background(), "/api/v2/file/list", nil, &out); err != nil {
		t.Fatal(err)
	}
	assertProfileCall(t, c.APIProfile().List, true, 1)
}

func TestAPIProfileAuthenticationRetry(t *testing.T) {
	for _, apiCode := range []bool{false, true} {
		t.Run(fmt.Sprint("envelope=", apiCode), func(t *testing.T) {
			var calls atomic.Uint64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/access_token" {
					fmt.Fprint(w, `{"code":0,"data":{"accessToken":"fresh","expiresIn":3600}}`)
					return
				}
				if calls.Add(1) == 1 {
					if apiCode {
						fmt.Fprint(w, `{"code":401}`)
					} else {
						w.WriteHeader(401)
					}
					return
				}
				fmt.Fprint(w, `{"code":0,"data":{}}`)
			}))
			defer srv.Close()
			c, err := New(Config{ClientID: "id", ClientSecret: "secret", BaseURL: srv.URL, TokenCache: filepath.Join(t.TempDir(), "token")})
			if err != nil {
				t.Fatal(err)
			}
			c.token = "stale"
			c.expires = time.Now().Add(time.Hour)
			c.scheduler.intervals[categoryV2List] = 0
			var out struct{}
			if err := c.getJSON(context.Background(), "/api/v2/file/list", nil, &out); err != nil {
				t.Fatal(err)
			}
			assertProfileCall(t, c.APIProfile().List, true, 1)
			assertProfileCall(t, c.APIProfile().AccessToken, true, 0)
		})
	}
}

func TestAPIProfileBodyReadFailure(t *testing.T) {
	c := profileClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		fmt.Fprint(w, `{}`)
	})
	var out struct{}
	if err := c.requestJSON(context.Background(), http.MethodPost, "/upload/v2/file/create", nil, nil, &out); err == nil {
		t.Fatal("expected read failure")
	}
	assertProfileCall(t, c.APIProfile().Other, false, 0)
}

func TestAPIProfileTokenReuseDuringAcquisition(t *testing.T) {
	c := profileClient(t, func(w http.ResponseWriter, r *http.Request) { t.Error("cached token should prevent HTTP acquisition") })
	// Another call can install a token while fetchToken waits for its quota or lock.
	token, err := c.fetchToken(context.Background())
	if err != nil || token != "token" {
		t.Fatalf("token=%q error=%v", token, err)
	}
	assertProfileCall(t, c.APIProfile().AccessToken, true, 0)
}
