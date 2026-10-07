package mountfs

import (
	"archive/zip"
	"bytes"
	"context"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/hanwen/go-fuse/v2/fs"
)

func TestDeflateOpenCancelsBlockedRangeAndCanRetry(t *testing.T) {
	content := make([]byte, 4<<20)
	rand.New(rand.NewSource(7)).Read(content)
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	member, _ := zw.Create("large.bin")
	member.Write(content)
	zw.Close()
	api := &fakeAPI{archive: archive.Bytes()}
	started := make(chan struct{})
	released := make(chan struct{})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("Range"), "bytes=1048576-") && calls.Add(1) == 1 {
			close(started)
			<-r.Context().Done()
			close(released)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		http.ServeContent(w, r, "archive", time.Unix(100, 0), bytes.NewReader(api.archive))
	}))
	defer server.Close()
	api.url = server.URL
	cache, err := storage.NewCache(t.TempDir(), 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	root := New(context.Background(), api, cache, 0, true)
	fs.NewNodeFS(root, &fs.Options{})
	file := lookup(t, lookup(t, root, "photos.zip"), "large.bin")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan syscall.Errno, 1)
	go func() {
		h, _, errno := file.Open(ctx, 0)
		if errno == 0 {
			h.(fs.FileReleaser).Release(context.Background())
		}
		result <- errno
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("no blocked member range")
	}
	cancel()
	select {
	case errno := <-result:
		if errno != syscall.EINTR {
			t.Fatalf("cancel returned %v", errno)
		}
	case <-time.After(time.Second):
		t.Fatal("Deflate Open did not honor cancellation")
	}
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("HTTP request was not canceled")
	}
	if got := readNode(t, file, 12345, 100); !bytes.Equal(got, content[12345:12445]) {
		t.Fatal("retry read mismatch")
	}
}
