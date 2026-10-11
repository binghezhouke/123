package mountfs

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestServiceZIPStreamsBeforeLaterRange(t *testing.T) {
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
	defer func() {
		select {
		case <-continueRange:
		default:
			close(continueRange)
		}
	}()
	cache, err := storage.NewCache(t.TempDir(), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api := &serviceNestedAPI{url: server.URL, files: []panapi.File{{ID: 1, Name: "large.zip", Size: int64(archive.Len()), Version: "v1"}}, detail: panapi.File{ID: 1, Name: "large.zip", Size: int64(archive.Len()), Version: "v1"}}
	service, err := NewService(ctx, api, cache, 0, true, Options{StreamMemberThreshold: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	requestCtx, stop := context.WithTimeout(ctx, 2*time.Second)
	defer stop()
	reader, size, err := service.OpenArchiveMember(requestCtx, 1, "large.bin")
	if err != nil {
		t.Fatalf("open should return before the blocked compressed range: %v", err)
	}
	defer reader.(io.Closer).Close()
	if size != int64(len(content)) {
		t.Fatal("wrong size")
	}
	select {
	case <-blocked:
	case <-requestCtx.Done():
		t.Fatal("producer never reached blocked range")
	}
	prefix := make([]byte, 512<<10)
	if _, err := reader.ReadAt(prefix, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(prefix, content[:len(prefix)]) {
		t.Fatal("prefix mismatch")
	}
	close(continueRange)
	tail := make([]byte, 4096)
	if _, err := reader.ReadAt(tail, size-int64(len(tail))); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(tail, content[len(content)-len(tail):]) {
		t.Fatal("tail mismatch")
	}
}
