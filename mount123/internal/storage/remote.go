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
	fetchGate      chan struct{}
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
	r := &Remote{lifetimeCtx: lifetimeCtx, cache: cache, size: size, key: key, linkKey: key, resolve: resolve, client: http.DefaultClient, fetchGate: make(chan struct{}, 1)}
	if size == 0 {
		r.key = namespaceWithoutValidator(key)
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
	end := off + want
	if err := r.ensureCachedRange(ctx, off, end); err != nil {
		return 0, err
	}
	n, err := r.readCachedRange(ctx, p[:want], off)
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
		if err := r.ensureCachedRange(ctx, ensureStart, ensureEnd); err != nil {
			if read > 0 {
				return read, err
			}
			return 0, err
		}
		got, err := r.readCachedRange(ctx, p[read:read+int(copyEnd-pos)], pos)
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

// ensureCachedRange stores every byte in [start,end) as canonical 64 KiB pages.
// One contiguous missing run is fetched with one HTTP range regardless of how
// many cache pages it spans.
func (r *Remote) ensureCachedRange(ctx context.Context, start, end int64) error {
	if start >= end {
		return nil
	}
	firstPageStart := start / remoteCachePageSize * remoteCachePageSize
	coveredEnd := min(((end-1)/remoteCachePageSize+1)*remoteCachePageSize, r.size)
	if coveredEnd-firstPageStart > r.cache.max {
		return syscall.ENOSPC
	}
	select {
	case r.fetchGate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-r.fetchGate }()
	first := start / remoteCachePageSize
	last := (end - 1) / remoteCachePageSize
	missing := make([]bool, last-first+1)
	for i := range missing {
		page := first + int64(i)
		pageStart := page * remoteCachePageSize
		pageSize := min(remoteCachePageSize, r.size-pageStart)
		h, err := r.cache.existing(r.pageKey(page), pageSize)
		if err != nil {
			return err
		}
		if h != nil {
			_ = h.Close()
			continue
		}
		if err := r.copyLegacyPage(ctx, page, pageSize); err != nil {
			return err
		}
		missing[i] = true
		h, err = r.cache.existing(r.pageKey(page), pageSize)
		if err != nil {
			return err
		}
		if h != nil {
			_ = h.Close()
			missing[i] = false
		}
	}
	for i := 0; i < len(missing); {
		if !missing[i] {
			i++
			continue
		}
		j := i + 1
		for j < len(missing) && missing[j] {
			j++
		}
		runStart := (first + int64(i)) * remoteCachePageSize
		runEnd := min((first+int64(j))*remoteCachePageSize, r.size)
		buf := make([]byte, runEnd-runStart)
		n, err := r.fetchRange(ctx, runStart, runEnd, buf)
		if err != nil {
			return err
		}
		if n != len(buf) {
			return errors.New("remote range body length mismatch")
		}
		for k := i; k < j; k++ {
			page := first + int64(k)
			pageStart := page * remoteCachePageSize
			pageEnd := min(pageStart+remoteCachePageSize, r.size)
			part := buf[pageStart-runStart : pageEnd-runStart]
			h, err := r.cache.Acquire(ctx, r.pageKey(page), int64(len(part)), func(_ context.Context, w io.Writer) error { _, writeErr := w.Write(part); return writeErr })
			if err != nil {
				return err
			}
			_ = h.Close()
		}
		i = j
	}
	return nil
}

func (r *Remote) rangeHasCachedPages(start, end int64) bool {
	if start >= end {
		return false
	}
	first, last := start/remoteCachePageSize, (end-1)/remoteCachePageSize
	for page := first; page <= last; page++ {
		pageStart := page * remoteCachePageSize
		size := min(remoteCachePageSize, r.size-pageStart)
		h, err := r.cache.existing(r.pageKey(page), size)
		if err == nil && h != nil {
			_ = h.Close()
			return true
		}
		if r.legacyPageExists(page, size) {
			return true
		}
	}
	return false
}

func (r *Remote) legacyPageKeys(page, pageSize int64) []string {
	keys := []string{fmt.Sprintf("remote-meta:%s:%d:%d", r.key, page, pageSize)}
	pageStart := page * remoteCachePageSize
	block := pageStart / remoteBlockSize
	blockStart := block * remoteBlockSize
	blockSize := min(remoteBlockSize, r.size-blockStart)
	keys = append(keys, fmt.Sprintf("remote:%s:%d:%d", r.key, block, blockSize))
	return keys
}

func (r *Remote) legacyPageExists(page, pageSize int64) bool {
	for _, key := range r.legacyPageKeys(page, pageSize) {
		size := pageSize
		if strings.HasPrefix(key, "remote:") {
			size = min(remoteBlockSize, r.size-(page*remoteCachePageSize)/remoteBlockSize*remoteBlockSize)
		}
		h, err := r.cache.existing(key, size)
		if err == nil && h != nil {
			_ = h.Close()
			return true
		}
	}
	return false
}

func (r *Remote) copyLegacyPage(ctx context.Context, page, pageSize int64) error {
	pageStart := page * remoteCachePageSize
	for _, key := range r.legacyPageKeys(page, pageSize) {
		legacySize := pageSize
		within := pageStart % remoteCachePageSize
		if strings.HasPrefix(key, "remote:") {
			blockStart := pageStart / remoteBlockSize * remoteBlockSize
			legacySize = min(remoteBlockSize, r.size-blockStart)
			within = pageStart - blockStart
		}
		h, err := r.cache.existing(key, legacySize)
		if err != nil {
			return err
		}
		if h == nil {
			continue
		}
		data := make([]byte, pageSize)
		_, readErr := h.ReadAt(data, within)
		_ = h.Close()
		if readErr != nil {
			return readErr
		}
		pageHandle, err := r.cache.Acquire(ctx, r.pageKey(page), pageSize, func(_ context.Context, w io.Writer) error { _, e := w.Write(data); return e })
		if err != nil {
			return err
		}
		_ = pageHandle.Close()
		return nil
	}
	return nil
}

func (r *Remote) pageKey(page int64) string { return fmt.Sprintf("remote-page:%s:%d", r.key, page) }

func (r *Remote) readCachedRange(ctx context.Context, p []byte, off int64) (int, error) {
	read := 0
	for read < len(p) {
		if err := ctx.Err(); err != nil {
			return read, err
		}
		pos := off + int64(read)
		page := pos / remoteCachePageSize
		start := page * remoteCachePageSize
		size := min(remoteCachePageSize, r.size-start)
		h, err := r.cache.existing(r.pageKey(page), size)
		if err != nil {
			return read, err
		}
		if h == nil {
			return read, errors.New("remote cache page disappeared")
		}
		n := min(int64(len(p)-read), size-(pos-start))
		got, e := h.ReadAt(p[read:read+int(n)], pos-start)
		_ = h.Close()
		read += got
		if e != nil && e != io.EOF {
			return read, e
		}
		if int64(got) < n {
			break
		}
	}
	if read < len(p) {
		return read, io.EOF
	}
	return read, nil
}

func (r *Remote) fetchRange(ctx context.Context, start, end int64, dst []byte) (int, error) {
	resp, err := r.requestRange(ctx, start, end-1, true)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusPreconditionFailed {
		return 0, errors.New("remote file changed")
	}
	if err = checkRangeResponse(resp, start, end-1, r.size); err != nil {
		return 0, err
	}
	if r.etag != "" && resp.Header.Get("ETag") != r.etag {
		return 0, errors.New("remote file changed")
	}
	if r.etag == "" && r.modified != "" && resp.Header.Get("Last-Modified") != r.modified {
		return 0, errors.New("remote file changed")
	}
	n, err := io.ReadFull(io.LimitReader(resp.Body, end-start+1), dst)
	if err != nil {
		return n, contextError(ctx, err, "could not read remote range")
	}
	return n, nil
}
