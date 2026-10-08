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
	"syscall"
	"time"
)

const remoteBlockSize int64 = 1 << 20
const remoteCachePageSize int64 = 64 << 10
const remoteRequestTimeout = 45 * time.Second

// ResolveURL obtains a temporary download URL. Authorization belongs in this
// resolver; the storage package only sends byte-range requests to its result.
type ResolveURL func(context.Context) (string, error)

type Remote struct {
	lifetimeCtx    context.Context
	cache          *Cache
	size           int64
	key            string
	linkKey        string
	resolve        ResolveURL
	client         *http.Client
	mu             sync.Mutex
	rangeID        string
	url            string
	resolvedURL    string
	urlUntil       time.Time
	etag, modified string
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
	r := &Remote{lifetimeCtx: lifetimeCtx, cache: cache, size: size, key: key, linkKey: key, resolve: resolve, client: http.DefaultClient}
	if size == 0 {
		r.key = namespaceWithoutValidator(key)
		r.rangeID = cacheID(r.key)
		return r, nil
	}
	if err := r.refresh(ctx); err != nil {
		return nil, err
	}
	resp, err := r.requestRange(ctx, 0, 0, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err = checkRangeResponse(resp, 0, 0, size); err != nil {
		return nil, err
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2))
	if err != nil {
		return nil, contextError(ctx, err, "remote range probe returned an invalid body")
	}
	if len(body) != 1 {
		return nil, errors.New("remote range probe returned an invalid body")
	}
	r.etag = resp.Header.Get("ETag")
	if strings.HasPrefix(strings.TrimSpace(r.etag), "W/") {
		r.etag = ""
	}
	r.modified = resp.Header.Get("Last-Modified")
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

func (r *Remote) refresh(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, remoteRequestTimeout)
	defer cancel()
	r.mu.Lock()
	previous := r.resolvedURL
	r.mu.Unlock()
	link, err := r.cache.downloadLink(cctx, r.linkKey, previous, r.resolve)
	if err != nil {
		return contextError(ctx, err, "could not resolve remote download URL")
	}
	r.mu.Lock()
	r.url, r.urlUntil = link.url, link.until
	r.resolvedURL = link.url
	r.mu.Unlock()
	return nil
}

func (r *Remote) requestRange(ctx context.Context, start, end int64, conditional bool) (*http.Response, error) {
	for attempt := 0; attempt < 2; attempt++ {
		r.mu.Lock()
		u := r.url
		expired := u == "" || time.Now().After(r.urlUntil)
		r.mu.Unlock()
		if expired {
			if err := r.refresh(ctx); err != nil {
				return nil, err
			}
			r.mu.Lock()
			u = r.url
			r.mu.Unlock()
		}
		cctx, cancel := context.WithTimeout(ctx, remoteRequestTimeout)
		req, err := http.NewRequestWithContext(cctx, http.MethodGet, u, nil)
		if err != nil {
			cancel()
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
		release, err := r.cache.acquireTransfer(cctx)
		if err != nil {
			cancel()
			return nil, err
		}
		resp, err := r.client.Do(req)
		if err != nil {
			release()
			cancel()
			return nil, contextError(ctx, err, "remote range request failed")
		}
		resp.Body = &cancelBody{ReadCloser: resp.Body, cancel: func() { cancel(); release() }}
		if (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone) && attempt == 0 {
			resp.Body.Close()
			if err = r.refresh(ctx); err != nil {
				return nil, err
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
	cancel context.CancelFunc
}

func (b *cancelBody) Close() error {
	err := b.ReadCloser.Close()
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
	return r.readAtContext(operationCtx, p, off, remoteBlockSize)
}

// ReadMetadataAtContext reads sparse archive metadata through the same cached
// pages as ordinary reads. A small request only downloads the pages it needs.
func (r *Remote) ReadMetadataAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	return r.readAtContext(ctx, p, off, remoteCachePageSize)
}

// ReadRangeAtContext reads an exact range through the shared page cache. It is
// intended for archive scanners that download a larger window in one HTTP
// request while retaining the same bytes for later ordinary reads.
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
	n, err := r.readRangeWithCache(ctx, p[:want], off)
	if err != nil {
		return n, err
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (r *Remote) readAtContext(operationCtx context.Context, p []byte, off int64, fetchSize int64) (int, error) {
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
			if err := r.ensureCachedRange(ctx, ensureStart, ensureEnd); err != nil {
				if read > 0 {
					return read, err
				}
				return 0, err
			}
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

// ensureCachedRange stores missing bytes as immutable range extents. Each
// contiguous HTTP response is one blob, regardless of its byte size.
func (r *Remote) ensureCachedRange(ctx context.Context, start, end int64) error {
	if start >= end {
		return nil
	}
	if end-start > r.cache.max {
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
		if !owner {
			continue
		}
		// The previous owner may have published the bytes just before this
		// flight was registered. Recheck coverage before issuing HTTP.
		for _, gap := range r.cache.missingRanges(r.rangeID, run.start, run.end) {
			gap := gap
			h, fillErr := r.cache.AcquireRange(ctx, r.rangeID, gap.start, gap.end, func(fetchCtx context.Context, w io.Writer) error {
				return r.fetchRangeTo(fetchCtx, gap.start, gap.end, w)
			})
			if fillErr != nil {
				err = fillErr
				break
			}
			_ = h.Close()
		}
		r.cache.finishRangeFlight(r.rangeID, flight, err)
		if err != nil {
			return err
		}
	}
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

func (r *Remote) readRangeWithCache(ctx context.Context, p []byte, off int64) (int, error) {
	end := off + int64(len(p))
	// Copy every cache hit before filling holes. As gaps are fetched, LRU may
	// evict any unpinned extent (including another part of this request); the
	// caller's buffer preserves those bytes and prevents a refetch loop.
	parts, err := r.cache.pinAvailableRange(r.rangeID, off, end)
	if err != nil {
		return 0, err
	}
	err = readPinnedRange(parts, p, off)
	closeRangeParts(parts)
	if err != nil {
		return 0, err
	}
	for _, gap := range r.cache.missingRanges(r.rangeID, off, end) {
		if r.cache.max <= 0 {
			return 0, syscall.ENOSPC
		}
		for pos := gap.start; pos < gap.end; {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			chunkEnd := min(gap.end, pos+r.cache.max)
			var chunkParts []pinnedRangePart
			pinned := false
			for attempt := 0; attempt < 2; attempt++ {
				if err := r.ensureCachedRange(ctx, pos, chunkEnd); err != nil {
					return 0, err
				}
				chunkParts, pinned, err = r.cache.pinRange(r.rangeID, pos, chunkEnd)
				if err != nil {
					return 0, err
				}
				if pinned {
					break
				}
			}
			if !pinned {
				return 0, syscall.ENOSPC
			}
			start := int(pos - off)
			err = readPinnedRange(chunkParts, p[start:start+int(chunkEnd-pos)], pos)
			closeRangeParts(chunkParts)
			if err != nil {
				return 0, err
			}
			pos = chunkEnd
		}
	}
	return len(p), nil
}

func (r *Remote) fetchRangeTo(ctx context.Context, start, end int64, w io.Writer) error {
	resp, err := r.requestRange(ctx, start, end-1, true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusPreconditionFailed {
		return errors.New("remote file changed")
	}
	if err = checkRangeResponse(resp, start, end-1, r.size); err != nil {
		return err
	}
	if r.etag != "" && resp.Header.Get("ETag") != r.etag {
		return errors.New("remote file changed")
	}
	if r.etag == "" && r.modified != "" && resp.Header.Get("Last-Modified") != r.modified {
		return errors.New("remote file changed")
	}
	expected := end - start
	_, err = io.CopyN(w, resp.Body, expected)
	if err != nil {
		return contextError(ctx, err, "could not read remote range")
	}
	var extra [1]byte
	nExtra, extraErr := io.ReadFull(resp.Body, extra[:])
	if nExtra != 0 {
		return errors.New("remote range body length mismatch")
	}
	if extraErr != io.EOF {
		return contextError(ctx, extraErr, "could not validate remote range length")
	}
	return nil
}
