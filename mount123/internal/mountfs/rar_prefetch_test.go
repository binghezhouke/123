package mountfs

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/hanwen/go-fuse/v2/fs"
)

func storedRARImages(payload []byte, count int) []byte {
	block := func(kind byte, flags uint16, fields []byte) []byte {
		h := []byte{kind}
		h = binary.LittleEndian.AppendUint16(h, flags)
		h = binary.LittleEndian.AppendUint16(h, uint16(len(fields)+7))
		h = append(h, fields...)
		return append(binary.LittleEndian.AppendUint16(nil, uint16(crc32.ChecksumIEEE(h))), h...)
	}
	out := []byte{'R', 'a', 'r', '!', 0x1a, 7, 0}
	out = append(out, block(0x73, 0, make([]byte, 6))...)
	for i := 1; i <= count; i++ {
		name := fmt.Sprintf("%04d.jpg", i)
		fields := binary.LittleEndian.AppendUint32(nil, uint32(len(payload)))
		fields = binary.LittleEndian.AppendUint32(fields, uint32(len(payload)))
		fields = append(fields, 2) // Windows attributes
		fields = binary.LittleEndian.AppendUint32(fields, crc32.ChecksumIEEE(payload))
		fields = binary.LittleEndian.AppendUint32(fields, 0)
		fields = append(fields, 20, 0x30) // RAR 2.0, Store
		fields = binary.LittleEndian.AppendUint16(fields, uint16(len(name)))
		fields = binary.LittleEndian.AppendUint32(fields, 0x20)
		fields = append(fields, name...)
		out = append(out, block(0x74, 0x8000, fields)...)
		out = append(out, payload...)
	}
	return append(out, block(0x7b, 0, nil)...)
}

// Drive the actual foreground -> Read -> RAR image prefetch -> foreground Open
// chain. A paced response forces background decoder reads to finish before the
// containing 1 MiB Range, unlike tiny fixtures delivered in one HTTP write.
func TestRARForegroundJoinsProgressiveImagePrefetch(t *testing.T) {
	payload := bytes.Repeat([]byte{'r'}, 2<<20)
	data := storedRARImages(payload, 3)
	var armed atomic.Bool
	started := make(chan struct{}, 1)
	cancelled := make(chan struct{}, 8)
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
		w.Header().Set("ETag", `"rar-prefetch"`)
		if armed.Load() && a >= 3<<20 && a < 4<<20 && b-a+1 > 64<<10 {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, len(data)))
			w.Header().Set("Content-Length", fmt.Sprint(b-a+1))
			w.WriteHeader(206)
			_, _ = w.Write(data[a : a+64<<10])
			w.(http.Flusher).Flush()
			select {
			case started <- struct{}{}:
			default:
			}
			select {
			case <-r.Context().Done():
				cancelled <- struct{}{}
				return
			case <-release:
				_, _ = w.Write(data[a+64<<10 : b+1])
				return
			}
		}
		http.ServeContent(w, r, "archive", time.Unix(100, 0), bytes.NewReader(data))
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	cache, err := storage.NewCache(t.TempDir(), 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	api := &archiveAPI{url: server.URL, files: []panapi.File{{ID: 77, Name: "images.rar", Size: int64(len(data)), Version: "v1"}}}
	root := NewWithOptions(ctx, api, cache, 0, true, Options{PrefetchFiles: 9, PrefetchBytes: 16 << 20, PrefetchWorkers: 1})
	fs.NewNodeFS(root, &fs.Options{})
	archive := lookup(t, root, "images.rar")
	// Finish indexing before pacing the member-body request.
	if _, err := archive.list(ctx); err != nil {
		t.Fatal(err)
	}
	first, second := lookup(t, archive, "0001.jpg"), lookup(t, archive, "0002.jpg")
	armed.Store(true)
	h, _, errno := first.Open(ctx, syscall.O_RDONLY)
	if errno != 0 {
		t.Fatal(errno)
	}
	result, errno := h.(fs.FileReader).Read(ctx, make([]byte, 32), 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	result.Done()
	h.(fs.FileReleaser).Release(ctx)
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("RAR prefetch did not reach the paced body")
	}
	select {
	case <-cancelled:
		t.Fatal("RAR decoder cancelled its response after a small background read")
	case <-time.After(50 * time.Millisecond):
	}
	done := make(chan syscall.Errno, 1)
	go func() {
		h, _, errno := second.Open(ctx, syscall.O_RDONLY)
		if errno == 0 {
			result, e := h.(fs.FileReader).Read(ctx, make([]byte, len(payload)), 0)
			errno = e
			if errno == 0 {
				got, status := result.Bytes(nil)
				if status != 0 || !bytes.Equal(got, payload) {
					errno = syscall.EIO
				}
				result.Done()
			}
			h.(fs.FileReleaser).Release(context.Background())
		}
		done <- errno
	}()
	unblock()
	select {
	case errno := <-done:
		if errno != 0 {
			t.Fatal(errno)
		}
	case <-ctx.Done():
		t.Fatal("foreground remained blocked on RAR prefetch")
	}
	root.tree.prefetch.interrupt()
	waitPrefetchIdle(t, root.tree.prefetch)
}
