package mountfs

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/bodgit/sevenzip"
)

// The clear two-file Copy group keeps group and member checksums independent.
func crcPresence7z(payloads [][]byte, defined []bool, values []uint32, groupDefined bool, groupCRC uint32) []byte {
	return crcPresence7zEncoding(payloads, defined, values, groupDefined, groupCRC, false)
}

// Copy+Delta is a supported graph outside the shared LZMA2 decoder allowlist;
// its nested member reads exercise the private owned-stream fallback.
func crcPresence7zEncoding(payloads [][]byte, defined []bool, values []uint32, groupDefined bool, groupCRC uint32, delta bool) []byte {
	plain := bytes.Join(payloads, nil)
	body := append([]byte(nil), plain...)
	if delta {
		var previous byte
		for i, b := range plain {
			body[i] = b - previous
			previous = b
		}
	}
	h := []byte{1, 4, 6, 0, 1, 9}
	h = append(h, crcPresenceUint(uint64(len(body)))...)
	h = append(h, 0, 7, 11, 1, 0)
	if delta {
		h = append(h, 2, 1, 0, 0x21, 3, 1, 0, 1, 0)
	} else {
		h = append(h, 1, 1, 0)
	}
	h = append(h, 12)
	h = append(h, crcPresenceUint(uint64(len(plain)))...)
	if delta {
		h = append(h, crcPresenceUint(uint64(len(plain)))...)
	}
	h = append(h, 10, 0)
	if groupDefined {
		h = append(h, 0x80)
		h = binary.LittleEndian.AppendUint32(h, groupCRC)
	} else {
		h = append(h, 0)
	}
	h = append(h, 0, 8, 13, 2, 9)
	h = append(h, crcPresenceUint(uint64(len(payloads[0])))...)
	h = append(h, 10, 0)
	bits := byte(0)
	for i, b := range defined {
		if b {
			bits |= 128 >> uint(i)
		}
	}
	h = append(h, bits)
	for i, b := range defined {
		if b {
			h = binary.LittleEndian.AppendUint32(h, values[i])
		}
	}
	h = append(h, 0, 0, 5, 2, 17, 9, 0, 'a', 0, 0, 0, 'b', 0, 0, 0, 0, 0)
	start := make([]byte, 20)
	binary.LittleEndian.PutUint64(start, uint64(len(body)))
	binary.LittleEndian.PutUint64(start[8:], uint64(len(h)))
	binary.LittleEndian.PutUint32(start[16:], crc32.ChecksumIEEE(h))
	data := []byte{'7', 'z', 0xbc, 0xaf, 0x27, 0x1c, 0, 4}
	data = binary.LittleEndian.AppendUint32(data, crc32.ChecksumIEEE(start))
	data = append(data, start...)
	data = append(data, body...)
	return append(data, h...)
}
func crcPresenceUint(v uint64) []byte {
	if v < 128 {
		return []byte{byte(v)}
	}
	p := make([]byte, 9)
	p[0] = 255
	binary.LittleEndian.PutUint64(p[1:], v)
	return p
}

type crcPresenceService struct {
	service  *Service
	cache    *storage.Cache
	cancel   context.CancelFunc
	api      *archiveAPI
	dir      string
	capacity int64
}

func newCRCPresenceService(t *testing.T, name string, body []byte, capacity int64) *crcPresenceService {
	t.Helper()
	f := &crcPresenceService{dir: t.TempDir(), capacity: capacity}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"crc-presence"`)
		http.ServeContent(w, r, "archive", time.Unix(100, 0), bytes.NewReader(body))
	}))
	t.Cleanup(server.Close)
	f.api = &archiveAPI{url: server.URL, files: []panapi.File{{ID: 77, Name: name, Size: int64(len(body)), Version: "immutable-v1"}}}
	f.open(t)
	t.Cleanup(f.close)
	return f
}
func (f *crcPresenceService) open(t *testing.T) {
	t.Helper()
	var err error
	f.cache, err = storage.NewCache(f.dir, f.capacity)
	if err != nil {
		t.Fatal(err)
	}
	var ctx context.Context
	ctx, f.cancel = context.WithCancel(context.Background())
	f.service, err = NewService(ctx, f.api, f.cache, 0, true, Options{DisableReadAhead: true, DisableStreamMembers: true, MaxConcurrentBuilds: 1})
	if err != nil {
		t.Fatal(err)
	}
}
func (f *crcPresenceService) close() {
	if f.cancel != nil {
		f.cancel()
		f.cancel = nil
	}
	if f.service != nil {
		f.service.tree.nestedDeflatePool.Close()
		f.service.tree.nestedSolid7zPool.Close()
		f.service = nil
	}
	if f.cache != nil {
		f.cache.Close()
		f.cache = nil
	}
}
func (f *crcPresenceService) restart(t *testing.T) { f.close(); f.open(t) }
func crcPresenceRead(ctx context.Context, s *Service, path string, want []byte) error {
	r, size, err := s.OpenArchiveMember(ctx, 77, path)
	if err != nil {
		return err
	}
	defer r.(io.Closer).Close()
	got := make([]byte, len(want))
	n, err := r.ReadAt(got, 0)
	if err != nil {
		return err
	}
	if size != int64(len(want)) || n != len(want) || !bytes.Equal(got, want) {
		return fmt.Errorf("member differs: size=%d n=%d bytes=%x", size, n, got)
	}
	return nil
}

func TestService7zCRCPresence(t *testing.T) {
	zero := []byte{0x9d, 0x0a, 0xd9, 0x6d}
	for _, tc := range []struct {
		name    string
		payload []byte
		defined bool
		crc     uint32
		bad     bool
	}{{"absent", []byte("BC"), false, 0, false}, {"declared-zero", zero, true, 0, false}, {"corrupt-declared-zero", []byte{0x9c, 0x0a, 0xd9, 0x6d}, true, 0, true}} {
		for _, parent := range []string{"raw", "store", "deflate"} {
			for _, delta := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/delta=%t", tc.name, parent, delta), func(t *testing.T) {
					payloads := [][]byte{[]byte("A"), tc.payload}
					body := crcPresence7zEncoding(payloads, []bool{true, tc.defined}, []uint32{crc32.ChecksumIEEE(payloads[0]), tc.crc}, true, crc32.ChecksumIEEE(bytes.Join(payloads, nil)), delta)
					name, prefix := "archive.7z", ""
					if parent != "raw" {
						method := uint16(zip.Store)
						if parent == "deflate" {
							method = zip.Deflate
						}
						name, prefix = "outer.zip", "inner.7z"
						body = nestedDeflateLeaseZIP(t, prefix, body, method)
					}
					f := newCRCPresenceService(t, name, body, 32<<20)
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					for attempt := 0; attempt < 2; attempt++ {
						if attempt == 1 {
							f.restart(t)
						}
						rows, err := f.service.ListArchive(ctx, 77, prefix)
						if err != nil || len(rows) != 2 {
							t.Fatalf("listing: %d %v", len(rows), err)
						}
						path := "b"
						if prefix != "" {
							path = prefix + "/b"
						}
						err = crcPresenceRead(ctx, f.service, path, tc.payload)
						if tc.bad {
							if !errors.Is(err, errArchiveIntegrity) && !errors.Is(err, syscall.EACCES) {
								t.Fatalf("declared zero accepted or wrong error: %v", err)
							}
						} else if err != nil {
							t.Fatalf("valid member rejected: %v", err)
						}
					}
				})
			}
		}
	}
}

func TestService7zGroupCRCPresenceAndPublication(t *testing.T) {
	zero := []byte{0x9d, 0x0a, 0xd9, 0x6d}
	for _, tc := range []struct {
		name         string
		body         []byte
		groupDefined bool
		groupCRC     uint32
		bad          bool
	}{
		{"group-only", []byte("ABC"), true, crc32.ChecksumIEEE([]byte("ABC")), false},
		{"no-checksum", []byte("ABC"), false, 0, false},
		{"declared-zero-group", zero, true, 0, false},
		{"corrupt-zero-group", []byte{0x9c, 0x0a, 0xd9, 0x6d}, true, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payloads := [][]byte{tc.body[:1], tc.body[1:]}
			archive := crcPresence7z(payloads, []bool{false, false}, []uint32{0, 0}, tc.groupDefined, tc.groupCRC)
			f := newCRCPresenceService(t, "archive.7z", archive, 32<<20)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			for attempt := 0; attempt < 2; attempt++ {
				if attempt == 1 {
					f.restart(t)
				}
				// Test the early member too: absent member CRC requires the
				// retained group's final validation before member publication.
				for i, want := range payloads {
					err := crcPresenceRead(ctx, f.service, string(rune('a'+i)), want)
					if tc.bad {
						if !errors.Is(err, errArchiveIntegrity) && !errors.Is(err, syscall.EACCES) && !errors.Is(err, syscall.EIO) {
							t.Fatalf("group CRC failure hidden: %v", err)
						}
					} else if err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}

func TestService7zCRCFallbackAndTruncatedPublication(t *testing.T) {
	for _, badGroup := range []bool{false, true} {
		t.Run(fmt.Sprintf("fallback/group-zero=%t", badGroup), func(t *testing.T) {
			first := bytes.Repeat([]byte("prefix"), 3000)
			payloads := [][]byte{first, []byte("BC")}
			groupCRC := crc32.ChecksumIEEE(bytes.Join(payloads, nil))
			if badGroup {
				groupCRC = 0
			}
			archive := crcPresence7z(payloads, []bool{false, false}, []uint32{0, 0}, true, groupCRC)
			// 18KiB group > cap/4: actual top-level full-group admission fallback.
			f := newCRCPresenceService(t, "archive.7z", archive, 32<<10)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			for attempt := 0; attempt < 2; attempt++ {
				if attempt == 1 {
					f.restart(t)
				}
				err := crcPresenceRead(ctx, f.service, "b", payloads[1])
				if badGroup {
					if !errors.Is(err, syscall.EIO) && !errors.Is(err, errArchiveIntegrity) && !errors.Is(err, syscall.EACCES) {
						t.Fatalf("fallback hid group zero CRC: %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestService7zPrivateGroupCRCPresence(t *testing.T) {
	for _, bad := range []bool{false, true} {
		for _, parent := range []uint16{zip.Store, zip.Deflate} {
			t.Run(fmt.Sprintf("bad=%t/parent=%d", bad, parent), func(t *testing.T) {
				body := []byte{0x9d, 0x0a, 0xd9, 0x6d}
				if bad {
					body[0] ^= 1
				}
				payloads := [][]byte{body[:1], body[1:]}
				inner := crcPresence7zEncoding(payloads, []bool{false, false}, []uint32{0, 0}, true, 0, true)
				outer := nestedDeflateLeaseZIP(t, "inner.7z", inner, parent)
				f := newCRCPresenceService(t, "outer.zip", outer, 32<<20)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				rows, err := f.service.ListArchive(ctx, 77, "inner.7z")
				if err != nil || len(rows) != 2 {
					t.Fatalf("list: %d %v", len(rows), err)
				}
				// Delta forces the private owned decoder. The last member reaches
				// group EOF, so an aggregate declared-zero CRC must be enforced.
				err = crcPresenceRead(ctx, f.service, "inner.7z/b", payloads[1])
				if bad {
					if err == nil {
						t.Fatal("owned decoder skipped zero group CRC")
					}
				} else if err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestService7zRejectsLegacyFinalDecodedBlock(t *testing.T) {
	inner := crcPresence7zEncoding([][]byte{[]byte("A"), []byte("BC")}, []bool{false, false}, []uint32{0, 0}, true, 0, true)
	outer := nestedDeflateLeaseZIP(t, "inner.7z", inner, zip.Store)
	f := newCRCPresenceService(t, "outer.zip", outer, 32<<20)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	idx, archive, source, password, err := f.service.outerArchiveIndex(ctx, 77)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(password)
	root, err := f.service.tree.rootArchiveView(ctx, source, archive, idx, password)
	if err != nil {
		t.Fatal(err)
	}
	view, _, m, err := f.service.tree.resolveArchivePath(ctx, root, "inner.7z/b", false)
	if err != nil {
		t.Fatal(err)
	}
	// Reproduce the exact previous public cache contract, rather than using
	// the current reader key. The old decoder could publish these bytes while
	// skipping the declared-zero group CRC; neither member has its own CRC.
	identity, err := json.Marshal(struct {
		Parent, Name, Password string
		Ordinal                int
		Size                   uint64
		CRC                    uint32
		BlockBytes             int64
	}{view.identity, m.name, f.service.tree.passwordTag(view.archive, nil), m.ordinal, m.size, m.crc, f.service.tree.nestedBlockBytes()})
	if err != nil {
		t.Fatal(err)
	}
	key := f.cache.StableDigest("nested-member-range-v4", string(identity)) + ":block:0"
	if err := view.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.cache.Store(ctx, key, []byte("BC")); err != nil {
		t.Fatal(err)
	}
	f.restart(t)
	if err := crcPresenceRead(ctx, f.service, "inner.7z/b", []byte("BC")); err == nil {
		t.Fatal("legacy decoded block bypassed declared-zero group CRC")
	}
	// Migration selects new derived keys; it does not sweep the disk cache.
	h, err := f.cache.OpenCached(key, 2)
	if err != nil || h == nil {
		t.Fatalf("legacy object unexpectedly deleted: %v", err)
	}
	h.Close()
}

type crcPresenceShortReader struct {
	reader           *bytes.Reader
	entered, release chan struct{}
	reads            int
	once             sync.Once
}

func (r *crcPresenceShortReader) Read(p []byte) (int, error) {
	if r.reads > 0 {
		r.once.Do(func() { close(r.entered) })
		<-r.release
	}
	r.reads++
	return r.reader.Read(p)
}

type crcPresenceObservedWriter struct {
	io.Writer
	copied chan struct{}
	once   sync.Once
}

func (w *crcPresenceObservedWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if n > 0 {
		w.once.Do(func() { close(w.copied) })
	}
	return n, err
}

func TestSolid7zTruncatedFillNeverPublishesAbsentMember(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	data := crcPresence7z([][]byte{[]byte("A"), []byte("BC")}, []bool{false, false}, []uint32{0, 0}, true, crc32.ChecksumIEEE([]byte("ABC")))
	zr, err := sevenzip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	cache, err := storage.NewCache(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	source := &crcPresenceShortReader{reader: bytes.NewReader([]byte("AB")), entered: make(chan struct{}), release: release}
	group, err := cache.AcquireGrowingRetained(ctx, ctx, "group", 3, func(fillCtx context.Context, w io.Writer) error {
		return copyValidated7zStream(fillCtx, zr, 0, 3, source, w)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer group.Close()
	select {
	case <-source.entered:
	case <-ctx.Done():
		t.Fatal("producer did not reach the short suffix")
	}
	copied := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		h, e := cache.Acquire(ctx, "member", 1, func(fillCtx context.Context, w io.Writer) error {
			return copySolid7zMember(fillCtx, group, &member{size: 1, sevenStream: &sevenStreamLocation{Stream: 0, Offset: 0, Size: 3}}, &crcPresenceObservedWriter{Writer: w, copied: copied})
		})
		if h != nil {
			h.Close()
		}
		done <- e
	}()
	select {
	case <-copied:
	case <-ctx.Done():
		t.Fatal("early member prefix was not copied")
	}
	unblock()
	select {
	case e := <-done:
		if !errors.Is(e, io.ErrUnexpectedEOF) {
			t.Fatalf("truncated group published member: %v", e)
		}
	case <-ctx.Done():
		t.Fatal("member wait did not finish")
	}
	for _, key := range []string{"group", "member"} {
		if h, e := cache.Open(key); e == nil {
			h.Close()
			t.Fatalf("failed %s published", key)
		}
	}
}
