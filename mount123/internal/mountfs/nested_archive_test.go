package mountfs

import (
	"bytes"
	"context"
	"github.com/bodgit/sevenzip"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/binghezhouke/123/mount123/internal/storage"
)

func TestIndexNested7zExplicitSeam(t *testing.T) {
	data, err := os.ReadFile("testdata/encrypted/plain.7z")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := indexNested7z(context.Background(), bytes.NewReader(data), int64(len(data)), nil, defaultNestedArchivePolicy)
	if err != nil {
		t.Fatalf("index nested 7z: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("nested index is empty")
	}
}

func TestNestedArchivePolicyBoundsExpansion(t *testing.T) {
	p := defaultNestedArchivePolicy
	if err := p.allow(1, 4<<30); err != nil {
		t.Fatalf("allowed nested archive: %v", err)
	}
	if err := p.allow(2, 1); err != syscall.ELOOP {
		t.Fatalf("depth error = %v", err)
	}
	if err := p.allow(1, p.MaxBytes+1); err != syscall.EFBIG {
		t.Fatalf("size error = %v", err)
	}
	if _, err := nestedIndexFromMembers([]archiveMember{{name: "large-name.jpg", size: 1}}, nestedArchivePolicy{MaxIndexBytes: 1}); err != syscall.EFBIG {
		t.Fatalf("index budget error = %v", err)
	}
}

func TestBoundedReaderAtNeverReadsPastMember(t *testing.T) {
	r := boundedReaderAt{r: bytes.NewReader([]byte("abcdef")), size: 3}
	buf := make([]byte, 5)
	n, err := r.ReadAt(buf, 1)
	if n != 2 || err != nil || string(buf[:2]) != "bc" {
		t.Fatalf("ReadAt = %d %v %q", n, err, buf[:2])
	}
	if n, err := r.ReadAt(buf, 3); n != 0 || err != io.EOF {
		t.Fatalf("at end = %d %v", n, err)
	}
}

func TestNested7zExplicitLookupExpandsMember(t *testing.T) {
	data, err := os.ReadFile("testdata/encrypted/nested-outer.7z")
	if err != nil {
		t.Fatal(err)
	}
	root, _ := otherArchiveFixture(t, "outer.7z", data, nil, false)
	outer := lookup(t, root, "outer.7z")
	// Readdir of the outer archive leaves inner archives as regular files.
	entries, err := outer.list(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if entries["inner.7z"] == nil || entries["inner.7z"].directory {
		t.Fatal("inner archive was expanded during listing")
	}
	inner := lookup(t, outer, "inner.7z")
	if !inner.item.directory || inner.item.nested == nil {
		t.Fatal("explicit lookup did not expand inner archive")
	}
	file := lookup(t, inner, "folder")
	file = lookup(t, file, "hello.txt")
	got := readNode(t, file, 0, 100)
	if string(got[:len("mount 7z fixture contents\n")]) != "mount 7z fixture contents\n" {
		t.Fatalf("nested member content = %q", got)
	}
}

func TestNested7zReusesSharedPassword(t *testing.T) {
	data, err := os.ReadFile("testdata/encrypted/nested-encrypted-outer.7z")
	if err != nil {
		t.Fatal(err)
	}
	root, _ := otherArchiveFixture(t, "outer.7z", data, []byte("secret"), false)
	inner := lookup(t, lookup(t, root, "outer.7z"), "inner.7z")
	if !inner.item.directory || inner.item.nested == nil {
		t.Fatal("encrypted inner archive was not expanded")
	}
	payload := lookup(t, inner, "payload")
	got := readNode(t, payload, 0, 218)
	if len(got) == 0 {
		t.Fatal("encrypted nested member is empty")
	}
}

func TestNested7zVolumesConcatenateOnExplicitLookup(t *testing.T) {
	data, err := os.ReadFile("testdata/encrypted/nested-vol-outer.7z")
	if err != nil {
		t.Fatal(err)
	}
	root, _ := otherArchiveFixture(t, "outer.7z", data, nil, false)
	inner := lookup(t, lookup(t, root, "outer.7z"), "inner.7z.001")
	if !inner.item.directory || inner.item.nested == nil {
		t.Fatal("volume member was not expanded")
	}
	innerFile := lookup(t, inner, "plain.7z")
	if !innerFile.item.directory {
		// The fixture intentionally keeps one extra archive layer to validate
		// that the volume reader returns the complete inner bytes.
		if len(readNode(t, innerFile, 0, 64)) == 0 {
			t.Fatal("volume nested member is empty")
		}
		return
	}
	if got := lookup(t, lookup(t, innerFile, "folder"), "hello.txt"); len(readNode(t, got, 0, 64)) == 0 {
		t.Fatal("volume nested member is empty")
	}
}

func TestSplit7zVolumeIsDirectoryEntryBeforeExpansion(t *testing.T) {
	root := &zipDir{dirs: map[string]*zipDir{}, files: map[string]*member{
		"inner.7z.001": {name: "inner.7z.001", size: 1},
		"inner.7z.002": {name: "inner.7z.002", size: 1},
	}}
	a := &archiveDescriptor{name: "outer.7z", size: 2}
	entries := (&zipIndex{root: root}).children("", nil, 2, a)
	if !entries["inner.7z.001"].directory {
		t.Fatal(".001 split volume must be exposed as an expandable directory")
	}
	if entries["inner.7z.002"].directory {
		t.Fatal("non-entry split volume must remain a regular file")
	}
}

func TestNestedSplitVolumeCycleIsHidden(t *testing.T) {
	if !repeatedSplitVolume("NO.002.7z.001", "NO.002.7z.001") {
		t.Fatal("same split volume path must be treated as a cycle")
	}
	if repeatedSplitVolume("NO.002.7z.001", "NO.002.7z.002") {
		t.Fatal("different split volume must remain visible")
	}
}

func TestNestedSplitVolumeChainCollapsesToLeaf(t *testing.T) {
	leaf := &zipDir{dirs: map[string]*zipDir{}, files: map[string]*member{"001.jpg": {name: "001.jpg"}}}
	second := &zipDir{dirs: map[string]*zipDir{"inner.7z.001": leaf}, files: map[string]*member{}}
	root := &zipDir{dirs: map[string]*zipDir{"inner.7z.001": second}, files: map[string]*member{}}
	got := nestedSplitLeafPath(root, "", "inner.7z.001")
	if got != "inner.7z.001/inner.7z.001" {
		t.Fatalf("leaf path = %q", got)
	}
}

func TestNestedOffsetReaderAtReadsOnlyMappedMember(t *testing.T) {
	base := bytes.Repeat([]byte("x"), 64)
	copy(base[24:32], []byte("inner-7z"))
	r := nestedOffsetReader{r: bytes.NewReader(base), start: 24, size: 8}
	got := make([]byte, 8)
	if n, err := r.ReadAt(got, 0); n != len(got) || err != nil || string(got) != "inner-7z" {
		t.Fatalf("mapped read = %d %v %q", n, err, got)
	}
	if n, err := r.ReadAt(make([]byte, 2), 7); n != 1 || err != io.EOF {
		t.Fatalf("bounded mapped read = %d %v", n, err)
	}
}

func TestNestedCopyMemberReaderUsesPackedRange(t *testing.T) {
	data, err := os.ReadFile("testdata/encrypted/nested-outer.7z")
	if err != nil {
		t.Fatal(err)
	}
	zr, err := sevenzip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if len(zr.File) == 0 {
		t.Fatal("empty outer archive")
	}
	f := zr.File[0]
	off, ok := f.StreamOffset()
	if !ok {
		t.Fatal("fixture member has no stream")
	}
	streams := zr.Streams()
	member := &member{name: f.Name, size: f.UncompressedSize, crc: f.CRC32, ordinal: 0, sevenStream: &sevenStreamLocation{Stream: f.Stream, Offset: off, Size: int64(streams[f.Stream].UncompressedSize)}}
	mapped, direct, err := nestedCopyMemberReader(bytes.NewReader(data), int64(len(data)), member, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !direct {
		t.Fatal("fixture outer member was not recognized as a copy stream")
	}
	got := make([]byte, member.size)
	if _, err := mapped.ReadAt(got, 0); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("empty mapped member")
	}
}

func TestNestedArchiveCloseReleasesFallbackPins(t *testing.T) {
	dir := t.TempDir()
	cache, err := storage.NewCache(filepath.Join(dir, "cache"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	key := "nested-fallback-member"
	h, err := cache.Acquire(context.Background(), key, 4, func(_ context.Context, w io.Writer) error {
		_, err := io.WriteString(w, "data")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	na := &nestedArchive{handles: []*storage.Handle{h}, password: []byte("secret")}
	if got := cache.Stats().PinnedBytes; got != 4 {
		t.Fatalf("pinned bytes before close = %d, want 4", got)
	}
	if err := na.Close(); err != nil {
		t.Fatal(err)
	}
	if got := cache.Stats().PinnedBytes; got != 0 {
		t.Fatalf("pinned bytes after close = %d, want 0", got)
	}
	if na.password != nil || len(na.handles) != 0 {
		t.Fatal("nested archive retained secrets or handles after close")
	}
	if err := na.Close(); err != nil {
		t.Fatal(err)
	}
}
