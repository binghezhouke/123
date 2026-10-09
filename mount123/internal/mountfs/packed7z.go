package mountfs

import (
	"context"
	"errors"
	"io"
	"sync"
	"syscall"

	"github.com/binghezhouke/123/mount123/internal/workqueue"
	"github.com/bodgit/sevenzip"
)

const packed7zWindowBytes int64 = 8 << 20
const packed7zTotalWindowBytes int64 = 16 << 20

type packedWindow interface {
	ReadAtContext(context.Context, []byte, int64) (int, error)
	Wait(context.Context) error
	Close() error
}
type packedSource interface {
	openPackedWindow(context.Context, int64, int64) (packedWindow, error)
}

func (r contextRemote) openPackedWindow(ctx context.Context, off, size int64) (packedWindow, error) {
	return r.source.OpenRangeWindow(ctx, off, size)
}

// A joined window is split only at physical volume boundaries. Every segment
// is registered before the decoder can issue a smaller demand read.
type volumePackedWindow struct {
	windows []packedWindow
	sizes   []int64
}

func (r *volumeReaderAt) openPackedWindow(ctx context.Context, off, size int64) (packedWindow, error) {
	w := &volumePackedWindow{}
	for i, volumeSize := range r.sizes {
		if off >= volumeSize {
			off -= volumeSize
			continue
		}
		amount := min(size, volumeSize-off)
		source, err := r.sourceAt(ctx, i)
		if err != nil {
			w.Close()
			return nil, err
		}
		window, err := source.OpenRangeWindow(ctx, off, amount)
		if err != nil {
			w.Close()
			return nil, err
		}
		w.windows = append(w.windows, window)
		w.sizes = append(w.sizes, amount)
		size -= amount
		off = 0
		if size == 0 {
			return w, nil
		}
	}
	w.Close()
	return nil, io.ErrUnexpectedEOF
}
func (w *volumePackedWindow) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, syscall.EINVAL
	}
	total := 0
	for i, size := range w.sizes {
		if off >= size {
			off -= size
			continue
		}
		amount := min(int64(len(p)), size-off)
		n, err := w.windows[i].ReadAtContext(ctx, p[:amount], off)
		total += n
		p = p[n:]
		if err != nil && !(err == io.EOF && int64(n) == amount) {
			return total, err
		}
		if int64(n) != amount {
			return total, io.ErrUnexpectedEOF
		}
		if len(p) == 0 {
			return total, nil
		}
		off = 0
	}
	return total, io.EOF
}
func (w *volumePackedWindow) Wait(ctx context.Context) error {
	for _, v := range w.windows {
		if err := v.Wait(ctx); err != nil {
			return err
		}
	}
	return nil
}
func (w *volumePackedWindow) Close() error {
	for _, v := range w.windows {
		_ = v.Close()
	}
	return nil
}

type packed7zReader struct {
	ctx    context.Context
	cancel context.CancelFunc
	source packedSource
	inputs []*packed7zInput
}
type packed7zInput struct {
	mu                     sync.Mutex
	start, end, windowSize int64
	current, next          *packed7zWindow
}
type packed7zWindow struct {
	start, end        int64
	handle            packedWindow
	used, speculative bool
}

func newPacked7zReader(ctx context.Context, reader io.ReaderAt, ranges []sevenzip.PackedRange, capacity int64) *packed7zReader {
	source, ok := reader.(packedSource)
	if !ok || len(ranges) == 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	r := &packed7zReader{ctx: ctx, cancel: cancel, source: source}
	// Current + next across ALL inputs use at most 16 MiB per group. Reduce this
	// further for small caches; existing HTTP and staging budgets still arbitrate.
	window := max(int64(1), min(packed7zWindowBytes, packed7zTotalWindowBytes/int64(2*len(ranges)), capacity/int64(8*len(ranges))))
	for _, v := range ranges {
		r.inputs = append(r.inputs, &packed7zInput{start: v.Offset, end: v.Offset + v.Size, windowSize: window})
	}
	return r
}
func (r *packed7zReader) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for _, input := range r.inputs {
		if off >= input.start && off < input.end {
			input.mu.Lock()
			defer input.mu.Unlock()
			want := min(int64(len(p)), input.end-off)
			n, err := r.readInput(input, p[:want], off)
			if err == nil && n < len(p) {
				err = io.EOF
			}
			return n, err
		}
	}
	return 0, io.EOF
}
func (r *packed7zReader) open(input *packed7zInput, start int64, speculative bool) (*packed7zWindow, error) {
	end := min(input.end, start+input.windowSize)
	ctx := r.ctx
	if speculative {
		ctx = workqueue.Background(ctx)
	}
	handle, err := r.source.openPackedWindow(ctx, start, end-start)
	if err != nil {
		return nil, err
	}
	return &packed7zWindow{start: start, end: end, handle: handle, speculative: speculative}, nil
}
func (r *packed7zReader) retire(w *packed7zWindow) error {
	if w == nil {
		return nil
	}
	var err error
	if w.used {
		err = w.handle.Wait(r.ctx)
	}
	_ = w.handle.Close()
	return err
}
func (r *packed7zReader) readInput(input *packed7zInput, p []byte, off int64) (int, error) {
	total := 0
	for len(p) > 0 {
		if err := r.ctx.Err(); err != nil {
			return total, err
		}
		if input.current != nil && (off < input.current.start || off >= input.current.end) {
			if err := r.retire(input.current); err != nil {
				return total, err
			}
			input.current = nil
			if input.next != nil && off >= input.next.start && off < input.next.end {
				input.current, input.next = input.next, nil
			} else if input.next != nil {
				_ = input.next.handle.Close()
				input.next = nil
			}
		}
		if input.current == nil {
			current, err := r.open(input, off, false)
			if err != nil {
				return total, err
			}
			input.current = current
		}
		w := input.current
		amount := min(int64(len(p)), w.end-off)
		n, err := w.handle.ReadAtContext(r.ctx, p[:amount], off-w.start)
		if err != nil && n == 0 && !w.used && (w.speculative || errors.Is(err, syscall.ENOSPC)) {
			// A speculative failure is retried once when demanded. Capacity pressure
			// shrinks the optimization down to demand-sized windows rather than making
			// a previously readable archive fail solely because of an 8 MiB fill.
			_ = w.handle.Close()
			input.current = nil
			if input.next != nil {
				_ = input.next.handle.Close()
				input.next = nil
			}
			if errors.Is(err, syscall.ENOSPC) {
				if input.windowSize <= 64<<10 {
					return total, err
				}
				input.windowSize = max(int64(64<<10), input.windowSize/2)
			}
			continue
		}
		if n > 0 {
			w.used = true
		}
		total += n
		off += int64(n)
		p = p[n:]
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrNoProgress
		}
		if input.next == nil && off >= w.start+(w.end-w.start)/2 && w.end < input.end && r.ctx.Err() == nil {
			// At most one completed-but-unconsumed future window per packed input.
			input.next, _ = r.open(input, w.end, true)
		}
	}
	return total, nil
}

// Finish validates used HTTP responses before the caller publishes the decoded
// group. Unconsumed speculation is cancelled and never affects group validity.
func (r *packed7zReader) Finish() error {
	for _, input := range r.inputs {
		input.mu.Lock()
		err := r.retire(input.current)
		input.current = nil
		if input.next != nil {
			_ = input.next.handle.Close()
			input.next = nil
		}
		input.mu.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}
func (r *packed7zReader) Close() error {
	r.cancel()
	for _, input := range r.inputs {
		input.mu.Lock()
		if input.current != nil {
			_ = input.current.handle.Close()
			input.current = nil
		}
		if input.next != nil {
			_ = input.next.handle.Close()
			input.next = nil
		}
		input.mu.Unlock()
	}
	return nil
}
