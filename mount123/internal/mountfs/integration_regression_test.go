package mountfs

import (
	"bytes"
	"context"
	"encoding/binary"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func TestHandleOutlivesOpenRequest(t *testing.T) {
	root, content := fixture(t)
	fs.NewNodeFS(root, &fs.Options{})
	images := lookup(t, lookup(t, root, "photos.zip"), "images")
	for _, name := range []string{"0.txt", "8.txt"} {
		member := lookup(t, images, name)
		ctx, cancel := context.WithCancel(context.Background())
		h, _, errno := member.Open(ctx, syscall.O_RDONLY)
		cancel()
		if errno != 0 {
			t.Fatal(errno)
		}
		result, errno := h.(fs.FileReader).Read(context.Background(), make([]byte, 256), 31)
		if errno != 0 {
			t.Fatalf("%s: expired Open context broke Read: %v", name, errno)
		}
		data, status := result.Bytes(make([]byte, 256))
		if status != fuse.OK || !bytes.Equal(data, content[31:287]) {
			t.Fatal("ZIP read mismatch")
		}
		h.(fs.FileReleaser).Release(context.Background())
	}
	plain := lookup(t, lookup(t, root, "folder"), "plain.txt")
	ctx, cancel := context.WithCancel(context.Background())
	h, _, errno := plain.Open(ctx, 0)
	cancel()
	if errno != 0 {
		t.Fatal(errno)
	}
	defer h.(fs.FileReleaser).Release(context.Background())
	result, errno := h.(fs.FileReader).Read(context.Background(), make([]byte, 11), 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	data, _ := result.Bytes(make([]byte, 11))
	if string(data) != "hello cloud" {
		t.Fatalf("plain = %q", data)
	}
}

func TestZIPSubdirectorySurvivesIndexExpiry(t *testing.T) {
	root, content := fixture(t)
	root.tree.opts.ZIPIndexTTL = 100 * time.Millisecond
	fs.NewNodeFS(root, &fs.Options{})
	archive := lookup(t, root, "photos.zip")
	// Warm the index before constructing a child through the cache-hit path.
	stream, errno := archive.Readdir(context.Background())
	if errno != 0 {
		t.Fatal(errno)
	}
	stream.Close()
	images := lookup(t, archive, "images")
	time.Sleep(150 * time.Millisecond)
	member := lookup(t, images, "8.txt")
	if got := readNode(t, member, 123, 100); !bytes.Equal(got, content[123:223]) {
		t.Fatal("expired ZIP subtree mismatch")
	}
}

func TestArchivePageCacheFlagsOnlyMaterializedMembers(t *testing.T) {
	root, _ := fixture(t)
	fs.NewNodeFS(root, &fs.Options{})
	archive := lookup(t, root, "photos.zip")
	deflate := lookup(t, lookup(t, archive, "images"), "8.txt")
	h, flags, errno := deflate.Open(context.Background(), syscall.O_RDONLY)
	if errno != 0 {
		t.Fatal(errno)
	}
	h.(fs.FileReleaser).Release(context.Background())
	if flags != fuse.FOPEN_KEEP_CACHE {
		t.Fatalf("materialized Deflate open flags=%x, want KEEP_CACHE", flags)
	}
	stored := lookup(t, lookup(t, archive, "images"), "0.txt")
	h, flags, errno = stored.Open(context.Background(), syscall.O_RDONLY)
	if errno != 0 {
		t.Fatal(errno)
	}
	h.(fs.FileReleaser).Release(context.Background())
	if flags != fuse.FOPEN_DIRECT_IO {
		t.Fatalf("Store member open flags=%x, want DIRECT_IO", flags)
	}
	root.tree.opts.DisableArchivePageCache = true
	h, flags, errno = deflate.Open(context.Background(), syscall.O_RDONLY)
	if errno != 0 {
		t.Fatal(errno)
	}
	h.(fs.FileReleaser).Release(context.Background())
	if flags != fuse.FOPEN_DIRECT_IO {
		t.Fatalf("disabled page cache flags=%x, want DIRECT_IO", flags)
	}
}

func TestArchiveImagePageCacheRespectsPrefetchOptions(t *testing.T) {
	imageNode := func(t *testing.T) *Node {
		t.Helper()
		root, _ := fixture(t)
		fs.NewNodeFS(root, &fs.Options{})
		image := lookup(t, lookup(t, lookup(t, root, "photos.zip"), "images"), "8.txt")
		// Keep the archive member identity while giving the mounted entry an
		// image extension, as would happen for a real image member.
		image.item.name = "8.jpg"
		return image
	}

	image := imageNode(t)
	image.tree.prefetch = newImagePrefetch(image.tree)
	defer image.tree.prefetch.interrupt()
	h, flags, errno := image.Open(context.Background(), syscall.O_RDONLY)
	if errno != 0 {
		t.Fatal(errno)
	}
	h.(fs.FileReleaser).Release(context.Background())
	if flags != fuse.FOPEN_DIRECT_IO {
		t.Fatalf("image with prefetch enabled flags=%x, want DIRECT_IO", flags)
	}

	image = imageNode(t)
	if image.tree.prefetch != nil {
		t.Fatal("prefetch unexpectedly enabled")
	}
	h, flags, errno = image.Open(context.Background(), syscall.O_RDONLY)
	if errno != 0 {
		t.Fatal(errno)
	}
	h.(fs.FileReleaser).Release(context.Background())
	if flags != fuse.FOPEN_KEEP_CACHE {
		t.Fatalf("image with prefetch disabled flags=%x, want KEEP_CACHE", flags)
	}
}

func TestDeflateRejectsCorruptCRC(t *testing.T) {
	root, _ := fixture(t)
	archive := root.tree.api.(*fakeAPI).archive
	found := false
	for offset := 0; offset < len(archive); {
		delta := bytes.Index(archive[offset:], []byte{'P', 'K', 1, 2})
		if delta < 0 {
			break
		}
		offset += delta
		if binary.LittleEndian.Uint16(archive[offset+10:]) == 8 {
			archive[offset+16] ^= 1
			found = true
			break
		}
		offset += 4
	}
	if !found {
		t.Fatal("fixture has no deflate central entry")
	}
	fs.NewNodeFS(root, &fs.Options{})
	member := lookup(t, lookup(t, lookup(t, root, "photos.zip"), "images"), "8.txt")
	h, _, errno := member.Open(context.Background(), 0)
	if errno == 0 {
		h.(fs.FileReleaser).Release(context.Background())
		t.Fatal("corrupt Deflate member accepted")
	}
	if errno != syscall.EIO {
		t.Fatalf("expected CRC error EIO, got %v", errno)
	}
}
