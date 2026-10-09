package mountfs

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"github.com/bodgit/sevenzip"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/hanwen/go-fuse/v2/fs"
)

// A generated Copy-coded 7z keeps this regression independent of an installed
// archiver and avoids storing a large incompressible binary fixture in git.
func copy7zFixture(payload []byte) []byte {
	number := func(v uint64) []byte {
		for n := 0; n < 8; n++ {
			if v < uint64(1)<<(7+7*n) {
				out := []byte{byte(0xff<<(8-n)) | byte(v>>(8*n))}
				for i := 0; i < n; i++ {
					out = append(out, byte(v>>(8*i)))
				}
				return out
			}
		}
		return binary.LittleEndian.AppendUint64([]byte{255}, v)
	}
	size := number(uint64(len(payload)))
	h := []byte{1, 4, 6, 0, 1, 9}
	h = append(h, size...)
	h = append(h, 0, 7, 11, 1, 0, 1, 1, 0, 12)
	h = append(h, size...)
	h = append(h, 10, 1)
	h = binary.LittleEndian.AppendUint32(h, crc32.ChecksumIEEE(payload))
	h = append(h, 0, 8, 0, 0, 5, 1, 17)
	var name []byte
	name = append(name, 0)
	for _, v := range utf16.Encode([]rune("payload.bin\x00")) {
		name = binary.LittleEndian.AppendUint16(name, v)
	}
	h = append(h, number(uint64(len(name)))...)
	h = append(h, name...)
	h = append(h, 0, 0)
	start := binary.LittleEndian.AppendUint64(nil, uint64(len(payload)))
	start = binary.LittleEndian.AppendUint64(start, uint64(len(h)))
	start = binary.LittleEndian.AppendUint32(start, crc32.ChecksumIEEE(h))
	data := []byte{'7', 'z', 0xbc, 0xaf, 0x27, 0x1c, 0, 4}
	data = binary.LittleEndian.AppendUint32(data, crc32.ChecksumIEEE(start))
	data = append(data, start...)
	data = append(data, payload...)
	return append(data, h...)
}

func TestSolid7zColdReadCoalescesRanges(t *testing.T) {
	payload := bytes.Repeat([]byte("0123456789abcdef"), 2<<20)
	data := copy7zFixture(payload)
	var mu sync.Mutex
	var ranges [][2]int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var a, b int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &a, &b); err != nil {
			t.Error(err)
			http.Error(w, "bad range", 400)
			return
		}
		mu.Lock()
		ranges = append(ranges, [2]int64{a, b})
		mu.Unlock()
		// Fixed RTT reproduces the serial 1 MiB bottleneck without a live CDN.
		time.Sleep(2 * time.Millisecond)
		w.Header().Set("ETag", `"copy7z"`)
		http.ServeContent(w, r, "archive", time.Unix(100, 0), bytes.NewReader(data))
	}))
	defer server.Close()
	cache, err := storage.NewCache(t.TempDir(), 256<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	api := &archiveAPI{url: server.URL, files: []panapi.File{{ID: 77, Name: "plain.7z", Size: int64(len(data)), Version: "v1"}}}
	root := New(context.Background(), api, cache, 0, true)
	fs.NewNodeFS(root, &fs.Options{})
	node := lookup(t, lookup(t, root, "plain.7z"), "payload.bin")
	readOther(t, node, payload)
	mu.Lock()
	defer mu.Unlock()
	var body int
	for _, r := range ranges {
		if r[1]-r[0]+1 > 1<<20 {
			body++
			if r[0] < 32 || r[1] >= 32+int64(len(payload)) {
				t.Fatalf("packed range crossed boundary: %v", r)
			}
		}
	}
	if len(ranges) > 12 || body < 3 {
		t.Fatalf("cold read issued %d HTTP ranges, %d large body ranges; want <=12 with coalesced packed reads", len(ranges), body)
	}
}

func TestPacked7zNextWindowStartsBeforeCurrentTail(t *testing.T) {
	payload := bytes.Repeat([]byte("0123456789abcdef"), 1<<20)
	data := copy7zFixture(payload)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	nextStarted := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var a, b int64
		if _, err := fmt.Sscanf(req.Header.Get("Range"), "bytes=%d-%d", &a, &b); err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("ETag", `"pipeline"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, len(data)))
		w.Header().Set("Content-Length", fmt.Sprint(b-a+1))
		w.WriteHeader(206)
		if a == b {
			_, _ = w.Write(data[a : b+1])
			return
		}
		if a < 32 || b >= 32+int64(len(payload)) {
			t.Errorf("out of packed bounds: %d-%d", a, b)
			return
		}
		if a == 32 {
			if b-a+1 != 8<<20 {
				t.Errorf("first window size %d", b-a+1)
			}
			_, _ = w.Write(data[a : a+4<<20])
			w.(http.Flusher).Flush()
			select {
			case <-req.Context().Done():
				return
			case <-release:
			}
			_, _ = w.Write(data[a+4<<20 : b+1])
			return
		}
		select {
		case nextStarted <- struct{}{}:
		default:
		}
		_, _ = w.Write(data[a : b+1])
	}))
	defer server.Close()
	cache, err := storage.NewCache(t.TempDir(), 256<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	remote, err := storage.NewRemote(ctx, cache, "pipeline", int64(len(data)), func(context.Context) (string, error) { return server.URL, nil })
	if err != nil {
		t.Fatal(err)
	}
	z, err := sevenzip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	ranges, err := z.PackedRanges(0)
	if err != nil {
		t.Fatal(err)
	}
	packed := newPacked7zReader(ctx, contextRemote{ctx: ctx, source: remote}, ranges, cache.Capacity())
	defer packed.Close()
	reader, err := z.OpenStreamWithReader(0, packed)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	done := make(chan error, 1)
	go func() {
		got, e := io.ReadAll(reader)
		if e == nil && !bytes.Equal(got, payload) {
			e = fmt.Errorf("decoded data differs")
		}
		done <- e
	}()
	select {
	case <-nextStarted:
	case <-ctx.Done():
		t.Fatal("next window did not start while current tail was blocked")
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := packed.Finish(); err != nil {
		t.Fatal(err)
	}
}

func TestPacked7zCapacityPressureShrinksWindow(t *testing.T) {
	payload := bytes.Repeat([]byte{'c'}, 2<<20)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"small-cache"`)
		http.ServeContent(w, r, "packed", time.Unix(100, 0), bytes.NewReader(payload))
	}))
	defer server.Close()
	cache, err := storage.NewCache(t.TempDir(), 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	// Leave only 512 KiB available, forcing the normal 1 MiB window to shrink.
	pin, err := cache.Acquire(context.Background(), "unrelated-pinned", (8<<20)-(512<<10), func(ctx context.Context, w io.Writer) error {
		_, e := w.Write(make([]byte, (8<<20)-(512<<10)))
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	defer pin.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	remote, err := storage.NewRemote(ctx, cache, "small-cache", int64(len(payload)), func(context.Context) (string, error) { return server.URL, nil })
	if err != nil {
		t.Fatal(err)
	}
	packed := newPacked7zReader(ctx, contextRemote{ctx: ctx, source: remote}, []sevenzip.PackedRange{{Offset: 0, Size: int64(len(payload))}}, cache.Capacity())
	defer packed.Close()
	got := make([]byte, len(payload))
	if n, err := packed.ReadAt(got, 0); err != nil || n != len(got) || !bytes.Equal(got, payload) {
		t.Fatalf("small cache=%d/%v", n, err)
	}
	if err := packed.Finish(); err != nil {
		t.Fatal(err)
	}
}

func TestVolumeReaderConcurrentNetworkReads(t *testing.T) {
	payload := bytes.Repeat([]byte{'v'}, 4<<20)
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var a, b int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &a, &b); err != nil {
			t.Error(err)
			return
		}
		if a != b {
			started <- struct{}{}
			select {
			case <-r.Context().Done():
				return
			case <-release:
			}
		}
		w.Header().Set("ETag", `"volume"`)
		http.ServeContent(w, r, "volume", time.Unix(100, 0), bytes.NewReader(payload))
	}))
	defer server.Close()
	cache, err := storage.NewCache(t.TempDir(), 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	remote, err := storage.NewRemote(ctx, cache, "volume", int64(len(payload)), func(context.Context) (string, error) { return server.URL, nil })
	if err != nil {
		t.Fatal(err)
	}
	volume := &volumeReaderAt{ctx: ctx, sources: []*storage.Remote{remote}, sizes: []int64{int64(len(payload))}}
	done := make(chan error, 2)
	for _, off := range []int64{0, 1 << 20} {
		go func(off int64) {
			got := make([]byte, 32)
			_, e := volume.ReadAt(got, off)
			if e == nil && !bytes.Equal(got, bytes.Repeat([]byte{'v'}, 32)) {
				e = fmt.Errorf("volume data differs")
			}
			done <- e
		}(off)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("volume mutex serialized independent network reads")
		}
	}
	unblock()
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestPacked7zIndependentInputsStayWithinBounds(t *testing.T) {
	const inputSize = 12 << 20
	payload := bytes.Repeat([]byte("0123456789abcdef"), 2*inputSize/16)
	var mu sync.Mutex
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var a, b int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &a, &b); err != nil {
			t.Error(err)
			return
		}
		if a != b {
			if a/inputSize != b/inputSize || b-a+1 > 4<<20 {
				t.Errorf("input bounds/budget exceeded: %d-%d", a, b)
			}
			mu.Lock()
			count++
			mu.Unlock()
		}
		w.Header().Set("ETag", `"multi-input"`)
		http.ServeContent(w, r, "inputs", time.Unix(100, 0), bytes.NewReader(payload))
	}))
	defer server.Close()
	cache, err := storage.NewCache(t.TempDir(), 128<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	remote, err := storage.NewRemote(ctx, cache, "multi-input", int64(len(payload)), func(context.Context) (string, error) { return server.URL, nil })
	if err != nil {
		t.Fatal(err)
	}
	packed := newPacked7zReader(ctx, contextRemote{ctx: ctx, source: remote}, []sevenzip.PackedRange{{Offset: 0, Size: inputSize}, {Offset: inputSize, Size: inputSize}}, cache.Capacity())
	defer packed.Close()
	// Interleaving packed streams must not be mistaken for a backwards seek on
	// one shared input. BCJ2 equivalence itself is covered in the fork suite.
	for off := int64(0); off < inputSize; off += 1 << 20 {
		for _, start := range []int64{0, inputSize} {
			got := make([]byte, 1<<20)
			if n, err := packed.ReadAt(got, start+off); err != nil || n != len(got) || !bytes.Equal(got, payload[start+off:start+off+int64(len(got))]) {
				t.Fatalf("interleaved read=%d/%v", n, err)
			}
		}
	}
	if err := packed.Finish(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if count != 6 {
		t.Fatalf("independent inputs fragmented into %d requests; want 6", count)
	}
}

func TestVolumePackedWindowSplitsAtPhysicalBoundary(t *testing.T) {
	const size = 1 << 20
	payload := bytes.Repeat([]byte{'d'}, size)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"volumes"`)
		http.ServeContent(w, r, "volume", time.Unix(100, 0), bytes.NewReader(payload))
	}))
	defer server.Close()
	cache, err := storage.NewCache(t.TempDir(), 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sources := make([]*storage.Remote, 2)
	for i := range sources {
		sources[i], err = storage.NewRemote(ctx, cache, fmt.Sprintf("volume-%d", i), size, func(context.Context) (string, error) { return server.URL, nil })
		if err != nil {
			t.Fatal(err)
		}
	}
	volume := &volumeReaderAt{ctx: ctx, sources: sources, sizes: []int64{size, size}}
	window, err := volume.openPackedWindow(ctx, size-32, 64)
	if err != nil {
		t.Fatal(err)
	}
	defer window.Close()
	got := make([]byte, 64)
	if n, err := window.ReadAtContext(ctx, got, 0); err != nil || n != 64 || !bytes.Equal(got, bytes.Repeat([]byte{'d'}, 64)) {
		t.Fatalf("split window=%d/%v", n, err)
	}
	if err := window.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if len(window.(*volumePackedWindow).windows) != 2 {
		t.Fatal("window not split at volume boundary")
	}
}
