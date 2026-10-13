package mountfs

import (
	"container/list"
	"context"
	"hash/crc32"
	"io"
	"sync"
	"syscall"

	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/bodgit/sevenzip"
)

// This is an additional bound on retained decoder state, independent of disk
// capacity and the ZIP continuation pool. Idle state survives until LRU eviction
// or shutdown; immutable archive bytes do not require a time-based invalidation.
const nestedSolid7zMemoryBytes int64 = 128 << 20

type nestedSolid7zPool struct {
	mu             sync.Mutex
	entries        map[string]*nestedSolid7zEntry
	idle           list.List
	used, maxBytes int64
	maxItems       int
	retiring       int // Still charged until decoder/ancestor Close returns.
	closed         bool
	stop           chan struct{}
}

type nestedSolid7zEntry struct {
	key           string
	charge        int64
	busy          bool
	ready         chan struct{}
	node          *list.Element
	state         *nestedSolid7zState
	releaseParent func()
}

func newNestedSolid7zPool(items int, bytes int64) *nestedSolid7zPool {
	return &nestedSolid7zPool{entries: make(map[string]*nestedSolid7zEntry), maxItems: items, maxBytes: bytes, stop: make(chan struct{})}
}

// acquire runs only inside an admitted synchronous nested decode. The producer
// already owns its permit and lends it to ancestors. Waiting for this turn never
// holds the pool lock or requires a producer to obtain another permit.
func (p *nestedSolid7zPool) acquire(ctx context.Context, key string, charge int64, parent *nestedArchive) (*nestedSolid7zEntry, error) {
	if p == nil || charge <= 0 || charge > p.maxBytes || p.maxItems <= 0 {
		return nil, nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, storage.ErrClosed
		}
		if e := p.entries[key]; e != nil {
			if e.busy {
				ready := e.ready
				p.mu.Unlock()
				select {
				case <-ready:
					continue
				case <-p.stop:
					return nil, storage.ErrClosed
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			p.idle.Remove(e.node)
			e.node, e.busy, e.ready = nil, true, make(chan struct{})
			p.mu.Unlock()
			return e, nil
		}
		if (len(p.entries)+p.retiring >= p.maxItems || p.used > p.maxBytes-charge) && p.idle.Len() > 0 {
			e := p.idle.Back().Value.(*nestedSolid7zEntry)
			p.detach(e)
			p.mu.Unlock()
			p.reclaim(e)
			continue
		}
		if len(p.entries)+p.retiring >= p.maxItems || p.used > p.maxBytes-charge {
			p.mu.Unlock()
			return nil, nil
		}
		// retain never calls into an ancestor. All releases occur outside the
		// pool lock; recursive lazy inputs can own leases in another pool.
		release, err := parent.retain()
		if err != nil {
			p.mu.Unlock()
			return nil, err
		}
		e := &nestedSolid7zEntry{key: key, charge: charge, busy: true, ready: make(chan struct{}), releaseParent: release}
		p.entries[key], p.used = e, p.used+charge
		p.mu.Unlock()
		return e, nil
	}
}

func (p *nestedSolid7zPool) detach(e *nestedSolid7zEntry) {
	delete(p.entries, e.key)
	p.retiring++
	if e.node != nil {
		p.idle.Remove(e.node)
		e.node = nil
	}
}

func (p *nestedSolid7zPool) reclaim(e *nestedSolid7zEntry) {
	if e.state != nil {
		_ = e.state.stream.Close()
		e.state = nil
	}
	e.releaseParent()
	p.mu.Lock()
	p.used -= e.charge
	p.retiring--
	p.mu.Unlock()
}

func (p *nestedSolid7zPool) release(e *nestedSolid7zEntry, healthy bool) {
	p.mu.Lock()
	retire := !healthy || p.closed || e.state == nil || e.state.finished
	if retire {
		p.detach(e)
	} else {
		e.node = p.idle.PushFront(e)
	}
	e.busy = false
	close(e.ready)
	p.mu.Unlock()
	if retire {
		p.reclaim(e)
	}
}

func (p *nestedSolid7zPool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	close(p.stop)
	var retired []*nestedSolid7zEntry
	for _, e := range p.entries {
		// Tree cancellation interrupts the request bound to active state. Its
		// owner releases the entry after I/O; never close a running decoder.
		if !e.busy {
			p.detach(e)
			retired = append(retired, e)
		}
	}
	p.mu.Unlock()
	for _, e := range retired {
		p.reclaim(e)
	}
}

// One borrower owns all mutable state. It hashes every consumed byte, including
// discarded prefixes; ordinary member boundaries never drain later members.
type nestedSolid7zState struct {
	stream              io.ReadCloser
	operation           *nestedReadContext
	source              *nestedSourceReader
	layout              *sevenzip.StreamLayout
	position            int64
	member              int
	memberCRC, groupCRC uint32
	finished            bool
}

func (s *nestedSolid7zState) boundary() error {
	for s.member < len(s.layout.Members) {
		m := s.layout.Members[s.member]
		if s.position != m.Offset+int64(m.Size) {
			break
		}
		if m.CRC.Defined && s.memberCRC != m.CRC.Value {
			return errArchiveIntegrity
		}
		s.member++
		s.memberCRC = 0
	}
	return nil
}

func (s *nestedSolid7zState) consume(ctx context.Context, end int64, w io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if end < s.position || end > int64(s.layout.UncompressedSize) {
		return syscall.EINVAL
	}
	var buffer [64 << 10]byte
	for s.position < end {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.boundary(); err != nil {
			return err
		}
		if s.member >= len(s.layout.Members) {
			return io.ErrUnexpectedEOF
		}
		m := s.layout.Members[s.member]
		amount := min(int64(len(buffer)), end-s.position, m.Offset+int64(m.Size)-s.position)
		if amount <= 0 {
			return syscall.ESTALE
		}
		n, err := s.stream.Read(buffer[:amount])
		if s.source != nil && s.source.sourceError() != nil {
			return s.source.sourceError()
		}
		if n < 0 || n > int(amount) {
			return syscall.EIO
		}
		if n > 0 {
			s.position += int64(n)
			s.memberCRC = crc32.Update(s.memberCRC, crc32.IEEETable, buffer[:n])
			s.groupCRC = crc32.Update(s.groupCRC, crc32.IEEETable, buffer[:n])
		}
		// Source errors supplied with final bytes take precedence over inferred
		// checksums. Failed cache fills discard any previously written bytes.
		if err != nil && err != io.EOF {
			return err
		}
		if err == io.EOF && s.position != int64(s.layout.UncompressedSize) {
			return io.ErrUnexpectedEOF
		}
		if check := s.boundary(); check != nil {
			return check
		}
		if n > 0 {
			written, writeErr := w.Write(buffer[:n])
			if writeErr != nil {
				return writeErr
			}
			if written != n {
				return io.ErrShortWrite
			}
		} else if err == nil {
			return io.ErrNoProgress
		}
	}
	if err := s.boundary(); err != nil {
		return err
	}
	if s.position == int64(s.layout.UncompressedSize) && !s.finished {
		if err := ctx.Err(); err != nil {
			return err
		}
		var tail [1]byte
		n, err := s.stream.Read(tail[:])
		if s.source != nil && s.source.sourceError() != nil {
			return s.source.sourceError()
		}
		if err != nil && err != io.EOF {
			return err
		}
		if n != 0 {
			return errArchiveIntegrity
		}
		if err != io.EOF {
			if err == nil {
				return io.ErrNoProgress
			}
			return err
		}
		if s.layout.CRC.Defined && s.groupCRC != s.layout.CRC.Value {
			return errArchiveIntegrity
		}
		if s.member != len(s.layout.Members) {
			return syscall.ESTALE
		}
		s.finished = true
	}
	return nil
}
