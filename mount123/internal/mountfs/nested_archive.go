package mountfs

import (
	"context"
	"errors"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/bodgit/sevenzip"
	"io"
	"math"
	"strings"
	"sync"
	"syscall"
)

// nestedArchive is created only after an explicit Lookup of an archive member.
// Copy/Store outer members are exposed through a bounded offset ReaderAt.
// Compressed outer members use the bounded cache fallback; neither path is
// activated by Readdir, only by explicit Lookup.
type nestedArchive struct {
	mu            sync.Mutex
	passwordMu    sync.Mutex
	reader        io.ReaderAt // legacy fixture input; live views use input below
	input         archiveReaderAt
	size          int64
	format        string
	index         *zipIndex
	password      []byte // legacy fixtures only; live passwords are protected
	protected     protectedPassword
	protectedSet  bool
	archive       *archiveDescriptor
	identity      string
	depth         int
	indexBytes    int64
	cachePrefix   string
	handles       []*storage.Handle
	lazyReaders   []*nestedMemberReader
	lazyInputs    []io.Closer
	parent        *nestedArchive
	parentRelease func()
	refs          int
	ownerOnce     sync.Once
	closed        bool
}

// Close releases the creator's reference once. Descendants and open handles
// retain independent references, so releasing a listing cannot close ancestors
// that are still serving a child.
func (n *nestedArchive) Close() error {
	if n == nil {
		return nil
	}
	var err error
	n.ownerOnce.Do(func() { err = n.release() })
	return err
}

func (n *nestedArchive) retain() (func(), error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return nil, storage.ErrClosed
	}
	if n.refs == 0 {
		n.refs = 1
	} // legacy fixture owner
	n.refs++
	var once sync.Once
	return func() { once.Do(func() { _ = n.release() }) }, nil
}

func (n *nestedArchive) release() error {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return nil
	}
	if n.refs > 1 {
		n.refs--
		n.mu.Unlock()
		return nil
	}
	n.refs = 0
	n.closed = true
	handles, readers, inputs, parentRelease := n.handles, n.lazyReaders, n.lazyInputs, n.parentRelease
	n.handles, n.lazyReaders, n.lazyInputs, n.parentRelease = nil, nil, nil, nil
	n.mu.Unlock()
	var first error
	for _, h := range handles {
		if err := h.Close(); err != nil && first == nil {
			first = err
		}
	}
	for _, r := range readers {
		if err := r.Close(); err != nil && first == nil {
			first = err
		}
	}
	for _, input := range inputs {
		if err := input.Close(); err != nil && first == nil {
			first = err
		}
	}
	n.passwordMu.Lock()
	clear(n.password)
	n.password = nil
	clear(n.protected.ciphertext)
	n.protected = protectedPassword{}
	n.passwordMu.Unlock()
	if parentRelease != nil {
		parentRelease()
	}
	return first
}

// nestedAESCopyInput owns only the fork's validated layout/key and the ancestor
// input. Every read binds the current request; the constructor reader is dropped.
type nestedAESCopyInput struct {
	input  archiveReaderAt
	stream *sevenzip.RandomAccessStream
}

func (r *nestedAESCopyInput) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return r.stream.ReadAtWithReader(archiveRequestReader{ctx: ctx, input: r.input}, p, off)
}
func (r *nestedAESCopyInput) Close() error { return r.stream.Close() }

// Record source failures separately from decoder failures: decrypting a wrong
// parent password can make inner headers invalid, but source I/O must retain
// its own error rather than become a password hint.
type nestedSourceReader struct {
	io.ReaderAt
	mu  sync.Mutex
	err error
}

func (r *nestedSourceReader) ReadAt(p []byte, off int64) (int, error) {
	n, err := r.ReaderAt.ReadAt(p, off)
	r.record(err)
	return n, err
}

func (r *nestedSourceReader) record(err error) {
	if err != nil && !errors.Is(err, io.EOF) {
		r.mu.Lock()
		if r.err == nil {
			r.err = err
		}
		r.mu.Unlock()
	}
}
func (r *nestedSourceReader) sourceError() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

type nestedSourceInput struct {
	input    archiveReaderAt
	recorder *nestedSourceReader
}

func (r nestedSourceInput) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	n, err := r.input.ReadAtContext(ctx, p, off)
	r.recorder.record(err)
	return n, err
}

type nestedSourceError struct{ err error }

func (e nestedSourceError) Error() string { return e.err.Error() }
func (e nestedSourceError) Unwrap() error { return e.err }

// Decoder errors over a directly decrypted ancestor need the same password
// hint as the sequential decoder. This walks only the bounded ancestor graph.
func nestedEncryptedInput(input archiveReaderAt) bool {
	switch r := input.(type) {
	case *nestedAESCopyInput:
		return true
	case *nestedRARStoredInput:
		return nestedEncryptedInput(r.input)
	case *nestedVolumeInput:
		r.mu.Lock()
		input := r.input
		r.mu.Unlock()
		return nestedEncryptedInput(input)
	case archiveOffsetInput:
		return nestedEncryptedInput(r.input)
	case archiveConcatInput:
		for _, part := range r.parts {
			if nestedEncryptedInput(part) {
				return true
			}
		}
	}
	return false
}

func nestedCopyMemberReader(ctx context.Context, input archiveReaderAt, size int64, outer *member, ordinal int, password []byte) (archiveReaderAt, io.Closer, bool, error) {
	reader := &nestedSourceReader{ReaderAt: archiveRequestReader{ctx: ctx, input: input}}
	zr, err := sevenzip.NewReaderWithPassword(reader, size, string(password))
	if err != nil {
		if sourceErr := reader.sourceError(); sourceErr != nil {
			return nil, nil, false, sourceErr
		}
		return nil, nil, false, err
	}
	if outer == nil || ordinal != outer.ordinal || ordinal < 0 || ordinal >= len(zr.File) {
		return nil, nil, false, syscall.ESTALE
	}
	f := zr.File[ordinal]
	if f.Name != outer.name || f.UncompressedSize != outer.size || f.CRC32 != outer.crc || f.Checksum().Defined != outer.sevenCRCDefined {
		return nil, nil, false, syscall.ESTALE
	}
	if outer.sevenStream == nil {
		return nil, nil, false, nil // Older indexes can use the generic decoder.
	}
	streams := zr.Streams()
	offset, hasOffset := f.StreamOffset()
	location := outer.sevenStream
	if !hasOffset || f.Stream != location.Stream || offset != location.Offset || location.Stream < 0 || location.Stream >= len(streams) {
		return nil, nil, false, syscall.ESTALE
	}
	stream := streams[location.Stream]
	if stream.UncompressedSize > math.MaxInt64 || location.Size != int64(stream.UncompressedSize) || !location.valid(outer.size) {
		return nil, nil, false, syscall.ESTALE
	}
	if !stream.Stored {
		// This first stage maps only an independent, complete AES+Copy member.
		if offset != 0 || outer.size != stream.UncompressedSize {
			return nil, nil, false, nil
		}
		random, err := zr.OpenRandomAccessStream(location.Stream)
		if errors.Is(err, sevenzip.ErrUnsupportedRandomAccess) {
			return nil, nil, false, nil
		}
		if err != nil {
			return nil, nil, false, err
		}
		if err := ctx.Err(); err != nil {
			_ = random.Close()
			return nil, nil, false, err
		}
		mapped := &nestedAESCopyInput{input: input, stream: random}
		return mapped, mapped, true, nil
	}
	ranges, err := zr.PackedRanges(location.Stream)
	if err != nil {
		return nil, nil, false, err
	}
	if len(ranges) != 1 || ranges[0].Size != int64(stream.UncompressedSize) {
		return nil, nil, false, syscall.EIO
	}
	start := ranges[0].Offset + offset
	if start < ranges[0].Offset || start > ranges[0].Offset+ranges[0].Size || int64(outer.size) > ranges[0].Size-(start-ranges[0].Offset) {
		return nil, nil, false, syscall.EIO
	}
	return archiveOffsetInput{input: input, start: start, size: int64(outer.size)}, nil, true, nil
}

// NestedArchivePolicy keeps recursive archive expansion deliberately bounded.
// The policy is consulted by the explicit nested-entry path; ordinary
// directory enumeration must never call it.
type nestedArchivePolicy struct {
	MaxDepth      int
	MaxBytes      int64
	MaxMembers    int
	MaxIndexBytes int64
}

var defaultNestedArchivePolicy = nestedArchivePolicy{
	MaxDepth: 8, MaxBytes: 64 << 30, MaxMembers: 100000, MaxIndexBytes: 64 << 20,
}

func (p nestedArchivePolicy) allow(depth int, size int64) error {
	if depth > p.MaxDepth {
		return syscall.ELOOP
	}
	if size < 0 || size > p.MaxBytes {
		return syscall.EFBIG
	}
	return nil
}

// boundedReaderAt limits a nested archive view to the member's advertised
// size. It is useful when a decompressed member is backed by a shared cache
// handle: archive parsers cannot accidentally read past the member boundary.
type boundedReaderAt struct {
	r    io.ReaderAt
	size int64
}

func (r boundedReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, syscall.EINVAL
	}
	if off >= r.size {
		return 0, io.EOF
	}
	if int64(len(p)) > r.size-off {
		p = p[:int(r.size-off)]
	}
	n, err := r.r.ReadAt(p, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, err
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// indexNested7z is the narrow seam used by the explicit nested-entry path.
// It performs only header/index work against the supplied ReaderAt; callers
// must obtain that reader from the outer member on an explicit lookup. Keeping
// this operation separate prevents ordinary Readdir from accidentally opening
// an inner archive.
func indexNested7z(ctx context.Context, reader io.ReaderAt, size int64, password []byte, policy nestedArchivePolicy) ([]archiveMember, error) {
	if err := policy.allow(1, size); err != nil {
		return nil, err
	}
	result := make([]archiveMember, 0, 128)
	err := scanArchive(ctx, ".7z", boundedReaderAt{r: reader, size: size}, size, password, func(m archiveMember) error {
		if len(result) >= policy.MaxMembers {
			return syscall.EFBIG
		}
		result = append(result, m)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// indexNestedRAR performs the same bounded header scan for an inner RAR.
// The reader is normally a lazy view over a compressed outer 7z member, so
// scanning headers does not materialize the entire outer archive.
func indexNestedRAR(ctx context.Context, reader io.ReaderAt, size int64, password []byte, policy nestedArchivePolicy) ([]archiveMember, error) {
	if err := policy.allow(1, size); err != nil {
		return nil, err
	}
	result := make([]archiveMember, 0, 128)
	err := scanArchive(ctx, ".rar", boundedReaderAt{r: reader, size: size}, size, password, func(m archiveMember) error {
		if len(result) >= policy.MaxMembers {
			return syscall.EFBIG
		}
		result = append(result, m)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func nestedIndexFromMembers(members []archiveMember, policy nestedArchivePolicy) (*zipIndex, error) {
	idx := &zipIndex{root: &zipDir{dirs: map[string]*zipDir{}, files: map[string]*member{}}, members: map[string]*member{}, bytes: 256, complete: true, changed: make(chan struct{})}
	for _, f := range members {
		name := strings.TrimSuffix(f.name, "/")
		if name == "" {
			return nil, syscall.EIO
		}
		parts := strings.Split(name, "/")
		idx.bytes += int64(256 + len(name))
		if idx.bytes > policy.MaxIndexBytes {
			return nil, syscall.EFBIG
		}
		dir := idx.root
		for i, part := range parts {
			if !validName(part) {
				return nil, syscall.EIO
			}
			if i < len(parts)-1 || f.directory {
				if dir.files[part] != nil {
					return nil, syscall.EIO
				}
				if dir.dirs[part] == nil {
					dir.order = append(dir.order, part)
					dir.dirs[part] = &zipDir{dirs: map[string]*zipDir{}, files: map[string]*member{}}
				}
				dir = dir.dirs[part]
				continue
			}
			if dir.dirs[part] != nil || dir.files[part] != nil {
				return nil, syscall.EIO
			}
			format := ".7z"
			if f.rarLocator != nil {
				format = ".rar"
			}
			m := &member{name: f.name, size: f.size, crc: f.crc, sevenCRCDefined: f.sevenCRCDefined, ordinal: f.ordinal, format: format, encrypted: f.encrypted, rarLocator: f.rarLocator, sevenStream: f.sevenStream}
			if f.zipFile != nil {
				m.format = ".zip"
				m.file = f.zipFile
				m.flags = f.zipFile.Flags
				m.method = f.zipFile.Method
				m.encrypted = f.encrypted
			}
			dir.order = append(dir.order, part)
			dir.files[part] = m
			idx.members[f.name] = m
		}
	}
	close(idx.changed)
	return idx, nil
}
