package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestImmutableRemoteRestartHitsBeforeURLAndMissesFetchExactRange(t *testing.T) {
	for _, version := range []string{"v1", "", ":32"} {
		t.Run("version="+version, func(t *testing.T) {
			data := []byte("0123456789abcdefghijklmnopqrstuv")
			var requests, resolves atomic.Int32
			server := httptest.NewServer(identityRangeHandler(data, "", &requests, 0))
			defer server.Close()
			dir := t.TempDir()
			cache, err := NewCache(dir, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			identity := RemoteIdentity{Account: "synthetic-immutable-account", FileID: 91, Version: version, Immutable: true, TTL: time.Nanosecond}
			resolve := func(context.Context) (string, error) { resolves.Add(1); return server.URL, nil }
			first := newIdentityRemote(t, cache, identity, int64(len(data)), resolve)
			if requests.Load() != 0 || resolves.Load() != 0 {
				t.Fatal("immutable constructor eagerly resolved/probed")
			}
			got := make([]byte, 8)
			if n, err := first.ReadRangeAtContext(context.Background(), got, 0); err != nil || n != 8 || !bytes.Equal(got, data[:8]) {
				t.Fatalf("seed: %d %v", n, err)
			}
			waitIdentityRangeCached(t, cache, first.rangeID, 0, 8)
			key := first.Key()
			if err := cache.Close(); err != nil {
				t.Fatal(err)
			}
			cache, err = NewCache(dir, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer cache.Close()
			if err := cache.Remove(linkCacheKey("identity-fixture")); err != nil {
				t.Fatal(err)
			}
			resolves.Store(0)
			requests.Store(0)
			var deny atomic.Bool
			deny.Store(true)
			remote := newIdentityRemote(t, cache, identity, int64(len(data)), func(ctx context.Context) (string, error) {
				resolves.Add(1)
				if deny.Load() {
					return "", errors.New("synthetic denied URL")
				}
				return server.URL, nil
			})
			if remote.Key() != key || !remote.IdentityExpiresAt().IsZero() {
				t.Fatal("immutable namespace/expiry changed on restart")
			}
			if _, err := remote.ReadRangeAtContext(context.Background(), got, 0); err != nil || !bytes.Equal(got, data[:8]) {
				t.Fatalf("warm hit: %v", err)
			}
			if resolves.Load() != 0 || requests.Load() != 0 {
				t.Fatal("warm hit did network I/O")
			}
			// A canceled request must not poison the retained source.
			canceled, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := remote.ReadRangeAtContext(canceled, got, 8); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel: %v", err)
			}
			deny.Store(false)
			if n, err := remote.ReadRangeAtContext(context.Background(), got, 8); err != nil || n != 8 || !bytes.Equal(got, data[8:16]) {
				t.Fatalf("gap: %d %v", n, err)
			}
			if resolves.Load() != 1 || requests.Load() != 1 {
				t.Fatalf("gap resolves=%d HTTP=%d; want only demanded range", resolves.Load(), requests.Load())
			}
		})
	}
}

func TestImmutableRemoteKeepsExpiredLegacyNamespaceWhenCDNValidatorChanges(t *testing.T) {
	for _, validator := range []string{"etag", "last-modified"} {
		t.Run(validator, func(t *testing.T) {
			data := []byte("immutable archive content")
			var requests, resolves, conditional atomic.Int32
			var changed atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if changed.Load() && (r.Header.Get("If-Match") != "" || r.Header.Get("If-Unmodified-Since") != "") {
					conditional.Add(1)
					w.WriteHeader(http.StatusPreconditionFailed)
					return
				}
				if validator == "last-modified" {
					date := "Mon, 02 Jan 2006 15:04:05 GMT"
					if changed.Load() {
						date = "Tue, 03 Jan 2006 15:04:05 GMT"
					}
					w.Header().Set("Last-Modified", date)
				}
				etag := ""
				if validator == "etag" {
					etag = `"cdn-old"`
					if changed.Load() {
						etag = `"cdn-new"`
					}
				}
				identityRangeHandler(data, etag, &requests, 0).ServeHTTP(w, r)
			}))
			defer server.Close()
			dir := t.TempDir()
			cache, err := NewCache(dir, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			identity := identityOpts(0)
			resolve := func(context.Context) (string, error) { resolves.Add(1); return server.URL, nil }
			legacy := newIdentityRemote(t, cache, identity, int64(len(data)), resolve)
			got := make([]byte, 8)
			if _, err := legacy.ReadRangeAtContext(context.Background(), got, 0); err != nil {
				t.Fatal(err)
			}
			waitIdentityRangeCached(t, cache, legacy.rangeID, 0, 8)
			// Expire the descriptor deterministically without sleeping or touching
			// the existing range data, then upgrade through the archive policy.
			descriptorKey, digest := remoteIdentityKeys(cache, identity, int64(len(data)))
			d, valid, err := restoreRemoteIdentity(context.Background(), cache, descriptorKey, digest, identity, int64(len(data)))
			if err != nil || !valid {
				t.Fatalf("legacy descriptor: %v %v", valid, err)
			}
			d.Obtained = time.Unix(1, 0).UTC()
			encoded, err := json.Marshal(d)
			if err != nil {
				t.Fatal(err)
			}
			if err := cache.ReplaceArchiveIndex(context.Background(), descriptorKey, encoded); err != nil {
				t.Fatal(err)
			}
			key := legacy.Key()
			if err := cache.Close(); err != nil {
				t.Fatal(err)
			}
			cache, err = NewCache(dir, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer cache.Close()
			if err := cache.Remove(linkCacheKey("identity-fixture")); err != nil {
				t.Fatal(err)
			}
			changed.Store(true)
			requests.Store(0)
			resolves.Store(0)
			identity.Immutable = true
			remote := newIdentityRemote(t, cache, identity, int64(len(data)), resolve)
			if remote.Key() != key {
				t.Fatal("discarded legacy cache namespace")
			}
			if _, err := remote.ReadRangeAtContext(context.Background(), got, 0); err != nil || !bytes.Equal(got, data[:8]) {
				t.Fatalf("warm legacy hit: %v", err)
			}
			if requests.Load()+resolves.Load() != 0 {
				t.Fatal("legacy hit revalidated contents")
			}
			if _, err := remote.ReadRangeAtContext(context.Background(), got, 8); err != nil || !bytes.Equal(got, data[8:16]) {
				t.Fatalf("changed CDN validator gap: %v", err)
			}
			if conditional.Load() != 0 || requests.Load() != 1 || resolves.Load() != 1 {
				t.Fatalf("conditional=%d HTTP=%d resolves=%d", conditional.Load(), requests.Load(), resolves.Load())
			}
			// The ordinary-file policy still expires/reprobes the separate legacy
			// descriptor and rejects a later changed validator on uncached reads.
			identity.Immutable = false
			ordinary := newIdentityRemote(t, cache, identity, int64(len(data)), resolve)
			if ordinary.Key() == key {
				t.Fatal("ordinary policy bypassed expired validator")
			}
		})
	}
}

func TestImmutableRemoteGapRetainsProtocolValidation(t *testing.T) {
	for _, mode := range []string{"range", "size", "encoding", "short-body"} {
		t.Run(mode, func(t *testing.T) {
			data := []byte("synthetic immutable bytes")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				start, end, size := 0, 7, len(data)
				if mode == "range" {
					start++
				}
				if mode == "size" {
					size++
				}
				if mode == "encoding" {
					w.Header().Set("Content-Encoding", "gzip")
				}
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
				w.Header().Set("Content-Length", "8")
				w.WriteHeader(http.StatusPartialContent)
				if mode == "short-body" {
					_, _ = w.Write(data[:3])
					return
				}
				_, _ = w.Write(data[:8])
			}))
			defer server.Close()
			cache, err := NewCache(t.TempDir(), 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer cache.Close()
			identity := identityOpts(0)
			identity.Immutable = true
			remote := newIdentityRemote(t, cache, identity, int64(len(data)), func(context.Context) (string, error) { return server.URL, nil })
			if _, err := remote.ReadRangeAtContext(context.Background(), make([]byte, 8), 0); err == nil {
				t.Fatal("accepted malformed immutable range")
			}
			if len(cache.missingRanges(remote.rangeID, 0, 8)) == 0 {
				t.Fatal("published malformed range")
			}
		})
	}
}

func TestImmutableRemoteURLRefreshAndCanceledReadReuseSameSource(t *testing.T) {
	for _, scenario := range []string{"rejected-url", "canceled-read"} {
		t.Run(scenario, func(t *testing.T) {
			data := []byte("synthetic immutable recovery")
			var requests, resolves atomic.Int32
			started, stopped := make(chan struct{}), make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if requests.Add(1) == 1 {
					if scenario == "rejected-url" {
						w.WriteHeader(http.StatusForbidden)
						return
					}
					close(started)
					<-req.Context().Done()
					close(stopped)
					return
				}
				identityRangeHandler(data, `"changed-cdn-validator"`, &atomic.Int32{}, 0).ServeHTTP(w, req)
			}))
			defer server.Close()
			cache, err := NewCache(t.TempDir(), 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer cache.Close()
			identity := identityOpts(0)
			identity.Immutable = true
			// The constructor's operation context is temporary; only the source's
			// explicit lifetime context may govern future requests.
			openCtx, cancelOpen := context.WithCancel(context.Background())
			remote, err := NewCachedRemoteContext(context.Background(), openCtx, cache, "immutable-recovery", int64(len(data)), func(context.Context) (string, error) {
				resolves.Add(1)
				return server.URL, nil
			}, identity)
			cancelOpen()
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "canceled-read" {
				readCtx, cancelRead := context.WithCancel(context.Background())
				done := make(chan error, 1)
				go func() { _, err := remote.ReadRangeAtContext(readCtx, make([]byte, len(data)), 0); done <- err }()
				select {
				case <-started:
				case <-time.After(time.Second):
					cancelRead()
					t.Fatal("range did not start")
				}
				cancelRead()
				select {
				case err := <-done:
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("cancel error: %v", err)
					}
				case <-time.After(time.Second):
					t.Fatal("read cancellation blocked")
				}
				select {
				case <-stopped:
				case <-time.After(time.Second):
					t.Fatal("request lifetime survived canceled read")
				}
			}
			got := make([]byte, len(data))
			if n, err := remote.ReadRangeAtContext(context.Background(), got, 0); err != nil || n != len(data) || !bytes.Equal(got, data) {
				t.Fatalf("same-source recovery: %d %v", n, err)
			}
			wantResolves := int32(1)
			if scenario == "rejected-url" {
				wantResolves = 2
			}
			if requests.Load() != 2 || resolves.Load() != wantResolves {
				t.Fatalf("HTTP=%d resolves=%d, want two requests and %d resolves", requests.Load(), resolves.Load(), wantResolves)
			}
		})
	}
}

func TestImmutableRemoteInterruptedBodyResumesWhileOrdinaryUnvalidatedSourceRefuses(t *testing.T) {
	for _, immutable := range []bool{true, false} {
		t.Run(fmt.Sprintf("immutable=%t", immutable), func(t *testing.T) {
			data := []byte("synthetic immutable suffix recovery")
			var demands atomic.Int32
			var suffixStart atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				var start, end int64
				if _, err := fmt.Sscanf(req.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
					http.Error(w, "bad range", 400)
					return
				}
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
				w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
				w.WriteHeader(http.StatusPartialContent)
				if end == 0 {
					_, _ = w.Write(data[:1])
					return
				} // ordinary constructor probe
				if demands.Add(1) == 1 {
					_, _ = w.Write(data[start : start+3])
					return
				}
				suffixStart.Store(start)
				_, _ = w.Write(data[start : end+1])
			}))
			defer server.Close()
			cache, err := NewCache(t.TempDir(), 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer cache.Close()
			identity := identityOpts(0)
			identity.Immutable = immutable
			remote := newIdentityRemote(t, cache, identity, int64(len(data)), func(context.Context) (string, error) { return server.URL, nil })
			got := make([]byte, 16)
			n, err := remote.ReadRangeAtContext(context.Background(), got, 0)
			if immutable {
				if err != nil || n != len(got) || !bytes.Equal(got, data[:len(got)]) {
					t.Fatalf("immutable suffix recovery: n=%d err=%v", n, err)
				}
				if demands.Load() != 2 || suffixStart.Load() != 3 {
					t.Fatalf("demands=%d suffixStart=%d, want one retry from offset 3", demands.Load(), suffixStart.Load())
				}
			} else if err == nil || demands.Load() != 1 {
				t.Fatalf("ordinary unvalidated source resumed: n=%d err=%v requests=%d", n, err, demands.Load())
			}
		})
	}
}

func TestImmutableRemoteRepeatedTruncationExhaustsBoundedRetries(t *testing.T) {
	data := []byte("synthetic immutable repeated truncation")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests.Add(1)
		var start, end int64
		if _, err := fmt.Sscanf(req.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			http.Error(w, "bad range", 400)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[start : start+1])
	}))
	defer server.Close()
	cache, err := NewCache(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	identity := identityOpts(0)
	identity.Immutable = true
	remote := newIdentityRemote(t, cache, identity, int64(len(data)), func(context.Context) (string, error) { return server.URL, nil })
	if _, err := remote.ReadRangeAtContext(context.Background(), make([]byte, 16), 0); err == nil {
		t.Fatal("accepted repeatedly truncated range")
	}
	if requests.Load() != 1+remoteRecoveryRetries {
		t.Fatalf("requests=%d, want bounded %d", requests.Load(), 1+remoteRecoveryRetries)
	}
	if len(cache.missingRanges(remote.rangeID, 0, 16)) == 0 {
		t.Fatal("published truncated range")
	}
}
