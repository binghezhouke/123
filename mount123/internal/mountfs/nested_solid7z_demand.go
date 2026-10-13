package mountfs

// A demand belongs to one cache fill. The active decoder may copy matching
// output into its buffer while its caller waits; only a healthy release makes
// that output available. No writer, request or plaintext outlives the fill.
type nestedSolid7zDemand struct {
	start, length int64
	buffer        []byte
	filled        int64
	ready         bool
	owner         *nestedSolid7zEntry
}

// Caller owns p.mu. Output reservations share the pool's total resident bound
// and a smaller dedicated cap so waiting output cannot consume all decoder
// capacity. A failed reservation simply leaves the ordinary replay path.
func (p *nestedSolid7zPool) registerDemandLocked(e *nestedSolid7zEntry, d *nestedSolid7zDemand) {
	if d == nil || d.owner != nil || d.length <= 0 || d.length > p.maxPendingBytes {
		return
	}
	if d.buffer == nil {
		if p.pendingBytes > p.maxPendingBytes-d.length || p.used > p.maxBytes-d.length {
			return
		}
		d.buffer = make([]byte, int(d.length))
		p.pendingBytes += d.length
		p.used += d.length
	}
	d.filled, d.ready, d.owner = 0, false, e
	e.demands = append(e.demands, d)
}

func (p *nestedSolid7zPool) releaseDemand(d *nestedSolid7zDemand) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d.owner != nil {
		e := d.owner
		for i, candidate := range e.demands {
			if candidate == d {
				e.demands = append(e.demands[:i], e.demands[i+1:]...)
				break
			}
		}
		d.owner = nil
	}
	p.used -= int64(len(d.buffer))
	p.pendingBytes -= int64(len(d.buffer))
	d.buffer = nil
}

// Copy only queued ranges encountered by this existing forward decode. The
// pool lock protects registration/cancellation against the bounded memory copy;
// no codec, disk or network I/O occurs while holding it.
func (p *nestedSolid7zPool) capture(e *nestedSolid7zEntry, offset int64, data []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, d := range e.demands {
		next := d.start + d.filled
		end := min(offset+int64(len(data)), d.start+d.length)
		if offset > next || end <= next {
			continue // A request arriving after its prefix was passed must replay.
		}
		copy(d.buffer[d.filled:], data[next-offset:end-offset])
		d.filled += end - next
	}
}

type nestedSolid7zCaptureWriter struct {
	pool     *nestedSolid7zPool
	entry    *nestedSolid7zEntry
	position int64
}

func (w *nestedSolid7zCaptureWriter) Write(data []byte) (int, error) {
	w.pool.capture(w.entry, w.position, data)
	w.position += int64(len(data))
	return len(data), nil
}
