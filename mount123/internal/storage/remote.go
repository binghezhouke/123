package storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/binghezhouke/123/mount123/internal/faults"
	"github.com/binghezhouke/123/mount123/internal/iostats"
	"github.com/binghezhouke/123/mount123/internal/workqueue"
)

const remoteBlockSize int64 = 1 << 20
const remoteCachePageSize int64 = 64 << 10
const remoteRequestTimeout = 45 * time.Second

// ResolveURL obtains a temporary download URL. Authorization belongs in this
// resolver; the storage package only sends byte-range requests to its result.
type ResolveURL func(context.Context) (string, error)

type Remote struct {
	lifetimeCtx           context.Context
	cache                 *Cache
	size                  int64
	key                   string
	linkKey               string
	resolve               ResolveURL
	client                *http.Client
	mu                    sync.Mutex
	rangeID               string
	url                   string
	resolvedURL           string
	urlUntil              time.Time
	etag, modified        string
	identityExpiresAt     time.Time
	identityDescriptorKey string
	identityInvalidated   atomic.Bool
	onIdentityInvalidated func()
}

// NewRemote probes byte zero to establish the remote entity validator and
// confirms that the endpoint supports exact HTTP ranges.
func NewRemote(ctx context.Context, cache *Cache, key string, size int64, resolve ResolveURL) (*Remote, error) {
	return NewRemoteContext(ctx, ctx, cache, key, size, resolve)
}

// NewRemoteContext probes the remote using both the mount lifetime and the
// operation context. Only lifetimeCtx is retained for subsequent ReadAt calls.
func NewRemoteContext(lifetimeCtx, operationCtx context.Context, cache *Cache, key string, size int64, resolve ResolveURL) (*Remote, error) {
	if cache == nil || resolve == nil || size < 0 {
		return nil, errors.New("invalid remote reader configuration")
	}
	if lifetimeCtx == nil {
		lifetimeCtx = context.Background()
	}
	ctx, cancel := combineContexts(lifetimeCtx, operationCtx)
	defer cancel()
	ctx, stopCache := combineContexts(cache.lifetimeCtx, ctx)
	defer stopCache()
	r := &Remote{lifetimeCtx: lifetimeCtx, cache: cache, size: size, key: key, linkKey: key, resolve: resolve, client: http.DefaultClient}
	if size == 0 {
		r.key = namespaceWithoutValidator(key)
		r.rangeID = cacheID(r.key)
		return r, nil
	}
	if err := r.refresh(ctx); err != nil {
		return nil, err
	}
	probePriority := &downloadPriority{}
	if !workqueue.IsBackground(operationCtx) {
		probePriority.promoted.Store(true)
	}
	probeETag, probeModified, err := r.probeRange(ctx, key, probePriority)
	if err != nil {
		return nil, err
	}
	r.etag, r.modified = probeETag, probeModified
	if strings.HasPrefix(strings.TrimSpace(r.etag), "W/") {
		r.etag = ""
	}
	if r.etag == "" && r.modified == "" {
		r.key = namespaceWithoutValidator(key)
	} else if r.etag != "" {
		r.key = key + "\x00etag:" + r.etag
	} else {
		r.key = key + "\x00modified:" + r.modified
	}
	r.rangeID = cacheID(r.key)
	return r, nil
}

func combineContexts(lifetimeCtx, operationCtx context.Context) (context.Context, context.CancelFunc) {
	if lifetimeCtx == nil {
		lifetimeCtx = context.Background()
	}
	if operationCtx == nil {
		operationCtx = context.Background()
	}
	ctx, cancel := context.WithCancel(operationCtx)
	stop := context.AfterFunc(lifetimeCtx, cancel)
	if lifetimeCtx.Err() != nil {
		cancel()
	}
	return ctx, func() { stop(); cancel() }
}

func contextError(ctx context.Context, err error, fallback string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return errors.New(fallback)
}

func namespaceWithoutValidator(key string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%s\x00session:%d", key, time.Now().UnixNano())
	}
	return key + "\x00session:" + hex.EncodeToString(b[:])
}

func (r *Remote) Key() string { return r.key }

// DownloadStats reports the cache-wide range scheduler capacity available to
// this reader. It deliberately exposes aggregate counters only.
func (r *Remote) DownloadStats() DownloadStats {
	if r == nil || r.cache == nil {
		return DownloadStats{}
	}
	return r.cache.DownloadStats()
}

func (r *Remote) refresh(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, remoteRequestTimeout)
	defer cancel()
	r.mu.Lock()
	previous := r.resolvedURL
	r.mu.Unlock()
	link, err := r.cache.downloadLink(cctx, r.linkKey, previous, func(ctx context.Context) (string, error) {
		started := time.Now()
		defer func() { r.cache.IOStats().ObserveStage(iostats.StageURLResolve, time.Since(started)) }()
		return r.resolve(ctx)
	})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var classified *faults.Error
		if errors.As(err, &classified) {
			return &faults.Error{Kind: classified.Kind, Message: "could not resolve remote download URL", Retryable: classified.Retryable}
		}
		return contextError(ctx, err, "could not resolve remote download URL")
	}
	r.mu.Lock()
	r.url, r.urlUntil = link.url, link.until
	r.resolvedURL = link.url
	r.mu.Unlock()
	return nil
}

func (r *Remote) requestRange(ctx context.Context, start, end int64, conditional bool) (*http.Response, error) {
	priority := &downloadPriority{}
	if !workqueue.IsBackground(ctx) {
		priority.promoted.Store(true)
	}
	return r.requestRangeScheduled(ctx, start, end, conditional, cacheID(r.key), priority)
}

func (r *Remote) requestRangeScheduled(ctx context.Context, start, end int64, conditional bool, file string, priority *downloadPriority) (*http.Response, error) {
	retryBudget := recoveryBudgetFrom(ctx)
	requestRecovery, _ := ctx.Value(remoteRequestRecoveryContextKey{}).(*remoteRequestRecoveryState)
	for attempt := 0; attempt < 2; attempt++ {
		requestCtx, stopCache := combineContexts(r.cache.lifetimeCtx, ctx)
		r.mu.Lock()
		u := r.url
		expired := u == "" || time.Now().After(r.urlUntil)
		r.mu.Unlock()
		if expired {
			if err := r.refresh(requestCtx); err != nil {
				stopCache()
				return nil, err
			}
			r.mu.Lock()
			u = r.url
			r.mu.Unlock()
		}
		cctx, cancel := context.WithTimeout(requestCtx, remoteRequestTimeout)
		req, err := http.NewRequestWithContext(cctx, http.MethodGet, u, nil)
		if err != nil {
			cancel()
			stopCache()
			return nil, errors.New("could not create remote range request")
		}
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
		req.Header.Set("Accept-Encoding", "identity")
		if conditional {
			if r.etag != "" {
				req.Header.Set("If-Match", r.etag)
			} else if r.modified != "" {
				req.Header.Set("If-Unmodified-Since", r.modified)
			}
		}
		release, err := r.cache.acquireTransfer(cctx, end-start+1, file, priority)
		if err != nil {
			cancel()
			stopCache()
			return nil, err
		}
		requestStarted := time.Now()
		r.cache.remoteRecovery.attempts.Add(1)
		resp, err := r.client.Do(req)
		if err != nil {
			release()
			cancel()
			stopCache()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, &faults.Error{Kind: faults.Network, Message: "remote range request failed", Retryable: retryableTransportError(err)}
		}
		resp.Body = &cancelBody{ReadCloser: resp.Body, cancel: func() { cancel(); stopCache(); release() }, started: requestStarted, stats: r.cache.IOStats()}
		if (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone) && attempt == 0 {
			if _, ok := retryBudget.takeRetry(); !ok {
				return resp, nil
			}
			r.cache.remoteRecovery.retries.Add(1)
			resp.Body.Close()
			refreshCtx, stopRefresh := combineContexts(r.cache.lifetimeCtx, ctx)
			err = r.refresh(refreshCtx)
			stopRefresh()
			if err != nil {
				return nil, err
			}
			if requestRecovery != nil {
				requestRecovery.authRefreshed = true
			}
			continue
		}
		// Reuse the resolved CDN endpoint instead of repeating its redirect
		// on every sparse metadata request. Authentication failures still refresh.
		if resp.StatusCode == http.StatusPartialContent && resp.Request != nil {
			r.mu.Lock()
			if r.url == u {
				r.url = resp.Request.URL.String()
			}
			r.mu.Unlock()
		}
		return resp, nil
	}
	return nil, errors.New("remote range request failed")
}

type cancelBody struct {
	io.ReadCloser
	cancel  context.CancelFunc
	started time.Time
	first   time.Time
	read    uint64
	stats   interface {
		ObserveHTTPBodyTTFB(time.Duration)
		ObserveHTTPTransfer(time.Duration)
		AddDownloadedBytes(uint64)
	}
	once sync.Once
}

func (b *cancelBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		now := time.Now()
		if b.first.IsZero() {
			b.first = now
			if b.stats != nil && !b.started.IsZero() {
				b.stats.ObserveHTTPBodyTTFB(now.Sub(b.started))
			}
		}
		b.read += uint64(n)
	}
	return n, err
}

func (b *cancelBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(func() {
		if b.stats != nil {
			b.stats.AddDownloadedBytes(b.read)
			if !b.first.IsZero() {
				b.stats.ObserveHTTPTransfer(time.Since(b.first))
			}
		}
	})
	b.cancel()
	return err
}

func checkRangeResponse(resp *http.Response, start, end, total int64) error {
	if resp.StatusCode != http.StatusPartialContent {
		return errors.New("remote server did not honor byte range")
	}
	want := fmt.Sprintf("bytes %d-%d/%d", start, end, total)
	if strings.TrimSpace(resp.Header.Get("Content-Range")) != want {
		return errors.New("remote server returned an invalid Content-Range")
	}
	encoding := strings.TrimSpace(resp.Header.Get("Content-Encoding"))
	if encoding != "" && !strings.EqualFold(encoding, "identity") {
		return errors.New("remote server returned an encoded byte range")
	}
	if resp.ContentLength >= 0 && resp.ContentLength != end-start+1 {
		return errors.New("remote server returned an invalid range length")
	}
	return nil
}

func (r *Remote) ReadAt(p []byte, off int64) (int, error) {
	return r.ReadAtContext(r.lifetimeCtx, p, off)
}

// ReadAtContext reads from the remote while observing both ctx and the
// lifetime context supplied when the Remote was constructed.
func (r *Remote) ReadAtContext(operationCtx context.Context, p []byte, off int64) (int, error) {
	return r.readAtContext(operationCtx, p, off, remoteBlockSize, true)
}

// ReadMetadataAtContext reads sparse archive metadata through the same cached
// pages as ordinary reads. A small request only downloads the pages it needs.
func (r *Remote) ReadMetadataAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	return r.readAtContext(ctx, p, off, remoteCachePageSize, false)
}

// PrefetchRangeAtContext fetches and caches an exact range without retaining a
// caller-sized buffer. Callers must keep the requested range within their own
// logical file or archive member boundaries.
func (r *Remote) PrefetchRangeAtContext(operationCtx context.Context, off, size int64) error {
	ctx, cancel := combineContexts(r.lifetimeCtx, operationCtx)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	if off < 0 || size < 0 {
		return errors.New("invalid remote range")
	}
	if size == 0 {
		return nil
	}
	if off > r.size || size > r.size-off {
		return io.EOF
	}
	return r.ensureCachedRange(ctx, off, off+size)
}

// ReadRangeAtContext reads an exact range through the shared cache. It is
// intended for archive scanners and sequential prefetchers.
func (r *Remote) ReadRangeAtContext(operationCtx context.Context, p []byte, off int64) (int, error) {
	ctx, cancel := combineContexts(r.lifetimeCtx, operationCtx)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if off < 0 {
		return 0, errors.New("negative offset")
	}
	if len(p) == 0 {
		return 0, nil
	}
	if off >= r.size {
		return 0, io.EOF
	}
	want := min(int64(len(p)), r.size-off)
	r.recordCacheHitBytes(ctx, off, off+want, false)
	n, err := r.readRangeWithCache(ctx, p[:want], off)
	if err != nil {
		return n, err
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (r *Remote) readAtContext(operationCtx context.Context, p []byte, off int64, fetchSize int64, applicationRead bool) (int, error) {
	ctx, cancel := combineContexts(r.lifetimeCtx, operationCtx)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if off < 0 {
		return 0, errors.New("negative offset")
	}
	if len(p) == 0 {
		return 0, nil
	}
	if off >= r.size {
		return 0, io.EOF
	}
	want := len(p)
	if int64(want) > r.size-off {
		want = int(r.size - off)
	}
	r.recordCacheHitBytes(ctx, off, off+int64(want), applicationRead)
	read := 0
	for read < want {
		if err := ctx.Err(); err != nil {
			return read, err
		}
		pos := off + int64(read)
		blockStart := pos / fetchSize * fetchSize
		blockEnd := min(blockStart+fetchSize, r.size)
		copyEnd := min(off+int64(want), blockEnd)
		ensureStart, ensureEnd := blockStart, blockEnd
		if r.rangeHasCachedPages(pos, copyEnd) {
			// If a scanner or previous read already populated this area, avoid
			// fetching the rest of the normal 1 MiB read-ahead block.
			ensureStart, ensureEnd = pos, copyEnd
		}
		// Preserve the ordinary 1 MiB read-ahead whenever the entire block fits
		// in the cache. Larger reads go through the copy-as-you-go path below.
		if ensureEnd-ensureStart <= r.cache.max {
			if err := r.importLegacyPages(ctx, ensureStart, ensureEnd); err != nil {
				if read > 0 {
					return read, err
				}
				return 0, err
			}
			var prefetch *rangeFlight
			if len(r.cache.missingRanges(r.rangeID, ensureStart, ensureEnd)) != 0 {
				var owner bool
				var err error
				prefetch, owner, err = r.cache.beginRangeFlight(ctx, r.rangeID, ensureStart, ensureEnd)
				if err != nil {
					if read > 0 {
						return read, err
					}
					return 0, err
				}
				if owner {
					go r.runRangeFlight(prefetch)
				}
			}
			got, err := r.readRangeWithCache(ctx, p[read:read+int(copyEnd-pos)], pos)
			if prefetch != nil {
				r.cache.releaseRangeFlight(prefetch)
			}
			read += got
			if err != nil && err != io.EOF {
				return read, err
			}
			if got < int(copyEnd-pos) {
				break
			}
			continue
		}
		got, err := r.readRangeWithCache(ctx, p[read:read+int(copyEnd-pos)], pos)
		read += got
		if err != nil && err != io.EOF {
			return read, err
		}
		if got < int(copyEnd-pos) {
			break
		}
	}
	if read < len(p) {
		return read, io.EOF
	}
	return read, nil
}

func (r *Remote) recordCacheHitBytes(ctx context.Context, start, end int64, applicationRead bool) {
	if start >= end {
		return
	}
	covered := r.cache.observeRangeRead(r.rangeID, start, end, workqueue.IsBackground(ctx), applicationRead)
	if stats := r.cache.IOStats(); stats != nil {
		stats.AddCacheHitBytes(uint64(max(covered, 0)))
	}
}

// ensureCachedRange stores missing bytes as immutable range extents. Each
// contiguous HTTP response is one blob, regardless of its byte size.
func (r *Remote) ensureCachedRange(ctx context.Context, start, end int64) error {
	if start >= end {
		return nil
	}
	if end-start > r.cache.max {
		r.cache.recordENOSPC()
		return syscall.ENOSPC
	}
	firstPage, lastPage := start/remoteCachePageSize, (end-1)/remoteCachePageSize
	for page := firstPage; page <= lastPage; page++ {
		pageStart := page * remoteCachePageSize
		pageEnd := min(pageStart+remoteCachePageSize, r.size)
		if err := r.importLegacyPage(ctx, page, pageStart, pageEnd); err != nil {
			return err
		}
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		missing := r.cache.missingRanges(r.rangeID, start, end)
		if len(missing) == 0 {
			return nil
		}
		run := missing[0]
		flight, owner, err := r.cache.beginRangeFlight(ctx, r.rangeID, run.start, run.end)
		if err != nil {
			return err
		}
		if owner {
			go r.runRangeFlight(flight)
		}
		select {
		case <-ctx.Done():
			r.cache.releaseRangeFlight(flight)
			return ctx.Err()
		case <-flight.done:
			err = flight.err
			r.cache.releaseRangeFlight(flight)
			if errors.Is(err, context.Canceled) && ctx.Err() == nil {
				continue
			}
			if err != nil {
				return err
			}
		}
	}
}

func (r *Remote) runRangeFlight(flight *rangeFlight) {
	var err error
	// One range flight shares a single recovery deadline across any staging
	// segments it needs to fill. Each segment's retry loop inherits this bound.
	recoveryCtx, cancelRecovery := context.WithTimeout(flight.ctx, remoteRecoveryBudget)
	defer cancelRecovery()
	recoveryCtx = context.WithValue(recoveryCtx, remoteRecoveryBudgetContextKey{}, &remoteRecoveryBudgetState{})
	// Recheck coverage in case an extent was published just before this flight
	// was registered.
	for _, gap := range r.cache.missingRanges(r.rangeID, flight.start, flight.end) {
		gap := gap
		limit := r.cache.downloads.cfg.MaxInFlightBytes
		if !flight.priority.promoted.Load() {
			limit -= r.cache.downloads.cfg.ForegroundReservedBytes
		}
		if limit < 1 {
			limit = r.cache.downloads.cfg.MaxInFlightBytes
		}
		for start := gap.start; start < gap.end; {
			end := min(gap.end, start+limit)
			release, stageErr := r.cache.staging.Acquire(recoveryCtx, end-start, r.rangeID, flight.priority)
			if stageErr != nil {
				err = stageErr
				break
			}
			class := foregroundClass(workqueue.IsBackground(flight.ctx))
			h, fillErr := r.cache.AcquireRangeWithProgress(recoveryCtx, r.rangeID, start, end, class, flight.progress, func(fetchCtx context.Context, w io.Writer) error {
				return r.fetchRangeTo(fetchCtx, start, end, w, flight.priority)
			})
			release()
			if fillErr != nil {
				err = fillErr
				break
			}
			flight.progress.addPin(h)
			start = end
		}
		if err != nil {
			break
		}
	}
	r.cache.finishRangeFlight(r.rangeID, flight, err)
}

func (r *Remote) rangeHasCachedPages(start, end int64) bool {
	if start >= end {
		return false
	}
	missing := r.cache.missingRanges(r.rangeID, start, end)
	covered := end - start
	for _, gap := range missing {
		covered -= gap.end - gap.start
	}
	if covered > 0 {
		return true
	}
	// Prior releases stored 1 MiB data blobs. Check once per legacy block,
	// then check old sparse 64 KiB keys for pages actually touched.
	firstBlock, lastBlock := start/remoteBlockSize, (end-1)/remoteBlockSize
	for block := firstBlock; block <= lastBlock; block++ {
		blockStart := block * remoteBlockSize
		blockSize := min(remoteBlockSize, r.size-blockStart)
		h, err := r.cache.existing(fmt.Sprintf("remote:%s:%d:%d", r.key, block, blockSize), blockSize)
		if err == nil && h != nil {
			_ = h.Close()
			return true
		}
	}
	for page := start / remoteCachePageSize; page <= (end-1)/remoteCachePageSize; page++ {
		pageStart := page * remoteCachePageSize
		pageSize := min(remoteCachePageSize, r.size-pageStart)
		for _, key := range r.legacyPageKeys(page, pageSize) {
			h, err := r.cache.existing(key, pageSize)
			if err == nil && h != nil {
				_ = h.Close()
				return true
			}
		}
	}
	return false
}

func (r *Remote) legacyPageKeys(page, pageSize int64) []string {
	return []string{
		fmt.Sprintf("remote-page:%s:%d", r.key, page),
		fmt.Sprintf("remote-meta:%s:%d:%d", r.key, page, pageSize),
	}
}

func (r *Remote) importLegacyPage(ctx context.Context, page, pageStart, pageEnd int64) error {
	if len(r.cache.missingRanges(r.rangeID, pageStart, pageEnd)) == 0 {
		return nil
	}
	// Import a former full data block as one extent. This keeps compatibility
	// with persistent caches created before range extents were introduced.
	blockStart := pageStart / remoteBlockSize * remoteBlockSize
	blockEnd := min(blockStart+remoteBlockSize, r.size)
	if blockEnd-blockStart <= r.cache.max {
		blockKey := fmt.Sprintf("remote:%s:%d:%d", r.key, blockStart/remoteBlockSize, blockEnd-blockStart)
		if h, err := r.cache.existing(blockKey, blockEnd-blockStart); err != nil {
			return err
		} else if h != nil {
			data := make([]byte, blockEnd-blockStart)
			_, readErr := h.ReadAt(data, 0)
			_ = h.Close()
			if readErr != nil {
				return readErr
			}
			extent, fillErr := r.cache.AcquireRange(ctx, r.rangeID, blockStart, blockEnd, func(_ context.Context, w io.Writer) error { _, e := w.Write(data); return e })
			if fillErr != nil {
				return fillErr
			}
			_ = extent.Close()
			return nil
		}
	}
	pageSize := pageEnd - pageStart
	if pageSize > r.cache.max {
		return nil
	}
	for _, key := range r.legacyPageKeys(page, pageSize) {
		h, err := r.cache.existing(key, pageSize)
		if err != nil {
			return err
		}
		if h == nil {
			continue
		}
		data := make([]byte, pageSize)
		_, readErr := h.ReadAt(data, 0)
		_ = h.Close()
		if readErr != nil {
			return readErr
		}
		extent, fillErr := r.cache.AcquireRange(ctx, r.rangeID, pageStart, pageEnd, func(_ context.Context, w io.Writer) error { _, e := w.Write(data); return e })
		if fillErr != nil {
			return fillErr
		}
		_ = extent.Close()
		return nil
	}
	return nil
}

func (r *Remote) importLegacyPages(ctx context.Context, start, end int64) error {
	if start >= end {
		return nil
	}
	first, last := start/remoteCachePageSize, (end-1)/remoteCachePageSize
	for page := first; page <= last; page++ {
		pageStart := page * remoteCachePageSize
		pageEnd := min(pageStart+remoteCachePageSize, r.size)
		if err := r.importLegacyPage(ctx, page, pageStart, pageEnd); err != nil {
			return err
		}
	}
	return nil
}

func (r *Remote) readRangeWithCache(ctx context.Context, p []byte, off int64) (int, error) {
	end := off + int64(len(p))
	if err := r.importLegacyPages(ctx, off, end); err != nil {
		return 0, err
	}
	// Copy every cache hit before filling holes. As gaps are fetched, LRU may
	// evict any unpinned extent (including another part of this request); the
	// caller's buffer preserves those bytes and prevents a refetch loop.
	parts, err := r.cache.pinAvailableRange(r.rangeID, off, end, !workqueue.IsBackground(ctx))
	if err != nil {
		return 0, err
	}
	err = readPinnedRange(parts, p, off)
	read := contiguousRangeBytes(parts, off)
	gaps := uncoveredRangeGaps(parts, off, end)
	closeRangeParts(parts)
	if err != nil {
		return read, err
	}
	for _, gap := range gaps {
		if r.cache.max <= 0 {
			r.cache.recordENOSPC()
			return read, syscall.ENOSPC
		}
		counted := gap.start <= off+int64(read)
		for pos := gap.start; pos < gap.end; {
			if err := ctx.Err(); err != nil {
				return read, err
			}
			chunkEnd := min(gap.end, pos+r.cache.max)
			flight, owner, beginErr := r.cache.beginRangeFlight(ctx, r.rangeID, pos, chunkEnd)
			if beginErr != nil {
				return read, beginErr
			}
			if window := rangeWindowOwner(ctx); window != nil {
				window.trackFlight(flight)
			}
			if owner {
				go r.runRangeFlight(flight)
			}
			coveredEnd := min(chunkEnd, flight.end)
			retryFlight := false
			for pos < coveredEnd {
				start := int(pos - off)
				got, readErr := flight.progress.copyAvailable(ctx, p[start:start+int(coveredEnd-pos)], pos)
				if got > 0 {
					// Progressive consumers need the containing response to finish even
					// when they are background decoders reading a small piece. Otherwise
					// releasing this read cancels the block and the next piece starts it
					// again. Explicit RangeWindows already own their flight references.
					if rangeWindowOwner(ctx) == nil {
						flight.progress.retainTask()
					}
					if !workqueue.IsBackground(ctx) {
						flight.progress.consume(r.cache, pos, pos+int64(got))
					}
					pos += int64(got)
					if counted {
						read += got
					}
					continue
				}
				if errors.Is(readErr, io.EOF) {
					// Successful publication may have raced the cache-gap snapshot.
					break
				}
				if readErr != nil {
					if errors.Is(readErr, context.Canceled) && ctx.Err() == nil {
						r.cache.releaseRangeFlight(flight)
						retryFlight = true
						break
					}
					r.cache.releaseRangeFlight(flight)
					return read, readErr
				}
			}
			if retryFlight {
				continue
			}
			r.cache.releaseRangeFlight(flight)
			if pos < coveredEnd {
				parts, pinned, pinErr := r.cache.pinRange(r.rangeID, pos, coveredEnd, !workqueue.IsBackground(ctx))
				if pinErr != nil {
					return read, pinErr
				}
				if !pinned {
					r.cache.recordENOSPC()
					return read, syscall.ENOSPC
				}
				n := int(coveredEnd - pos)
				if err = readPinnedRange(parts, p[int(pos-off):int(pos-off)+n], pos); err != nil {
					closeRangeParts(parts)
					return read, err
				}
				closeRangeParts(parts)
				if counted {
					read += n
				}
				pos = coveredEnd
			}
		}
	}
	return len(p), nil
}

func contiguousRangeBytes(parts []pinnedRangePart, start int64) int {
	cursor := start
	for _, part := range parts {
		if part.start > cursor {
			break
		}
		if part.end > cursor {
			cursor = part.end
		}
	}
	return int(cursor - start)
}

func uncoveredRangeGaps(parts []pinnedRangePart, start, end int64) []byteRange {
	var gaps []byteRange
	cursor := start
	for _, part := range parts {
		if part.end <= cursor || part.start >= end {
			continue
		}
		if part.start > cursor {
			gaps = append(gaps, byteRange{start: cursor, end: min(part.start, end)})
		}
		if part.end > cursor {
			cursor = part.end
		}
		if cursor >= end {
			break
		}
	}
	if cursor < end {
		gaps = append(gaps, byteRange{start: cursor, end: end})
	}
	return gaps
}

func (r *Remote) fetchRangeTo(ctx context.Context, start, end int64, w io.Writer, priority *downloadPriority) error {
	chunkLimit := r.cache.downloads.cfg.RequestChunkBytes
	if maxBackground := r.cache.downloads.cfg.MaxInFlightBytes - r.cache.downloads.cfg.ForegroundReservedBytes; chunkLimit > maxBackground {
		chunkLimit = maxBackground
	}
	for cursor := start; cursor < end; {
		chunkEnd := min(end, cursor+chunkLimit)
		if priority != nil && priority.promoted.Load() {
			chunkEnd = min(end, cursor+r.cache.downloads.cfg.RequestChunkBytes)
		}
		if err := r.fetchOneRange(ctx, cursor, chunkEnd, w, priority); err != nil {
			return err
		}
		cursor = chunkEnd
	}
	return nil
}

func (r *Remote) fetchOneRange(ctx context.Context, start, end int64, w io.Writer, priority *downloadPriority) error {
	return r.fetchOneRangeWithRecovery(ctx, start, end, w, priority)
}
