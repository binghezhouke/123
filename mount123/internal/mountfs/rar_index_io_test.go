package mountfs

import (
	"context"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/hanwen/go-fuse/v2/fs"
)

type sparseRAR struct {
	size    int64
	headers map[int64][]byte
}

type recordingRAR struct {
	sparseRAR
	offsets []int64
}

func (r *recordingRAR) ReadAt(p []byte, off int64) (int, error) {
	r.offsets = append(r.offsets, off)
	return r.sparseRAR.ReadAt(p, off)
}

func (r sparseRAR) ReadAt(p []byte, off int64) (int, error) {
	if off >= r.size {
		return 0, io.EOF
	}
	n := len(p)
	if int64(n) > r.size-off {
		n = int(r.size - off)
	}
	clear(p[:n])
	for start, data := range r.headers {
		end := start + int64(len(data))
		a, b := off, off+int64(n)
		if a < start {
			a = start
		}
		if b > end {
			b = end
		}
		if a < b {
			copy(p[a-off:b-off], data[a-start:b-start])
		}
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}
func sparseRARFixture() sparseRAR { return rarFixture(8, 4<<20) }

func rarFixture(count int, packed uint32) sparseRAR {
	r := sparseRAR{headers: map[int64][]byte{0: []byte("Rar!\x1a\x07\x00")}}
	header := func(kind byte, flags uint16, data []byte) []byte {
		h := make([]byte, 7+len(data))
		h[2] = kind
		binary.LittleEndian.PutUint16(h[3:], flags)
		binary.LittleEndian.PutUint16(h[5:], uint16(len(h)))
		copy(h[7:], data)
		binary.LittleEndian.PutUint16(h, uint16(crc32.ChecksumIEEE(h[2:])))
		return h
	}
	r.headers[7] = header(0x73, 0, make([]byte, 6))
	off := int64(20)
	for i := 0; i < count; i++ {
		name := []byte(fmt.Sprintf("%04d.jpg", i))
		data := make([]byte, 25+len(name))
		binary.LittleEndian.PutUint32(data, packed)
		binary.LittleEndian.PutUint32(data[4:], packed)
		data[8] = 3
		data[17] = 20
		data[18] = 0x30
		binary.LittleEndian.PutUint16(data[19:], uint16(len(name)))
		binary.LittleEndian.PutUint32(data[21:], 0100644)
		copy(data[25:], name)
		h := header(0x74, 0x8000, data)
		r.headers[off] = h
		off += int64(len(h)) + int64(packed)
	}
	h := header(0x7b, 0, nil)
	r.headers[off] = h
	r.size = off + int64(len(h))
	return r
}

type countResponse struct {
	http.ResponseWriter
	bytes *atomic.Int64
}

func (w countResponse) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.bytes.Add(int64(n))
	return n, err
}
func TestRARIndexDoesNotDownloadMegabytesPerHeader(t *testing.T) {
	data := sparseRARFixture()
	var transferred atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"sparse-rar"`)
		http.ServeContent(countResponse{w, &transferred}, r, "large.rar", time.Unix(1, 0), io.NewSectionReader(data, 0, data.size))
	}))
	defer server.Close()
	cache, err := storage.NewCache(t.TempDir(), 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	ctx := context.Background()
	source, err := storage.NewRemote(ctx, cache, "sparse", data.size, func(context.Context) (string, error) { return server.URL, nil })
	if err != nil {
		t.Fatal(err)
	}
	tree := New(ctx, nil, cache, 0, true).tree
	idx, err := tree.otherIndex(ctx, source, &archiveDescriptor{id: 1, name: "large.rar", size: data.size}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.members) != 8 {
		t.Fatalf("members=%d", len(idx.members))
	}
	if got := transferred.Load(); got > 1<<20 {
		t.Fatalf("listing 8 headers downloaded %d bytes; want <=1 MiB", got)
	}
}

func TestRARListingSkipsSolidCompressedPayload(t *testing.T) {
	data := sparseRARFixture()
	for _, h := range data.headers {
		if len(h) < 7 {
			continue
		}
		switch h[2] {
		case 0x73:
			binary.LittleEndian.PutUint16(h[3:], 8)
		case 0x74:
			h[7+18] = 0x33
		default:
			continue
		}
		binary.LittleEndian.PutUint16(h, uint16(crc32.ChecksumIEEE(h[2:])))
	}
	count := 0
	err := scanArchive(context.Background(), ".rar", data, data.size, nil, func(archiveMember) error { count++; return nil })
	if err != nil || count != 8 {
		t.Fatalf("metadata listing decoded solid payload: count=%d error=%v", count, err)
	}
}

func TestRARScanResumesAtVerifiedMemberBoundary(t *testing.T) {
	data := rarFixture(4, 128<<10)
	recording := &recordingRAR{sparseRAR: data}
	var resume int64
	count := 0
	err := scanArchiveFrom(context.Background(), ".rar", recording, data.size, nil, 0, 0, func(member archiveMember, next int64) error {
		count++
		if count == 2 {
			resume = next
			return syscall.EAGAIN
		}
		return nil
	})
	if err != syscall.EAGAIN || resume <= 0 {
		t.Fatalf("initial scan error=%v resume=%d", err, resume)
	}
	recording.offsets = nil
	var names []string
	err = scanArchiveFrom(context.Background(), ".rar", recording, data.size, nil, resume, 2, func(member archiveMember, _ int64) error {
		names = append(names, member.name)
		return nil
	})
	if err != nil {
		t.Fatalf("resumed scan: %v", err)
	}
	if got, want := strings.Join(names, ","), "0002.jpg,0003.jpg"; got != want {
		t.Fatalf("resumed names=%q, want %q", got, want)
	}
	for _, off := range recording.offsets {
		if off >= 20 && off < resume {
			t.Fatalf("resumed scan reread earlier member header at offset %d before token %d", off, resume)
		}
	}
}

func TestArchiveIndexContinuesAfterForegroundTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tree := New(ctx, nil, nil, 0, true).tree
	started, finish := make(chan struct{}), make(chan struct{})
	var builds atomic.Int32
	build := func(ctx context.Context) (any, error) {
		builds.Add(1)
		close(started)
		select {
		case <-finish:
			return "index", nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	_, err := tree.waitArchiveIndex(context.Background(), "rar", 10*time.Millisecond, build)
	if err != syscall.EAGAIN {
		t.Fatalf("slow foreground got %v", err)
	}
	<-started
	result := make(chan error, 1)
	waiting := &indexWaitContext{Context: context.Background(), entered: make(chan struct{})}
	go func() {
		value, err := tree.waitArchiveIndex(waiting, "rar", time.Second, build)
		if err == nil && value != "index" {
			err = fmt.Errorf("wrong index")
		}
		result <- err
	}()
	// The retry must join the active scan, not start another one.
	<-waiting.entered
	close(finish)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if builds.Load() != 1 {
		t.Fatal("foreground retry restarted scan")
	}
}

// Observe the retry entering its select without relying on scheduler timing.
type indexWaitContext struct {
	context.Context
	once    sync.Once
	entered chan struct{}
}

func (c *indexWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func TestDenseRARIndexUsesLargeRanges(t *testing.T) {
	data := rarFixture(128, 256<<10)
	var requests atomic.Int64
	var largest atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var start, end int64
		fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end)
		if end-start+1 > largest.Load() {
			largest.Store(end - start + 1)
		}
		w.Header().Set("ETag", `"dense-rar"`)
		http.ServeContent(w, r, "dense.rar", time.Unix(1, 0), io.NewSectionReader(data, 0, data.size))
	}))
	defer server.Close()
	cache, err := storage.NewCache(t.TempDir(), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	ctx := context.Background()
	source, err := storage.NewRemote(ctx, cache, "dense", data.size, func(context.Context) (string, error) { return server.URL, nil })
	if err != nil {
		t.Fatal(err)
	}
	tree := New(ctx, nil, cache, 0, true).tree
	index, err := tree.otherIndex(ctx, source, &archiveDescriptor{id: 1, name: "dense.rar", size: data.size}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(index.members) != 128 {
		t.Fatalf("members=%d", len(index.members))
	}
	if requests.Load() > 16 || largest.Load() != 16<<20 {
		t.Fatalf("requests=%d largest=%d", requests.Load(), largest.Load())
	}
	t.Logf("128 members: %d requests, largest Range %d MiB", requests.Load(), largest.Load()>>20)
}

func TestRARIndexUsesFileIdentityAcrossRenameAndCDNChange(t *testing.T) {
	data := rarFixture(4, 128<<10)
	var revision, requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("ETag", fmt.Sprintf(`"cdn-%d"`, revision.Load()))
		http.ServeContent(w, r, "a.rar", time.Unix(1, 0), io.NewSectionReader(data, 0, data.size))
	}))
	defer server.Close()
	cache, err := storage.NewCache(t.TempDir(), 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	ctx := context.Background()
	tree := New(ctx, nil, cache, 0, true).tree
	resolver := func(context.Context) (string, error) { return server.URL, nil }
	first, err := storage.NewRemote(ctx, cache, "cloud-file", data.size, resolver)
	if err != nil {
		t.Fatal(err)
	}
	a := &archiveDescriptor{id: 77, name: "a.rar", version: "content-v1", size: data.size}
	index, err := tree.otherIndex(ctx, first, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	revision.Add(1)
	second, err := storage.NewRemote(ctx, cache, "cloud-file", data.size, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if first.Key() == second.Key() {
		t.Fatal("test did not change remote validator")
	}
	renamed := *a
	renamed.name = "renamed.rar"
	renamed.parentID = 999
	before := requests.Load()
	reused, err := tree.otherIndex(ctx, second, &renamed, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reused != index || requests.Load() != before {
		t.Fatal("rename/CDN change rebuilt an unchanged file index")
	}
	renamed.version = "content-v2"
	updated, err := tree.otherIndex(ctx, second, &renamed, nil)
	if err != nil {
		t.Fatal(err)
	}
	if updated == index {
		t.Fatal("new content version reused old index")
	}
}

func TestRARColdMemberOpenDoesNotRescanEarlierHeaders(t *testing.T) {
	data := sparseRARFixture()
	payload := make([]byte, 4<<20)
	var last int64
	for off, h := range data.headers {
		if len(h) > 32 && h[2] == 0x74 {
			binary.LittleEndian.PutUint32(h[16:], crc32.ChecksumIEEE(payload))
			binary.LittleEndian.PutUint16(h, uint16(crc32.ChecksumIEEE(h[2:])))
			if off > last {
				last = off
			}
		}
	}
	var denyEarlier atomic.Bool
	var forbidden atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var start, end int64
		fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end)
		if denyEarlier.Load() && start >= 1<<20 && start < (last/(1<<20))*(1<<20) {
			forbidden.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("ETag", `"direct-member"`)
		http.ServeContent(w, r, "a.rar", time.Unix(1, 0), io.NewSectionReader(data, 0, data.size))
	}))
	defer server.Close()
	cache, err := storage.NewCache(t.TempDir(), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	api := &archiveAPI{url: server.URL, files: []panapi.File{{ID: 77, Name: "a.rar", Size: data.size, Version: "v1"}}}
	root := New(context.Background(), api, cache, 0, true)
	fs.NewNodeFS(root, &fs.Options{})
	archive := lookup(t, root, "a.rar")
	if _, err := archive.list(context.Background()); err != nil {
		t.Fatal(err)
	}
	denyEarlier.Store(true)
	member := lookup(t, archive, "0007.jpg")
	readOther(t, member, payload)
	if forbidden.Load() != 0 {
		t.Fatal("member open revisited preceding RAR headers")
	}
}
