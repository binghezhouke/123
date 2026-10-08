package mountfs

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type discFixtureFile struct {
	id   int64
	name string
	data []byte
}

type discObservedRange struct {
	id         int64
	start, end int64
}

type discFixtureAPI struct {
	url      string
	files    map[int64]discFixtureFile
	listing  []panapi.File
	urls     atomic.Int32
	listings atomic.Int32
}

func (a *discFixtureAPI) List(_ context.Context, id int64) ([]panapi.File, error) {
	a.listings.Add(1)
	if id != 0 {
		return nil, nil
	}
	return append([]panapi.File(nil), a.listing...), nil
}

func (a *discFixtureAPI) DownloadURL(_ context.Context, id int64) (string, error) {
	a.urls.Add(1)
	return fmt.Sprintf("%s/%d", a.url, id), nil
}

func newDiscFixtureTree(t *testing.T, fixtures []discFixtureFile, opts Options, beforeServe func(*http.Request, int64, int64, int64)) (*Node, *discFixtureAPI, <-chan discObservedRange) {
	t.Helper()
	files := make(map[int64]discFixtureFile, len(fixtures))
	listing := make([]panapi.File, 0, len(fixtures))
	for _, fixture := range fixtures {
		files[fixture.id] = fixture
		listing = append(listing, panapi.File{ID: fixture.id, Name: fixture.name, Size: int64(len(fixture.data)), Version: "disc-test-v1"})
	}
	observed := make(chan discObservedRange, 256)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/"), 10, 64)
		if err != nil {
			http.Error(w, "bad fixture id", http.StatusBadRequest)
			return
		}
		fixture, ok := files[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		start, end := int64(0), int64(len(fixture.data)-1)
		if raw := r.Header.Get("Range"); raw != "" {
			if _, err := fmt.Sscanf(raw, "bytes=%d-%d", &start, &end); err != nil {
				http.Error(w, "bad range", http.StatusBadRequest)
				return
			}
		}
		select {
		case observed <- discObservedRange{id: id, start: start, end: end}:
		default:
		}
		if beforeServe != nil {
			beforeServe(r, id, start, end)
		}
		w.Header().Set("ETag", `"disc-test-v1"`)
		http.ServeContent(w, r, fixture.name, time.Unix(100, 0), bytes.NewReader(fixture.data))
	}))
	t.Cleanup(server.Close)
	api := &discFixtureAPI{url: server.URL, files: files, listing: listing}
	cache, err := storage.NewCache(t.TempDir(), 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	return NewWithOptions(context.Background(), api, cache, 0, true, opts), api, observed
}

func discImage(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "disc", "images", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func discPayload(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "disc", "payload", filepath.FromSlash(path)))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func lookupDiscNode(t *testing.T, parent *Node, name string) (*Node, fuse.EntryOut) {
	t.Helper()
	var out fuse.EntryOut
	inode, errno := parent.Lookup(context.Background(), name, &out)
	if errno != 0 {
		t.Fatalf("Lookup(%q): %v", name, errno)
	}
	return inode.Operations().(*Node), out
}

func lookupDiscPath(t *testing.T, parent *Node, path string) (*Node, fuse.EntryOut) {
	t.Helper()
	var node = parent
	var attr fuse.EntryOut
	for _, part := range strings.Split(path, "/") {
		node, attr = lookupDiscNode(t, node, part)
	}
	return node, attr
}

func readDiscAt(t *testing.T, node *Node, offset int64, size int) []byte {
	t.Helper()
	h, _, errno := node.Open(context.Background(), syscall.O_RDONLY)
	if errno != 0 {
		t.Fatalf("Open(%q): %v", node.item.name, errno)
	}
	defer h.(fs.FileReleaser).Release(context.Background())
	result, errno := h.(fs.FileReader).Read(context.Background(), make([]byte, size), offset)
	if errno != 0 {
		t.Fatalf("Read(%q, %d): %v", node.item.name, offset, errno)
	}
	data, status := result.Bytes(make([]byte, size))
	if status != fuse.OK {
		t.Fatalf("Read(%q, %d) status: %v", node.item.name, offset, status)
	}
	return data
}

func TestISODirectoryFixturesExposeNamesContentsAndReadOnlyAttributes(t *testing.T) {
	cases := []struct {
		name, image string
		files       []string
	}{
		{name: "iso9660", image: "iso9660-base.iso", files: []string{"README.TXT", "CHECK.BIN"}},
		{name: "joliet", image: "joliet-unicode.iso", files: []string{"说明-中文长文件名.txt", "校验数据.bin"}},
		{name: "rockridge", image: "rockridge-long-path.iso", files: []string{"目录层级一/目录层级二/this-is-a-deliberately-long-rock-ridge-filename.txt", "目录层级一/目录层级二/payload-check.bin"}},
		{name: "udf_bridge", image: "udf-bridge.iso", files: []string{"桥接目录/bridge-readme.txt", "桥接目录/udf-check.bin"}},
	}
	payloads := map[string]string{
		"README.TXT":    "base/README.TXT",
		"CHECK.BIN":     "base/CHECK.BIN",
		"说明-中文长文件名.txt": "joliet/说明-中文长文件名.txt",
		"校验数据.bin":      "joliet/校验数据.bin",
		"目录层级一/目录层级二/this-is-a-deliberately-long-rock-ridge-filename.txt": "rockridge/目录层级一/目录层级二/this-is-a-deliberately-long-rock-ridge-filename.txt",
		"目录层级一/目录层级二/payload-check.bin":                                   "rockridge/目录层级一/目录层级二/payload-check.bin",
		"桥接目录/bridge-readme.txt":                                          "bridge/桥接目录/bridge-readme.txt",
		"桥接目录/udf-check.bin":                                              "bridge/桥接目录/udf-check.bin",
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, _, _ := newDiscFixtureTree(t, []discFixtureFile{{id: 100 + int64(i), name: tc.image, data: discImage(t, tc.image)}}, Options{DisableReadAhead: true}, nil)
			fs.NewNodeFS(root, &fs.Options{})
			image, imageAttr := lookupDiscNode(t, root, tc.image)
			if got := imageAttr.Attr.Mode & syscall.S_IFMT; got != syscall.S_IFDIR {
				t.Fatalf("cloud ISO mode = %#o, want directory", got)
			}
			stream, errno := image.Readdir(context.Background())
			if errno != 0 {
				t.Fatalf("Readdir ISO root: %v", errno)
			}
			stream.Close()
			for _, path := range tc.files {
				want := discPayload(t, payloads[path])
				file, attr := lookupDiscPath(t, image, path)
				if got := attr.Attr.Mode & syscall.S_IFMT; got != syscall.S_IFREG {
					t.Fatalf("%s mode = %#o, want regular", path, got)
				}
				if attr.Attr.Size != uint64(len(want)) {
					t.Fatalf("%s size = %d, want %d", path, attr.Attr.Size, len(want))
				}
				for _, off := range []int64{0, int64(len(want) / 3), max(int64(len(want)-11), 0)} {
					size := min(37, len(want)-int(off))
					if got := readDiscAt(t, file, off, size); !bytes.Equal(got, want[off:int(off)+size]) {
						t.Fatalf("%s random read at %d mismatch", path, off)
					}
				}
				if got := readDiscAt(t, file, int64(len(want)-3), 64); !bytes.Equal(got, want[len(want)-3:]) {
					t.Fatalf("%s EOF read mismatch", path)
				}
				if got := readDiscAt(t, file, int64(len(want)), 32); len(got) != 0 {
					t.Fatalf("%s read at EOF returned %d bytes", path, len(got))
				}
				if _, _, errno := file.Open(context.Background(), syscall.O_WRONLY); errno != syscall.EROFS {
					t.Fatalf("%s O_WRONLY errno = %v, want EROFS", path, errno)
				}
				if errno := file.Setattr(context.Background(), nil, &fuse.SetAttrIn{}, &fuse.AttrOut{}); errno != syscall.EROFS {
					t.Fatalf("%s Setattr errno = %v, want EROFS", path, errno)
				}
			}
		})
	}
}

func TestISODirectoryDisableKeepsCloudISOAsFile(t *testing.T) {
	image := discImage(t, "iso9660-base.iso")
	root, _, _ := newDiscFixtureTree(t, []discFixtureFile{{id: 201, name: "archive.ISO", data: image}}, Options{DisableISODirs: true}, nil)
	fs.NewNodeFS(root, &fs.Options{})
	node, attr := lookupDiscNode(t, root, "archive.ISO")
	if got := attr.Attr.Mode & syscall.S_IFMT; got != syscall.S_IFREG {
		t.Fatalf("disabled ISO mode = %#o, want regular file", got)
	}
	if attr.Attr.Size != uint64(len(image)) {
		t.Fatalf("disabled ISO size = %d, want %d", attr.Attr.Size, len(image))
	}
	if got := readDiscAt(t, node, 0, 64); !bytes.Equal(got, image[:64]) {
		t.Fatal("disabled ISO file content mismatch")
	}
}

func TestISODirectoryListingDoesNotFetchVideoExtent(t *testing.T) {
	data := discImage(t, "large-listing.iso")
	root, _, ranges := newDiscFixtureTree(t, []discFixtureFile{{id: 301, name: "listing.iso", data: data}}, Options{DisableReadAhead: true}, nil)
	fs.NewNodeFS(root, &fs.Options{})
	image, _ := lookupDiscNode(t, root, "listing.iso")
	stream, errno := image.Readdir(context.Background())
	if errno != 0 {
		t.Fatalf("Readdir large ISO: %v", errno)
	}
	defer stream.Close()
	names := map[string]bool{}
	for stream.HasNext() {
		de, err := stream.Next()
		if err != 0 {
			t.Fatalf("read large ISO directory stream: %v", err)
		}
		names[de.Name] = true
	}
	if !names["video.mp4"] || !names["000-padding.dat"] {
		t.Fatalf("large ISO names = %v", names)
	}
	const videoExtent = int64(1049 * 2048)
	for {
		select {
		case request := <-ranges:
			if request.id == 301 && request.end >= videoExtent {
				t.Fatalf("listing fetched range %+v, overlapping video extent at %d", request, videoExtent)
			}
		default:
			return
		}
	}
}

func TestISODirectoryConcurrentHandlesReuseSourceAndCache(t *testing.T) {
	image := discImage(t, "large-listing.iso")
	root, api, ranges := newDiscFixtureTree(t, []discFixtureFile{{id: 401, name: "shared.iso", data: image}}, Options{DisableReadAhead: true}, nil)
	fs.NewNodeFS(root, &fs.Options{})
	first, _ := lookupDiscNode(t, root, "shared.iso")
	second, _ := lookupDiscNode(t, root, "shared.iso")
	firstFile, _ := lookupDiscPath(t, first, "video.mp4")
	secondFile, _ := lookupDiscPath(t, second, "video.mp4")
	want := discPayload(t, "listing/video.mp4")
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, node := range []*Node{firstFile, secondFile} {
		wg.Add(1)
		go func(node *Node) {
			defer wg.Done()
			got := readDiscAt(t, node, 9, 31)
			if !bytes.Equal(got, want[9:40]) {
				errs <- fmt.Errorf("concurrent ISO handle returned wrong bytes")
			}
		}(node)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if got := api.urls.Load(); got != 1 {
		t.Fatalf("DownloadURL calls = %d, want shared source resolution once", got)
	}
	const videoExtent = int64(1049 * 2048)
	requestCount := 0
	for {
		select {
		case request := <-ranges:
			if request.id == 401 && request.start <= videoExtent+9 && request.end >= videoExtent+9 {
				requestCount++
			}
		default:
			if requestCount != 1 {
				t.Fatalf("HTTP ranges covering concurrent video read = %d, want one shared fill", requestCount)
			}
			return
		}
	}
}

func TestISODirectoryCancellationDoesNotPoisonRetry(t *testing.T) {
	image := discImage(t, "iso9660-base.iso")
	started := make(chan struct{})
	var block atomic.Bool
	block.Store(true)
	var once sync.Once
	root, _, _ := newDiscFixtureTree(t, []discFixtureFile{{id: 501, name: "cancel.iso", data: image}}, Options{DisableReadAhead: true}, func(r *http.Request, id, _, _ int64) {
		if block.CompareAndSwap(true, false) {
			once.Do(func() { close(started) })
			<-r.Context().Done()
		}
	})
	fs.NewNodeFS(root, &fs.Options{})
	disc, _ := lookupDiscNode(t, root, "cancel.iso")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan syscall.Errno, 1)
	go func() {
		_, errno := disc.Readdir(ctx)
		done <- errno
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("cancel test did not reach the blocked HTTP range")
	}
	cancel()
	select {
	case errno := <-done:
		if errno == 0 {
			t.Fatal("canceled ISO listing unexpectedly succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled ISO listing did not return")
	}
	stream, errno := disc.Readdir(context.Background())
	if errno != 0 {
		t.Fatalf("retry after canceled listing: %v", errno)
	}
	stream.Close()
}

func TestISODirectoryHandleLookupReturnsFrozenEntryAttributes(t *testing.T) {
	root, _, _ := newDiscFixtureTree(t, []discFixtureFile{{id: 601, name: "attrs.iso", data: discImage(t, "rockridge-long-path.iso")}}, Options{DisableReadAhead: true}, nil)
	fs.NewNodeFS(root, &fs.Options{})
	disc, _ := lookupDiscNode(t, root, "attrs.iso")
	if _, err := disc.discDirectory(context.Background()); err != nil {
		t.Fatalf("build ISO directory: %v", err)
	}
	source, _, _, err := disc.discSource(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cacheKey := fmt.Sprintf("disc:%s:.", source.Key())
	h, _, errno := disc.OpendirHandle(context.Background(), syscall.O_RDONLY)
	if errno != 0 {
		t.Fatalf("OpendirHandle: %v", errno)
	}
	defer h.(fs.FileReleasedirer).Releasedir(context.Background(), 0)
	// Simulate replacement of the cached directory snapshot after open. The
	// live handle must continue to enumerate and resolve its original entries.
	replacement := &discDirectory{
		entries: map[string]*entry{"replacement.txt": {name: "replacement.txt", disc: &discEntry{path: "replacement.txt", size: 17}}},
		names:   []string{"replacement.txt"},
	}
	disc.tree.mu.Lock()
	meta := disc.tree.meta[cacheKey]
	if meta == nil {
		disc.tree.mu.Unlock()
		t.Fatal("ISO directory was not cached")
	}
	disc.tree.meta[cacheKey] = &metaItem{key: cacheKey, value: replacement, bytes: meta.bytes, expires: time.Now().Add(time.Hour), seq: meta.seq}
	disc.tree.mu.Unlock()
	reader := h.(fs.FileReaddirenter)
	lookuper := h.(fs.FileLookuper)
	seen := make(map[string]bool)
	for {
		de, errno := reader.Readdirent(context.Background())
		if errno != 0 {
			t.Fatalf("Readdirent: %v", errno)
		}
		if de == nil {
			break
		}
		seen[de.Name] = true
		if de.Name == "replacement.txt" {
			t.Fatal("opened directory handle observed a replacement snapshot")
		}
		var out fuse.EntryOut
		inode, errno := lookuper.Lookup(context.Background(), de.Name, &out)
		if errno != 0 {
			t.Fatalf("directory-handle Lookup(%q): %v", de.Name, errno)
		}
		child := inode.Operations().(*Node)
		if out.Attr.Mode&syscall.S_IFMT == syscall.S_IFDIR {
			if !child.item.directory || de.Mode&syscall.S_IFMT != syscall.S_IFDIR {
				t.Fatalf("directory attrs disagree for %q: dirent=%#o attr=%#o", de.Name, de.Mode, out.Attr.Mode)
			}
			continue
		}
		if child.item.directory || out.Attr.Size != child.item.disc.size || out.Attr.Mode&syscall.S_IFMT != syscall.S_IFREG {
			t.Fatalf("file attrs disagree for %q: size=%d mode=%#o", de.Name, out.Attr.Size, out.Attr.Mode)
		}
	}
	if !seen["目录层级一"] {
		t.Fatalf("frozen directory entries = %v", seen)
	}
}

func TestActualFUSEMountISO(t *testing.T) {
	if os.Getenv("MOUNT123_FUSE_TEST") != "1" {
		t.Skip("set MOUNT123_FUSE_TEST=1 to test actual Linux FUSE mounting")
	}
	root, _, _ := newDiscFixtureTree(t, []discFixtureFile{{id: 701, name: "disc.iso", data: discImage(t, "joliet-unicode.iso")}}, Options{DisableReadAhead: true}, nil)
	mountpoint := t.TempDir()
	server, err := fs.Mount(mountpoint, root, &fs.Options{MountOptions: fuse.MountOptions{Options: []string{"ro"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := server.Unmount(); err != nil {
			t.Error(err)
		}
		server.Wait()
	}()
	path := filepath.Join(mountpoint, "disc.iso", "说明-中文长文件名.txt")
	want := discPayload(t, "joliet/说明-中文长文件名.txt")
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("mounted ISO read = %q, %v", got, err)
	}
	if err := os.WriteFile(path, []byte("mutate"), 0600); err == nil || (!os.IsPermission(err) && err != syscall.EROFS && !strings.Contains(fmt.Sprint(err), "read-only")) {
		t.Fatalf("mounted ISO write not rejected: %v", err)
	}
}
