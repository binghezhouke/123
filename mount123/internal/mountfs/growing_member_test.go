package mountfs

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type growingMemberAPI struct {
	url     string
	archive []byte
}

func (a growingMemberAPI) List(context.Context, int64) ([]panapi.File, error) {
	return []panapi.File{{ID: 1, Name: "large.zip", Size: int64(len(a.archive)), Version: "v1"}}, nil
}
func (a growingMemberAPI) DownloadURL(context.Context, int64) (string, error) {
	return a.url + "/1", nil
}

func TestLargeZIPMemberStreamsBeforeLaterRangeAndWaitsForTail(t *testing.T) {
	content := make([]byte, 12<<20)
	if _, err := rand.Read(content); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	w, err := zw.Create("large.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	blocked := make(chan struct{})
	continueRange := make(chan struct{})
	var blockedOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("Range"), "bytes=1048576-") {
			blockedOnce.Do(func() { close(blocked) })
			select {
			case <-continueRange:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("ETag", `"stable"`)
		http.ServeContent(w, r, "large.zip", time.Unix(1, 0), bytes.NewReader(archive.Bytes()))
	}))
	defer server.Close()
	cache, err := storage.NewCache(t.TempDir(), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := NewWithOptions(ctx, growingMemberAPI{url: server.URL, archive: archive.Bytes()}, cache, 0, true, Options{StreamMemberThreshold: 1 << 20})
	fs.NewNodeFS(root, &fs.Options{})
	archiveNode := lookup(t, root, "large.zip")
	member := lookup(t, archiveNode, "large.bin")
	h, flags, errno := member.Open(ctx, syscall.O_RDONLY)
	if errno != 0 {
		t.Fatalf("Open: %v", errno)
	}
	if flags != fuse.FOPEN_DIRECT_IO {
		t.Fatalf("incomplete member flags = %x, want DIRECT_IO", flags)
	}
	defer h.(fs.FileReleaser).Release(context.Background())
	select {
	case <-blocked:
	case <-time.After(3 * time.Second):
		t.Fatal("decompression did not request the blocked later range")
	}
	first, errno := h.(fs.FileReader).Read(ctx, make([]byte, 512<<10), 0)
	if errno != 0 {
		t.Fatalf("prefix Read: %v", errno)
	}
	got, status := first.Bytes(nil)
	if status != fuse.OK || !bytes.Equal(got, content[:len(got)]) || len(got) == 0 {
		t.Fatal("prefix was not readable while later compressed data was blocked")
	}
	first.Done()
	waitCtx, stop := context.WithTimeout(ctx, 30*time.Millisecond)
	defer stop()
	_, errno = h.(fs.FileReader).Read(waitCtx, make([]byte, 4096), int64(len(content)-4096))
	if errno != syscall.ETIMEDOUT {
		t.Fatalf("tail read errno=%v, want ETIMEDOUT", errno)
	}
	close(continueRange)
	tail, errno := h.(fs.FileReader).Read(ctx, make([]byte, 4096), int64(len(content)-4096))
	if errno != 0 {
		t.Fatalf("tail Read after download: %v", errno)
	}
	got, status = tail.Bytes(nil)
	if status != fuse.OK || !bytes.Equal(got, content[len(content)-4096:]) {
		t.Fatal("tail bytes mismatch after decompression completed")
	}
	tail.Done()
	if errno := h.(fs.FileReleaser).Release(context.Background()); errno != 0 {
		t.Fatalf("close completed member: %v", errno)
	}
	// A second open reuses the validated retained object. Reading backwards
	// after the forward fill must not request another compressed replay.
	reopened, flags, errno := member.Open(context.Background(), syscall.O_RDONLY)
	if errno != 0 {
		t.Fatalf("reopen retained member: %v", errno)
	}
	defer reopened.(fs.FileReleaser).Release(context.Background())
	if flags != fuse.FOPEN_KEEP_CACHE {
		t.Fatalf("reopened retained member flags=%x, want KEEP_CACHE", flags)
	}
	back, errno := reopened.(fs.FileReader).Read(context.Background(), make([]byte, 4096), 0)
	if errno != 0 {
		t.Fatalf("backward read after reopen: %v", errno)
	}
	got, status = back.Bytes(nil)
	if status != fuse.OK || !bytes.Equal(got, content[:4096]) {
		t.Fatal("backward read after retained reopen mismatched content")
	}
	back.Done()
}

func TestSmallZIPMemberKeepsStrictOpenValidation(t *testing.T) {
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "small.bin", Method: zip.Deflate})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(w, strings.Repeat("small", 100))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	root := fixtureArchive(t, archive.Bytes())
	fs.NewNodeFS(root, &fs.Options{})
	node := lookup(t, lookup(t, root, "photos.zip"), "small.bin")
	h, _, errno := node.Open(context.Background(), syscall.O_RDONLY)
	if errno != 0 {
		t.Fatalf("strict small-member open failed: %v", errno)
	}
	h.(fs.FileReleaser).Release(context.Background())
}

func TestLargeZIPCRCErrorIsReturnedAndNotCached(t *testing.T) {
	content := make([]byte, 9<<20)
	if _, err := rand.Read(content); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	w, err := zw.Create("large.bin")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write(content)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	data := archive.Bytes()
	central := bytes.Index(data, []byte{'P', 'K', 1, 2})
	if central < 0 {
		t.Fatal("central directory not found")
	}
	binary.LittleEndian.PutUint32(data[central+16:central+20], 0x12345678)
	root := fixtureArchive(t, data)
	fs.NewNodeFS(root, &fs.Options{})
	node := lookup(t, lookup(t, root, "photos.zip"), "large.bin")
	h, _, errno := node.Open(context.Background(), syscall.O_RDONLY)
	if errno != 0 {
		t.Fatalf("large member Open should return before checksum validation: %v", errno)
	}
	defer h.(fs.FileReleaser).Release(context.Background())
	waitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, errno := h.(fs.FileReader).Read(waitCtx, make([]byte, 4096), int64(len(content)-4096))
	if result != nil {
		result.Done()
	}
	if errno != syscall.EIO {
		t.Fatalf("checksum failure Read errno=%v, want EIO", errno)
	}
	m := node.item.member
	key := node.tree.diskCacheScope() + ":" + node.item.source.Key() + ":.zip:member:" + m.name + fmt.Sprintf(":%08x:%d", m.crc, m.size)
	if _, err := node.tree.cache.Open(key); !os.IsNotExist(err) {
		t.Fatalf("CRC-invalid growing result was published: %v", err)
	}
}
