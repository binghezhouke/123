package storage

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/binghezhouke/123/mount123/internal/faults"
)

const (
	// Each probe or range flight can spend at most three recovery retries
	// total, including the existing signed-URL refresh retry. The surrounding
	// context also imposes a single 20-second wall-clock budget.
	remoteRecoveryRetries = 3
	remoteRecoveryBudget  = 20 * time.Second
	remoteRetryAfterLimit = 5 * time.Second
)

type remoteRecoveryBudgetContextKey struct{}

type remoteRequestRecoveryContextKey struct{}

type remoteRequestRecoveryState struct {
	authRefreshed bool
}

type remoteRecoveryBudgetState struct {
	mu      sync.Mutex
	retries int
}

func (b *remoteRecoveryBudgetState) takeRetry() (int, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.retries >= remoteRecoveryRetries {
		return b.retries, false
	}
	b.retries++
	return b.retries, true
}

func recoveryBudgetFrom(ctx context.Context) *remoteRecoveryBudgetState {
	if budget, ok := ctx.Value(remoteRecoveryBudgetContextKey{}).(*remoteRecoveryBudgetState); ok && budget != nil {
		return budget
	}
	return &remoteRecoveryBudgetState{}
}

type remoteRecoveryCounters struct {
	attempts    atomic.Uint64
	retries     atomic.Uint64
	recovered   atomic.Uint64
	exhausted   atomic.Uint64
	cancelled   atomic.Uint64
	suffixBytes atomic.Uint64
}

// RemoteRecoveryStats contains aggregate recovery counters for a Cache.
// It contains no URLs, file names, or other request details.
type RemoteRecoveryStats struct {
	Attempts    uint64
	Retries     uint64
	Recovered   uint64
	Exhausted   uint64
	Cancelled   uint64
	SuffixBytes uint64
}

// RecoveryStats returns cache-wide retry activity for remote readers.
func (r *Remote) RecoveryStats() RemoteRecoveryStats {
	if r == nil {
		return RemoteRecoveryStats{}
	}
	return r.cache.RemoteRecoveryStats()
}

// RemoteRecoveryStats returns aggregate retry activity across all remote
// readers owned by this cache.
func (c *Cache) RemoteRecoveryStats() RemoteRecoveryStats {
	if c == nil {
		return RemoteRecoveryStats{}
	}
	return RemoteRecoveryStats{
		Attempts:    c.remoteRecovery.attempts.Load(),
		Retries:     c.remoteRecovery.retries.Load(),
		Recovered:   c.remoteRecovery.recovered.Load(),
		Exhausted:   c.remoteRecovery.exhausted.Load(),
		Cancelled:   c.remoteRecovery.cancelled.Load(),
		SuffixBytes: c.remoteRecovery.suffixBytes.Load(),
	}
}

func (r *Remote) fetchOneRangeWithRecovery(ctx context.Context, start, end int64, w io.Writer, priority *downloadPriority) (retErr error) {
	budgetCtx, cancel := context.WithTimeout(ctx, remoteRecoveryBudget)
	defer cancel()
	retryBudget := recoveryBudgetFrom(budgetCtx)
	budgetCtx = context.WithValue(budgetCtx, remoteRecoveryBudgetContextKey{}, retryBudget)
	requestRecovery := &remoteRequestRecoveryState{}
	budgetCtx = context.WithValue(budgetCtx, remoteRequestRecoveryContextKey{}, requestRecovery)
	defer func() {
		if errors.Is(retErr, context.Canceled) {
			r.cache.remoteRecovery.cancelled.Add(1)
		}
	}()

	cursor := start
	for attempt := 0; ; attempt++ {
		if err := budgetCtx.Err(); err != nil {
			return err
		}
		resp, err := r.requestRangeScheduled(budgetCtx, cursor, end-1, true, r.rangeID, priority)
		if err != nil {
			if !faults.IsTransient(err) {
				return err
			}
			retryNo, ok := retryBudget.takeRetry()
			if !ok {
				if faults.IsTransient(err) {
					r.cache.remoteRecovery.exhausted.Add(1)
				}
				return err
			}
			r.cache.remoteRecovery.retries.Add(1)
			if err := waitRemoteRetry(budgetCtx, retryNo, ""); err != nil {
				return err
			}
			continue
		}

		if resp.StatusCode == http.StatusPreconditionFailed {
			_ = resp.Body.Close()
			return &faults.Error{Kind: faults.RemoteChanged, Message: "remote file changed"}
		}
		if statusErr := remoteStatusError(resp); statusErr != nil {
			retryAfter := resp.Header.Get("Retry-After")
			_ = resp.Body.Close()
			if !faults.IsTransient(statusErr) {
				return statusErr
			}
			retryNo, ok := retryBudget.takeRetry()
			if !ok {
				r.cache.remoteRecovery.exhausted.Add(1)
				return statusErr
			}
			r.cache.remoteRecovery.retries.Add(1)
			if err := waitRemoteRetry(budgetCtx, retryNo, retryAfter); err != nil {
				if budgetCtx.Err() != nil {
					return budgetCtx.Err()
				}
				return statusErr
			}
			continue
		}
		if err := checkRangeResponse(resp, cursor, end-1, r.size); err != nil {
			_ = resp.Body.Close()
			return &faults.Error{Kind: faults.InvalidResponse, Message: "remote server returned an invalid byte range"}
		}
		if err := r.checkEntityValidator(resp); err != nil {
			_ = resp.Body.Close()
			return err
		}

		expected := end - cursor
		tracked := &remoteRetryWriter{writer: w}
		written, readErr := io.CopyN(tracked, resp.Body, expected)
		if tracked.err != nil {
			_ = resp.Body.Close()
			return tracked.err
		}
		if cursor > start && written > 0 {
			r.cache.remoteRecovery.suffixBytes.Add(uint64(written))
		}
		cursor += written
		if readErr == nil {
			var extra [1]byte
			nExtra, extraErr := io.ReadFull(resp.Body, extra[:])
			_ = resp.Body.Close()
			if nExtra != 0 {
				return &faults.Error{Kind: faults.InvalidResponse, Message: "remote range body length mismatch"}
			}
			if extraErr == io.EOF {
				if attempt > 0 || requestRecovery.authRefreshed {
					r.cache.remoteRecovery.recovered.Add(1)
				}
				return nil
			}
			if budgetCtx.Err() != nil {
				return budgetCtx.Err()
			}
			if cursor == end {
				r.cache.remoteRecovery.exhausted.Add(1)
				return &faults.Error{Kind: faults.Network, Message: "remote range completion was interrupted", Retryable: true}
			}
			readErr = extraErr
		} else {
			_ = resp.Body.Close()
		}

		if budgetCtx.Err() != nil {
			return budgetCtx.Err()
		}
		if !isInterruptedBody(readErr) {
			return &faults.Error{Kind: faults.InvalidResponse, Message: "could not read remote range"}
		}
		if cursor > start && r.strongResumeValidator() == "" {
			return &faults.Error{Kind: faults.InvalidResponse, Message: "remote range was interrupted without a safe validator"}
		}
		retryNo, ok := retryBudget.takeRetry()
		if !ok {
			r.cache.remoteRecovery.exhausted.Add(1)
			return &faults.Error{Kind: faults.Network, Message: "remote range response was interrupted", Retryable: true}
		}
		r.cache.remoteRecovery.retries.Add(1)
		if err := waitRemoteRetry(budgetCtx, retryNo, ""); err != nil {
			return err
		}
	}
}

type remoteRetryWriter struct {
	writer io.Writer
	err    error
}

func (w *remoteRetryWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	if err != nil {
		w.err = err
	} else if n != len(p) {
		w.err = io.ErrShortWrite
	}
	return n, err
}

func (r *Remote) probeRange(ctx context.Context, key string, priority *downloadPriority) (etag, modified string, retErr error) {
	budgetCtx, cancel := context.WithTimeout(ctx, remoteRecoveryBudget)
	defer cancel()
	retryBudget := recoveryBudgetFrom(budgetCtx)
	budgetCtx = context.WithValue(budgetCtx, remoteRecoveryBudgetContextKey{}, retryBudget)
	requestRecovery := &remoteRequestRecoveryState{}
	budgetCtx = context.WithValue(budgetCtx, remoteRequestRecoveryContextKey{}, requestRecovery)
	defer func() {
		if errors.Is(retErr, context.Canceled) {
			r.cache.remoteRecovery.cancelled.Add(1)
		}
	}()
	for attempt := 0; ; attempt++ {
		if err := budgetCtx.Err(); err != nil {
			return "", "", err
		}
		resp, err := r.requestRangeScheduled(budgetCtx, 0, 0, false, cacheID(key), priority)
		if err != nil {
			if !faults.IsTransient(err) {
				return "", "", err
			}
			retryNo, ok := retryBudget.takeRetry()
			if !ok {
				r.cache.remoteRecovery.exhausted.Add(1)
				return "", "", err
			}
			r.cache.remoteRecovery.retries.Add(1)
			if err := waitRemoteRetry(budgetCtx, retryNo, ""); err != nil {
				return "", "", err
			}
			continue
		}
		if resp.StatusCode == http.StatusPreconditionFailed {
			_ = resp.Body.Close()
			return "", "", &faults.Error{Kind: faults.RemoteChanged, Message: "remote file changed"}
		}
		if statusErr := remoteStatusError(resp); statusErr != nil {
			retryAfter := resp.Header.Get("Retry-After")
			_ = resp.Body.Close()
			if !faults.IsTransient(statusErr) {
				return "", "", statusErr
			}
			retryNo, ok := retryBudget.takeRetry()
			if !ok {
				r.cache.remoteRecovery.exhausted.Add(1)
				return "", "", statusErr
			}
			r.cache.remoteRecovery.retries.Add(1)
			if err := waitRemoteRetry(budgetCtx, retryNo, retryAfter); err != nil {
				if budgetCtx.Err() != nil {
					return "", "", budgetCtx.Err()
				}
				return "", "", statusErr
			}
			continue
		}
		if err := checkRangeResponse(resp, 0, 0, r.size); err != nil {
			_ = resp.Body.Close()
			return "", "", &faults.Error{Kind: faults.InvalidResponse, Message: "remote server returned an invalid byte range"}
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 2))
		etag, modified = resp.Header.Get("ETag"), resp.Header.Get("Last-Modified")
		_ = resp.Body.Close()
		if readErr == nil && len(body) == 1 {
			if attempt > 0 || requestRecovery.authRefreshed {
				r.cache.remoteRecovery.recovered.Add(1)
			}
			return etag, modified, nil
		}
		if budgetCtx.Err() != nil {
			return "", "", budgetCtx.Err()
		}
		if readErr == nil {
			return "", "", &faults.Error{Kind: faults.InvalidResponse, Message: "remote range probe returned an invalid body"}
		}
		if !isInterruptedBody(readErr) {
			return "", "", &faults.Error{Kind: faults.InvalidResponse, Message: "remote range probe returned an invalid body"}
		}
		retryNo, ok := retryBudget.takeRetry()
		if !ok {
			r.cache.remoteRecovery.exhausted.Add(1)
			return "", "", &faults.Error{Kind: faults.Network, Message: "remote range probe was interrupted", Retryable: true}
		}
		r.cache.remoteRecovery.retries.Add(1)
		if err := waitRemoteRetry(budgetCtx, retryNo, ""); err != nil {
			return "", "", err
		}
	}
}

func (r *Remote) checkEntityValidator(resp *http.Response) error {
	if r.etag != "" && resp.Header.Get("ETag") != r.etag {
		return &faults.Error{Kind: faults.RemoteChanged, Message: "remote file changed"}
	}
	if r.etag == "" && r.modified != "" && resp.Header.Get("Last-Modified") != r.modified {
		return &faults.Error{Kind: faults.RemoteChanged, Message: "remote file changed"}
	}
	return nil
}

func (r *Remote) strongResumeValidator() string {
	if r.etag != "" {
		return r.etag
	}
	return r.modified
}

func remoteStatusError(resp *http.Response) error {
	status := resp.StatusCode
	switch status {
	case http.StatusPartialContent:
		return nil
	case http.StatusTooManyRequests:
		return &faults.Error{Kind: faults.Throttled, Message: "remote server throttled range request", Retryable: true}
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return &faults.Error{Kind: faults.Unavailable, Message: fmt.Sprintf("remote server returned HTTP %d", status), Retryable: true}
	case http.StatusUnauthorized, http.StatusForbidden:
		return &faults.Error{Kind: faults.Unauthorized, Message: "remote server rejected range request"}
	case http.StatusNotFound, http.StatusGone:
		return &faults.Error{Kind: faults.NotFound, Message: "remote file is unavailable"}
	default:
		return &faults.Error{Kind: faults.InvalidResponse, Message: fmt.Sprintf("remote server returned HTTP %d", status)}
	}
}

func isInterruptedBody(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || faults.IsTransient(err)
}

func retryableTransportError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalidCert x509.CertificateInvalidError
	if errors.As(err, &unknownAuthority) || errors.As(err, &hostname) || errors.As(err, &invalidCert) {
		return false
	}
	var uerr *url.Error
	if errors.As(err, &uerr) {
		err = uerr.Err
	}
	var network net.Error
	return errors.As(err, &network)
}

func waitRemoteRetry(ctx context.Context, attempt int, retryAfter string) error {
	delay, err := retryAfterDelay(retryAfter)
	if err != nil {
		return err
	}
	if delay < 0 {
		delay = 0
	}
	if delay == 0 {
		delay = time.Duration(1<<uint(attempt-1)) * 100 * time.Millisecond
		if delay > time.Second {
			delay = time.Second
		}
	}
	if delay > remoteRetryAfterLimit {
		return &faults.Error{Kind: faults.Throttled, Message: "remote retry delay exceeds recovery limit", Retryable: true}
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func retryAfterDelay(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds > int64(remoteRetryAfterLimit/time.Second) {
			return remoteRetryAfterLimit + time.Nanosecond, nil
		}
		return time.Duration(seconds) * time.Second, nil
	}
	if when, err := http.ParseTime(value); err == nil {
		return time.Until(when), nil
	}
	return 0, &faults.Error{Kind: faults.InvalidResponse, Message: "remote server returned an invalid Retry-After value"}
}
