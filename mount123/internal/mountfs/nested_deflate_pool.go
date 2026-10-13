package mountfs

import (
	"archive/zip"
	"container/list"
	"context"
	"math"
	"sync"
	"time"

	"github.com/binghezhouke/123/mount123/internal/storage"
)

const nestedDeflateResidentUnit int64 = 1 << 20

func nestedZIPRawInputs(input archiveReaderAt) (int64, bool) {
	switch input := input.(type) {
	case *storage.Remote:
		return 1, input != nil
	case *volumeReaderAt:
		return int64(len(input.parts)), len(input.parts) > 0 && len(input.parts) == len(input.sizes)
	case archiveOffsetInput:
		return nestedZIPRawInputs(input.input)
	case archiveConcatInput:
		var count int64
		for _, part := range input.parts {
			inputs, ok := nestedZIPRawInputs(part)
			if !ok || inputs > math.MaxInt64-count {
				return 0, false
			}
			count += inputs
		}
		return count, count > 0
	default:
		// A disc or arbitrary adapter can conceal a non-ZIP decoder. Its
		// dictionary and lifetime cannot be charged by this pool.
		return 0, false
	}
}

// This is an additional resident budget, separate from the metadata LRU.
// Charge entire retained ancestor indexes and a conservative allowance per
// known ZIP input, including its decoder window, Huffman tables and buffers.
func nestedDeflateResidentBytes(n *nestedArchive, m *member) (int64, bool) {
	clearZIP := func(m *member) bool {
		return m != nil && !m.encrypted && m.flags&1 == 0 && m.aes == nil && (m.method == zip.Store || m.method == zip.Deflate)
	}
	if n == nil || !clearZIP(m) || m.method != zip.Deflate || n.indexBytes < 0 {
		return 0, false
	}
	inputs := int64(1) // Selected Deflate decoder.
	for view := n; view != nil; view = view.parent {
		if view.format != ".zip" || view.index == nil {
			return 0, false
		}
		if view.parent == nil {
			if view.index.prepared == nil {
				return 0, false
			}
			rawInputs, ok := nestedZIPRawInputs(view.index.prepared.input)
			if !ok || rawInputs > math.MaxInt64-inputs {
				return 0, false
			}
			inputs += max(rawInputs, int64(len(view.index.prepared.layout.sizes)))
			continue
		}
		if len(view.lazyInputs) == 0 {
			return 0, false
		}
		for _, input := range view.lazyInputs {
			part, ok := input.(*nestedVolumeInput)
			if !ok || part.parent != view.parent || !clearZIP(part.member) {
				return 0, false
			}
			inputs++
		}
	}
	if inputs > (math.MaxInt64-n.indexBytes-(32<<10))/nestedDeflateResidentUnit {
		return 0, false
	}
	return n.indexBytes + inputs*nestedDeflateResidentUnit + (32 << 10), true
}

type nestedDeflatePool struct {
	mu       sync.Mutex
	entries  map[string]*nestedDeflateEntry
	idle     list.List
	maxItems int
	maxBytes int64
	used     int64
	ttl      time.Duration
	closed   bool
}

type nestedDeflateEntry struct {
	key        string
	reader     *nestedMemberReader
	bytes      int64
	release    func()
	busy       bool
	generation uint64
	node       *list.Element
	timer      *time.Timer
	closeOnce  sync.Once
}

func newNestedDeflatePool(maxItems int, maxBytes int64, ttl time.Duration) *nestedDeflatePool {
	return &nestedDeflatePool{entries: make(map[string]*nestedDeflateEntry), maxItems: maxItems, maxBytes: maxBytes, ttl: ttl}
}

func (e *nestedDeflateEntry) close() {
	e.closeOnce.Do(func() {
		_ = e.reader.Close()
		e.release()
	})
}

// detach is called with the pool locked. Recursive ancestor release must run
// outside that lock: a retained view may itself own another pool lease.
func (p *nestedDeflatePool) detach(e *nestedDeflateEntry) {
	delete(p.entries, e.key)
	p.used -= e.bytes
	e.generation++
	if e.timer != nil {
		e.timer.Stop()
		e.timer = nil
	}
	if e.node != nil {
		p.idle.Remove(e.node)
		e.node = nil
	}
}

func (p *nestedDeflatePool) acquire(ctx context.Context, key string, bytes int64, parent *nestedArchive, candidate *nestedMemberReader) (*nestedDeflateLease, bool) {
	if p == nil || ctx.Err() != nil || bytes <= 0 || bytes > p.maxBytes || p.maxItems <= 0 {
		return nil, false
	}
	releaseParent, err := parent.retain()
	if err != nil {
		return nil, false
	}
	var retired []*nestedDeflateEntry
	p.mu.Lock()
	if p.closed || ctx.Err() != nil {
		p.mu.Unlock()
		releaseParent()
		return nil, false
	}
	if e := p.entries[key]; e != nil {
		if e.busy {
			p.mu.Unlock()
			releaseParent()
			return nil, false
		}
		e.busy = true
		e.generation++
		if e.timer != nil {
			e.timer.Stop()
			e.timer = nil
		}
		p.idle.Remove(e.node)
		e.node = nil
		p.mu.Unlock()
		releaseParent()
		_ = candidate.Close()
		return &nestedDeflateLease{pool: p, entry: e}, true
	}
	for (len(p.entries) >= p.maxItems || p.used > p.maxBytes-bytes) && p.idle.Len() > 0 {
		e := p.idle.Back().Value.(*nestedDeflateEntry)
		p.detach(e)
		retired = append(retired, e)
	}
	if len(p.entries) >= p.maxItems || p.used > p.maxBytes-bytes {
		p.mu.Unlock()
		for _, e := range retired {
			e.close()
		}
		releaseParent()
		return nil, false
	}
	candidate.poolIdle = true
	e := &nestedDeflateEntry{key: key, reader: candidate, bytes: bytes, release: releaseParent, busy: true}
	p.entries[key], p.used = e, p.used+bytes
	p.mu.Unlock()
	for _, old := range retired {
		old.close()
	}
	return &nestedDeflateLease{pool: p, entry: e}, true
}

func (p *nestedDeflatePool) release(e *nestedDeflateEntry) {
	e.reader.mu.Lock()
	continuation := !e.reader.closed.Load() && e.reader.stream != nil
	e.reader.mu.Unlock()
	p.mu.Lock()
	if p.entries[e.key] != e {
		p.mu.Unlock()
		return
	}
	if !continuation || p.closed {
		p.detach(e)
		p.mu.Unlock()
		e.close()
		return
	}
	e.busy = false
	e.generation++
	generation := e.generation
	e.node = p.idle.PushFront(e)
	e.timer = time.AfterFunc(p.ttl, func() { p.expire(e, generation) })
	p.mu.Unlock()
}

func (p *nestedDeflatePool) expire(e *nestedDeflateEntry, generation uint64) {
	p.mu.Lock()
	if p.entries[e.key] != e || e.busy || e.generation != generation {
		p.mu.Unlock()
		return
	}
	p.detach(e)
	p.mu.Unlock()
	e.close()
}

func (p *nestedDeflatePool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	retired := make([]*nestedDeflateEntry, 0, len(p.entries))
	for _, e := range p.entries {
		p.detach(e)
		retired = append(retired, e)
	}
	p.mu.Unlock()
	for _, e := range retired {
		e.close()
	}
}

// A lease serializes its Close with in-flight reads, so a returned decoder
// cannot be borrowed while the previous owner is still using it.
type nestedDeflateLease struct {
	mu        sync.Mutex
	pool      *nestedDeflatePool
	entry     *nestedDeflateEntry
	closed    bool
	inflight  sync.WaitGroup
	closeOnce sync.Once
}

func (l *nestedDeflateLease) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return 0, storage.ErrClosed
	}
	l.inflight.Add(1)
	l.mu.Unlock()
	defer l.inflight.Done()
	// The decoder owner resets on failed I/O while holding its operation lock.
	// A canceled waiter must not acquire that lock or reset another request's
	// healthy continuation.
	return l.entry.reader.ReadAtContext(ctx, p, off)
}

func (l *nestedDeflateLease) Close() error {
	l.closeOnce.Do(func() {
		l.mu.Lock()
		l.closed = true
		l.mu.Unlock()
		// Add is guarded by the closed flag under the same lock. No new read
		// can enter once Wait begins, and readers never wait behind this Close.
		l.inflight.Wait()
		l.pool.release(l.entry)
	})
	return nil
}
