package storage

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
)

// rangeProgress exposes only bytes already written to the cache staging file.
// It never buffers response data in memory.
type rangeProgress struct {
	mu       sync.Mutex
	changed  chan struct{}
	parts    []*rangeProgressPart
	pins     []*Handle
	err      error
	finished bool
	keep     bool
}

type rangeProgressPart struct {
	start, size, written int64
	file                 *os.File
	id                   string
	class                cacheClass
	hasUse               bool
	scanDir              int8
	lastUseStart         int64
	lastUseEnd           int64
	published            bool
}

type rangeProgressState struct {
	class        cacheClass
	hasUse       bool
	scanDir      int8
	lastUseStart int64
	lastUseEnd   int64
}

type rangeProgressUse struct {
	id         string
	start, end int64
}

func newRangeProgress() *rangeProgress { return &rangeProgress{changed: make(chan struct{})} }

func (p *rangeProgress) signalLocked() { close(p.changed); p.changed = make(chan struct{}) }

func (p *rangeProgress) addFile(start, size int64, id string, class cacheClass, file *os.File) *rangeProgressPart {
	p.mu.Lock()
	part := &rangeProgressPart{start: start, size: size, id: id, class: class, file: file}
	p.parts = append(p.parts, part)
	p.signalLocked()
	p.mu.Unlock()
	return part
}

func (p *rangeProgress) wrote(part *rangeProgressPart, n int) {
	if n <= 0 {
		return
	}
	p.mu.Lock()
	part.written += int64(n)
	p.signalLocked()
	p.mu.Unlock()
}

func (p *rangeProgress) finish(err error) {
	p.mu.Lock()
	p.err, p.finished = err, true
	p.signalLocked()
	p.mu.Unlock()
}

func (p *rangeProgress) close() {
	p.mu.Lock()
	for _, part := range p.parts {
		_ = part.file.Close()
	}
	p.parts = nil
	pins := p.pins
	p.pins = nil
	p.mu.Unlock()
	for _, pin := range pins {
		_ = pin.Close()
	}
}

func (p *rangeProgress) addPin(h *Handle) { p.mu.Lock(); p.pins = append(p.pins, h); p.mu.Unlock() }

// publishState snapshots access history while Cache.mu is held, preserving the
// lock order Cache.mu -> rangeProgress.mu. A later consume releases p.mu before
// updating the published cache entry.
func (p *rangeProgress) publishState(id string) (rangeProgressState, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, part := range p.parts {
		if part.id == id {
			part.published = true
			return rangeProgressState{class: part.class, hasUse: part.hasUse, scanDir: part.scanDir, lastUseStart: part.lastUseStart, lastUseEnd: part.lastUseEnd}, true
		}
	}
	return rangeProgressState{}, false
}

// consume records only successful foreground copies. It updates the part's
// bounded scan state under p.mu, then—after releasing p.mu—applies each new use
// to an already-published cache entry under Cache.mu.
func (p *rangeProgress) consume(cache *Cache, start, end int64) {
	p.mu.Lock()
	var published []rangeProgressUse
	for _, part := range p.parts {
		partEnd := min(end, part.start+part.written)
		useStart, useEnd := max(start, part.start), partEnd
		if useEnd <= useStart {
			continue
		}
		consumeClass(&part.class, &part.hasUse, &part.scanDir, &part.lastUseStart, &part.lastUseEnd, useStart, useEnd)
		if part.published {
			published = append(published, rangeProgressUse{id: part.id, start: useStart, end: useEnd})
		}
	}
	p.mu.Unlock()
	if len(published) == 0 {
		return
	}
	cache.mu.Lock()
	for _, use := range published {
		if entry := cache.entries[use.id]; entry != nil {
			cache.consumeEntryLocked(use.id, entry, use.start, use.end)
		}
	}
	cache.mu.Unlock()
}

func (p *rangeProgress) retainTask()    { p.mu.Lock(); p.keep = true; p.mu.Unlock() }
func (p *rangeProgress) retained() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.keep }

// copyAvailable copies the contiguous staged prefix beginning at absolute pos.
func (p *rangeProgress) copyAvailable(ctx context.Context, dst []byte, pos int64) (int, error) {
	for {
		p.mu.Lock()
		limit := pos
		var chosen *rangeProgressPart
		for _, part := range p.parts {
			end := part.start + part.written
			if part.start <= pos && pos < end {
				chosen, limit = part, end
				break
			}
			if part.start == pos && part.written > 0 {
				chosen, limit = part, end
				break
			}
		}
		if chosen != nil {
			n := int(min(int64(len(dst)), limit-pos))
			got, err := chosen.file.ReadAt(dst[:n], pos-chosen.start)
			p.mu.Unlock()
			if got != n {
				if err == nil || errors.Is(err, io.EOF) {
					err = io.ErrUnexpectedEOF
				}
				return got, err
			}
			return got, nil
		}
		if p.finished {
			err := p.err
			p.mu.Unlock()
			if err == nil {
				return 0, io.EOF
			}
			return 0, err
		}
		changed := p.changed
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-changed:
		}
	}
}

type progressWriter struct {
	w    io.Writer
	p    *rangeProgress
	part *rangeProgressPart
}

func (w *progressWriter) Write(b []byte) (int, error) {
	n, err := w.w.Write(b)
	w.p.wrote(w.part, n)
	return n, err
}
