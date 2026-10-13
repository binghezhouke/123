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
	if err := p.allow(p.MaxDepth+1, 1); err != syscall.ELOOP {
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
	if !innerFile.item.directory || innerFile.item.nested == nil {
		t.Fatal("archive inside concatenated volumes was not expanded")
	}
	file := lookup(t, lookup(t, innerFile, "folder"), "hello.txt")
	want := bytes.Repeat([]byte("mount 7z fixture contents\n"), 200)
	if got := readNode(t, file, 0, len(want)+1); !bytes.Equal(got, want) {
		t.Fatalf("nested volume member content = %q, want %q", got, want)
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

func TestNestedOffsetReaderAtReadsOnlyMappedMember(t *testing.T) {
	base := bytes.Repeat([]byte("x"), 64)
	copy(base[24:32], []byte("inner-7z"))
	r := archiveOffsetInput{input: archivePlainInput{reader: bytes.NewReader(base)}, start: 24, size: 8}
	got := make([]byte, 8)
	if n, err := r.ReadAtContext(context.Background(), got, 0); n != len(got) || err != nil || string(got) != "inner-7z" {
		t.Fatalf("mapped read = %d %v %q", n, err, got)
	}
	if n, err := r.ReadAtContext(context.Background(), make([]byte, 2), 7); n != 1 || err != io.EOF {
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
	member := &member{name: f.Name, size: f.UncompressedSize, crc: f.CRC32, sevenCRCDefined: f.Checksum().Defined, ordinal: 0, sevenStream: &sevenStreamLocation{Stream: f.Stream, Offset: off, Size: int64(streams[f.Stream].UncompressedSize)}}
	mapped, owned, direct, err := nestedCopyMemberReader(context.Background(), archivePlainInput{reader: bytes.NewReader(data)}, int64(len(data)), member, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !direct {
		t.Fatal("fixture outer member was not recognized as a copy stream")
	}
	if owned != nil {
		defer owned.Close()
	}
	got := make([]byte, member.size)
	if _, err := mapped.ReadAtContext(context.Background(), got, 0); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("empty mapped member")
	}
}

func TestNestedCopyMemberReaderPasswordMetadata(t *testing.T) {
	plain := []byte("independent plaintext Copy bytes with a shared password")
	encrypted, err := os.ReadFile("testdata/encrypted/copy.7z")
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct {
		name, password string
		data, want     []byte
		stored         bool
	}{
		{"plaintext_copy_with_shared_password", "shared-password", copy7zFixture(plain), plain, true},
		{"aes_copy_with_correct_password", "mount-test-password", encrypted, bytes.Repeat([]byte("mount 7z fixture contents\n"), 200), false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			zr, err := sevenzip.NewReaderWithPassword(bytes.NewReader(fixture.data), int64(len(fixture.data)), fixture.password)
			if err != nil {
				t.Fatal(err)
			}
			f := zr.File[0]
			off, ok := f.StreamOffset()
			if !ok {
				t.Fatal("fixture member has no stream")
			}
			streams := zr.Streams()
			ranges, err := zr.PackedRanges(f.Stream)
			if err != nil || len(ranges) != 1 || ranges[0].Size != int64(streams[f.Stream].UncompressedSize) {
				t.Fatalf("fixture must have one equal-sized packed range: %+v, %v", ranges, err)
			}
			if streams[f.Stream].Stored != fixture.stored {
				t.Fatalf("fixture Stored = %v, want %v", streams[f.Stream].Stored, fixture.stored)
			}
			r, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := io.ReadAll(r)
			_ = r.Close()
			if err != nil || !bytes.Equal(decoded, fixture.want) {
				t.Fatalf("fixture does not decode to independent golden bytes: %v", err)
			}
			member := &member{name: f.Name, size: f.UncompressedSize, crc: f.CRC32, sevenCRCDefined: f.Checksum().Defined, ordinal: 0,
				sevenStream: &sevenStreamLocation{Stream: f.Stream, Offset: off, Size: int64(streams[f.Stream].UncompressedSize)}}
			mapped, owned, direct, err := nestedCopyMemberReader(context.Background(), archivePlainInput{reader: bytes.NewReader(fixture.data)}, int64(len(fixture.data)), member, 0, []byte(fixture.password))
			if err != nil || !direct {
				t.Fatalf("direct mapping = %v, %v; want supported", direct, err)
			}
			if owned != nil {
				defer owned.Close()
			}
			if direct {
				got := make([]byte, len(fixture.want))
				if n, err := mapped.ReadAtContext(context.Background(), got, 0); n != len(got) || err != nil || !bytes.Equal(got, fixture.want) {
					t.Fatalf("direct plaintext read = %d, %v; golden bytes differ", n, err)
				}
			} else if mapped != nil {
				t.Fatal("AES Copy exposed a raw packed reader")
			}
		})
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
