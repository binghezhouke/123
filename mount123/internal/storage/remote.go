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
	"time"
)

const remoteBlockSize int64 = 1 << 20
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
		resp, err := r.client.Do(req)
		if err != nil {
			cancel()
			return nil, contextError(ctx, err, "remote range request failed")
		}
		resp.Body = &cancelBody{ReadCloser: resp.Body, cancel: cancel}
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
	return r.readAtContext(operationCtx, p, off, remoteBlockSize, "remote")
}

// ReadMetadataAtContext avoids fetching a whole 1 MiB data block for
// every small header. Existing data blocks remain usable after upgrading.
func (r *Remote) ReadMetadataAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	ctx, cancel := combineContexts(r.lifetimeCtx, ctx)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if off >= 0 && off < r.size && len(p) > 0 {
		block := off / remoteBlockSize
		start := block * remoteBlockSize
		size := remoteBlockSize
		if size > r.size-start {
			size = r.size - start
		}
		if int64(len(p)) <= size-(off-start) {
			key := fmt.Sprintf("remote:%s:%d:%d", r.key, block, size)
			h, err := r.cache.existing(key, size)
			if err != nil {
				return 0, err
			}
			if h != nil {
				defer h.Close()
				return h.ReadAt(p, off-start)
			}
		}
	}
	return r.readAtContext(ctx, p, off, 64<<10, "remote-meta")
}

func (r *Remote) readAtContext(operationCtx context.Context, p []byte, off int64, blockSize int64, prefix string) (int, error) {
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
		block := pos / blockSize
		start := block * blockSize
		end := start + blockSize - 1
		if end >= r.size {
			end = r.size - 1
		}
		nblock := end - start + 1
		cacheKey := fmt.Sprintf("%s:%s:%d:%d", prefix, r.key, block, nblock)
		h, err := r.cache.Acquire(ctx, cacheKey, nblock, func(ctx context.Context, w io.Writer) error { return r.fetchBlock(ctx, start, end, w) })
		if err != nil {
			if read > 0 {
				return read, err
			}
			return 0, err
		}
		within := pos - start
		n := want - read
		if int64(n) > nblock-within {
			n = int(nblock - within)
		}
		got, e := h.ReadAt(p[read:read+n], within)
		ce := h.Close()
		if e != nil && e != io.EOF {
			return read, e
		}
		if ce != nil {
			return read, ce
		}
		read += got
		if got < n {
			break
		}
	}
	if read < len(p) {
		return read, io.EOF
	}
	return read, nil
}

func (r *Remote) fetchBlock(ctx context.Context, start, end int64, w io.Writer) error {
	resp, err := r.requestRange(ctx, start, end, true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusPreconditionFailed {
		return errors.New("remote file changed")
	}
	if err = checkRangeResponse(resp, start, end, r.size); err != nil {
		return err
	}
	if r.etag != "" && resp.Header.Get("ETag") != r.etag {
		return errors.New("remote file changed")
	}
	if r.etag == "" && r.modified != "" && resp.Header.Get("Last-Modified") != r.modified {
		return errors.New("remote file changed")
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, end-start+2))
	if err != nil {
		return contextError(ctx, err, "could not read remote range")
	}
	if n != end-start+1 {
		return errors.New("remote range body length mismatch")
	}
	return nil
}
