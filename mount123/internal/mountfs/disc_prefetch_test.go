package mountfs

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// TestDiscReadAheadWaitsForCompletePhysicalRanges guards the interaction
// between small ReaderAt calls made by the ISO adapter and Remote's
// prefix-readable range flights. A prefetch must not cancel a larger physical
// range after consuming only its first 64 KiB.
func TestDiscReadAheadWaitsForCompletePhysicalRanges(t *testing.T) {
	image := discImage(t, "large-listing.iso")
	const (
		id           = int64(601)
		videoExtent  = int64(1049 * 2048)
		physicalEdge = int64(3 << 20)
	)
	if physicalEdge <= videoExtent {
		t.Fatal("fixture extent does not reach test boundary")
	}
	foregroundOffset := physicalEdge - videoExtent - 1
	tailRelease := make(chan struct{})
	targetStarted := make(chan struct{}, 16)
	canceledTail := make(chan struct{}, 16)
	var largeTargetRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/601" {
			http.NotFound(w, r)
			return
		}
		start, end := int64(0), int64(len(image)-1)
		if raw := r.Header.Get("Range"); raw != "" {
			if _, err := fmt.Sscanf(raw, "bytes=%d-%d", &start, &end); err != nil {
				http.Error(w, "bad range", http.StatusBadRequest)
				return
			}
		}
		if start < 0 || end < start || end >= int64(len(image)) {
			http.Error(w, "out of range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("ETag", `"disc-prefetch-v1"`)
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(image)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		data := image[start : end+1]
		if start >= physicalEdge {
			select {
			case targetStarted <- struct{}{}:
			default:
			}
		}
		if start >= physicalEdge && int64(len(data)) > 64<<10 {
			largeTargetRequests.Add(1)
			if _, err := w.Write(data[:64<<10]); err != nil {
				return
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			select {
			case <-r.Context().Done():
				select {
				case canceledTail <- struct{}{}:
				default:
				}
				return
			case <-tailRelease:
			}
			_, _ = w.Write(data[64<<10:])
			return
		}
		_, _ = w.Write(data)
	}))
	defer server.Close()
	api := &discFixtureAPI{url: server.URL, files: map[int64]discFixtureFile{id: {id: id, name: "listing.iso", data: image}}, listing: []panapi.File{{ID: id, Name: "listing.iso", Size: int64(len(image)), Version: "disc-prefetch-v1"}}}
	cache, err := storage.NewCache(t.TempDir(), 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	root := NewWithOptions(context.Background(), api, cache, 0, true, Options{})
	fs.NewNodeFS(root, &fs.Options{})
	disc, _ := lookupDiscNode(t, root, "listing.iso")
	video, _ := lookupDiscPath(t, disc, "video.mp4")
	handle, _, errno := video.Open(context.Background(), syscall.O_RDONLY)
	if errno != 0 {
		t.Fatalf("open video: %v", errno)
	}
	defer handle.(fs.FileReleaser).Release(context.Background())
	reader := handle.(fs.FileReader)
	first, errno := reader.Read(context.Background(), make([]byte, 1), foregroundOffset)
	if errno != 0 {
		t.Fatalf("read before prefetch boundary: %v", errno)
	}
	if got, status := first.Bytes(make([]byte, 1)); status != fuse.OK || len(got) != 1 {
		t.Fatalf("first read returned %d bytes with status %v", len(got), status)
	}
	discHandle := handle.(*discHandle)
	if discHandle.readAhead == nil {
		t.Fatal("ISO read-ahead was not enabled")
	}

	select {
	case <-targetStarted:
		// A legacy Remote.ReadAtContext request asks for a 1 MiB block, receives
		// a 64 KiB prefix, then cancels its remaining response. The new prefetch
		// adapter asks for exact completed extents, each at most 64 KiB.
		select {
		case <-canceledTail:
			t.Fatal("background ISO prefetch canceled a range after reading its prefix")
		case <-time.After(150 * time.Millisecond):
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ISO read-ahead did not issue a range beyond the foreground block")
	}
	close(tailRelease)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if discHandle.readAhead.snapshot().CompletedBytes >= 512<<10 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if completed := discHandle.readAhead.snapshot().CompletedBytes; completed < 512<<10 {
		t.Fatalf("background prefetch completed %d bytes, want at least 512 KiB", completed)
	}
	if got := largeTargetRequests.Load(); got != 0 {
		t.Fatalf("prefetch issued %d oversized target ranges, want exact bounded reads", got)
	}
	select {
	case <-canceledTail:
		t.Fatal("completed ISO prefetch canceled a response body")
	default:
	}

	// Stop any later speculative window so the following request measures only
	// whether the completed physical extent was published to the shared cache.
	discHandle.readAhead.Close()
	before := cache.DownloadStats().CompletedRequests
	result, errno := reader.Read(context.Background(), make([]byte, 32), foregroundOffset+1)
	if errno != 0 {
		t.Fatalf("read from prefetched ISO extent: %v", errno)
	}
	got, status := result.Bytes(make([]byte, 32))
	want := discPayload(t, "listing/video.mp4")[foregroundOffset+1 : foregroundOffset+33]
	if status != fuse.OK || !bytes.Equal(got, want) {
		t.Fatalf("read from prefetched extent returned status %v or wrong bytes", status)
	}
	if after := cache.DownloadStats().CompletedRequests; after != before {
		t.Fatalf("cache-hit read completed %d additional HTTP ranges", after-before)
	}
}

func TestISOPrefetchPlanRespectsDirectionAndByteBudget(t *testing.T) {
	parent := &Node{}
	tree := &Tree{opts: Options{PrefetchFiles: 4, PrefetchBytes: 100}}
	entries := make(map[string]*entry)
	for i := 1; i <= 6; i++ {
		name := fmt.Sprintf("%03d.jpg", i)
		entries[name] = &entry{name: name, disc: &discEntry{path: fmt.Sprintf("%03d.bin", i), size: 40}}
	}
	p := &imagePrefetch{tree: tree}
	current := func(name string) *Node {
		return &Node{tree: tree, parent: parent, item: entries[name]}
	}
	names := func(plan []*entry) []string {
		result := make([]string, 0, len(plan))
		for _, item := range plan {
			result = append(result, item.name)
		}
		return result
	}
	if got, want := names(p.plan(current("002.jpg"), entries)), []string{"003.jpg", "004.jpg"}; !equalStrings(got, want) {
		t.Fatalf("initial forward plan = %v, want %v", got, want)
	}
	tree.opts.PrefetchBytes = 90 // two 40-byte neighbors fit; a third does not.
	if got, want := names(p.plan(current("003.jpg"), entries)), []string{"004.jpg", "005.jpg"}; !equalStrings(got, want) {
		t.Fatalf("continued forward plan = %v, want %v", got, want)
	}
	tree.opts.PrefetchBytes = 60
	if got, want := names(p.plan(current("002.jpg"), entries)), []string{"001.jpg"}; !equalStrings(got, want) {
		t.Fatalf("reverse plan = %v, want %v", got, want)
	}
}

func TestISOPrefetchImageFillsSharedCacheForNeighborRead(t *testing.T) {
	image := discImage(t, "large-listing.iso")
	root, _, _ := newDiscFixtureTree(t, []discFixtureFile{{id: 701, name: "listing.iso", data: image}}, Options{PrefetchFiles: 4, DisableReadAhead: true}, nil)
	fs.NewNodeFS(root, &fs.Options{})
	disc, _ := lookupDiscNode(t, root, "listing.iso")
	video, _ := lookupDiscPath(t, disc, "video.mp4")

	// The ISO payload remains named VIDEO.MP4 in the image; only its synthetic
	// outer name participates in neighboring-image planning.
	neighbor := *video.item
	neighbor.name = "002.jpg"
	prefetchImage(context.Background(), &Node{tree: root.tree, item: &neighbor, parent: disc})

	before := root.tree.cache.DownloadStats().CompletedRequests
	handle, _, errno := video.Open(context.Background(), syscall.O_RDONLY)
	if errno != 0 {
		t.Fatalf("open cached ISO payload: %v", errno)
	}
	defer handle.(fs.FileReleaser).Release(context.Background())
	result, errno := handle.(fs.FileReader).Read(context.Background(), make([]byte, 64), 1<<20)
	if errno != 0 {
		t.Fatalf("read cached ISO payload: %v", errno)
	}
	got, status := result.Bytes(make([]byte, 64))
	want := discPayload(t, "listing/video.mp4")[1<<20 : (1<<20)+64]
	if status != fuse.OK || !bytes.Equal(got, want) {
		t.Fatalf("cached ISO read returned status %v or incorrect bytes", status)
	}
	if after := root.tree.cache.DownloadStats().CompletedRequests; after != before {
		t.Fatalf("read after neighboring prefetch downloaded %d more ranges", after-before)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
