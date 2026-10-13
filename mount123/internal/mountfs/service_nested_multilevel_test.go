package mountfs

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/bodgit/sevenzip"
)

// multilevelFixture writes small real archives from independent member bytes.
// RAR uses the documented stored RAR3 layout, with valid header and data CRCs.
func multilevelFixture(t *testing.T, format string, entries map[string][]byte) []byte {
	t.Helper()
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	switch strings.TrimPrefix(format, ".") {
	case "zip":
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		for _, name := range names {
			w, err := zw.Create(name)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write(entries[name]); err != nil {
				t.Fatal(err)
			}
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	case "7z":
		sevenzip, err := exec.LookPath("7z")
		if err != nil {
			t.Skip("7z fixture generator unavailable")
		}
		dir := t.TempDir()
		for _, name := range names {
			file := filepath.Join(dir, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, entries[name], 0600); err != nil {
				t.Fatal(err)
			}
		}
		output := filepath.Join(t.TempDir(), "fixture.7z")
		args := append([]string{"a", "-t7z", "-m0=lzma2", "-mx=1", "-ms=on", "-mqs=off", output}, names...)
		cmd := exec.Command(sevenzip, args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("generate 7z: %v: %s", err, out)
		}
		data, err := os.ReadFile(output)
		if err != nil {
			t.Fatal(err)
		}
		return data
	case "rar":
		block := func(kind byte, flags uint16, fields []byte) []byte {
			h := []byte{kind}
			h = binary.LittleEndian.AppendUint16(h, flags)
			h = binary.LittleEndian.AppendUint16(h, uint16(len(fields)+7))
			h = append(h, fields...)
			return append(binary.LittleEndian.AppendUint16(nil, uint16(crc32.ChecksumIEEE(h))), h...)
		}
		out := []byte("Rar!\x1a\x07\x00")
		out = append(out, block(0x73, 0, make([]byte, 6))...)
		for _, name := range names {
			payload := entries[name]
			fields := binary.LittleEndian.AppendUint32(nil, uint32(len(payload)))
			fields = binary.LittleEndian.AppendUint32(fields, uint32(len(payload)))
			fields = append(fields, 2) // Windows file attributes.
			fields = binary.LittleEndian.AppendUint32(fields, crc32.ChecksumIEEE(payload))
			fields = binary.LittleEndian.AppendUint32(fields, 0)
			fields = append(fields, 20, 0x30) // RAR 2.0 decoder, Store.
			fields = binary.LittleEndian.AppendUint16(fields, uint16(len(name)))
			fields = binary.LittleEndian.AppendUint32(fields, 0x20)
			fields = append(fields, name...)
			out = append(out, block(0x74, 0x8000, fields)...)
			out = append(out, payload...)
		}
		return append(out, block(0x7b, 0, nil)...)
	default:
		t.Fatalf("unsupported fixture format %q", format)
		return nil
	}
}

func multilevelService(t *testing.T, format string, data []byte, opts Options, capacity int64) *Service {
	t.Helper()
	name := "outer." + strings.TrimPrefix(format, ".")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"multilevel-fixture"`)
		http.ServeContent(w, r, name, time.Unix(100, 0), bytes.NewReader(data))
	}))
	t.Cleanup(server.Close)
	file := panapi.File{ID: 77, Name: name, Size: int64(len(data)), Version: "v1"}
	api := &serviceNestedAPI{url: server.URL, files: []panapi.File{file}, detail: file}
	cache, err := storage.NewCache(t.TempDir(), capacity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s, err := NewService(ctx, api, cache, 0, true, opts)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func multilevelRead(t *testing.T, service *Service, memberPath string, want []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, size, err := service.OpenArchiveMember(ctx, 77, memberPath)
	if err != nil {
		t.Fatalf("open %s: %v", memberPath, err)
	}
	if closer, ok := r.(io.Closer); ok {
		defer closer.Close()
	}
	if size != int64(len(want)) {
		t.Fatalf("%s size = %d, want %d", memberPath, size, len(want))
	}
	got := make([]byte, len(want))
	if n, err := r.ReadAt(got, 0); n != len(got) || (err != nil && err != io.EOF) || !bytes.Equal(got, want) {
		t.Fatalf("read %s = %d, %v, %q; want %q", memberPath, n, err, got, want)
	}
}

func multilevelNames(t *testing.T, service *Service, memberPath string, want ...string) {
	t.Helper()
	rows, err := service.ListArchive(context.Background(), 77, memberPath)
	if err != nil {
		t.Fatalf("list %s: %v", memberPath, err)
	}
	got := make([]string, 0, len(rows))
	for _, row := range rows {
		got = append(got, row.Name)
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("list %s = %q, want %q", memberPath, got, want)
	}
}

func TestServiceMultilevel7z7zRAR(t *testing.T) {
	want := []byte("three archive layers: 7z -> 7z -> RAR -> file\n")
	rar := multilevelFixture(t, "rar", map[string][]byte{"folder/leaf.txt": want, "second.txt": []byte("second independent member")})
	middle := multilevelFixture(t, "7z", map[string][]byte{"middle.rar": rar})
	outer := multilevelFixture(t, "7z", map[string][]byte{"inner.7z": middle})
	s := newServiceNested(t, outer)
	multilevelNames(t, s, "inner.7z/middle.rar", "folder", "second.txt")
	multilevelNames(t, s, "inner.7z/middle.rar/folder", "leaf.txt")
	multilevelRead(t, s, "inner.7z/middle.rar/folder/leaf.txt", want)
	multilevelRead(t, s, "inner.7z/middle.rar", rar)
	first, err := s.ListArchivePage(context.Background(), 77, "inner.7z/middle.rar", "", 1)
	if err != nil || len(first.Items) != 1 || first.Items[0].Name != "folder" || first.NextCursor == "" {
		t.Fatalf("first nested page = %+v, %v", first, err)
	}
	second, err := s.ListArchivePage(context.Background(), 77, "inner.7z/middle.rar", first.NextCursor, 1)
	if err != nil || len(second.Items) != 1 || second.Items[0].Name != "second.txt" || !second.Complete || second.NextCursor != "" {
		t.Fatalf("second nested page = %+v, %v", second, err)
	}
	if _, _, err := s.OpenArchiveMember(context.Background(), 77, "inner.7z/middle.rar/folder/missing.txt"); !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("missing deeply nested member = %v, want ENOENT", err)
	}
	if _, err := s.ListArchive(context.Background(), 77, "inner.7z/middle.rar/missing"); !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("missing deeply nested directory = %v, want ENOENT", err)
	}
}

func TestServiceMultilevelFormatMatrix(t *testing.T) {
	for _, outerFormat := range []string{"zip", "7z", "rar"} {
		for _, innerFormat := range []string{"zip", "7z", "rar"} {
			t.Run(outerFormat+"_"+innerFormat, func(t *testing.T) {
				want := []byte("independent payload for " + outerFormat + "/" + innerFormat)
				inner := multilevelFixture(t, innerFormat, map[string][]byte{"docs/leaf.txt": want})
				// The inner archive is deliberately not the first path component.
				member := "ordinary/folder/inner." + innerFormat
				outer := multilevelFixture(t, outerFormat, map[string][]byte{member: inner, "normal.zip/readme.txt": []byte("directory ending in .zip")})
				s := multilevelService(t, outerFormat, outer, Options{}, 16<<20)
				multilevelNames(t, s, member+"/docs", "leaf.txt")
				multilevelRead(t, s, member+"/docs/leaf.txt", want)
				multilevelRead(t, s, member, inner)
				multilevelNames(t, s, "normal.zip", "readme.txt")
				multilevelRead(t, s, "normal.zip/readme.txt", []byte("directory ending in .zip"))
				page, err := s.ListArchivePage(context.Background(), 77, member+"/docs", "", 1)
				if err != nil || len(page.Items) != 1 || page.Items[0].Name != "leaf.txt" || !page.Complete || page.NextCursor != "" {
					t.Fatalf("nested page = %+v, %v", page, err)
				}
			})
		}
	}
}

func TestServiceMultilevelSameNameChainsDoNotAlias(t *testing.T) {
	wantA, wantB := []byte("left same-name chain"), []byte("right same-name chain")
	chain := func(want []byte) []byte {
		leaf := multilevelFixture(t, "zip", map[string][]byte{"leaf.txt": want})
		return multilevelFixture(t, "zip", map[string][]byte{"same.zip": leaf})
	}
	outer := multilevelFixture(t, "zip", map[string][]byte{"left/same.zip": chain(wantA), "right/same.zip": chain(wantB)})
	s := multilevelService(t, "zip", outer, Options{}, 16<<20)
	for i := 0; i < 2; i++ {
		multilevelRead(t, s, "left/same.zip/same.zip/leaf.txt", wantA)
		multilevelRead(t, s, "right/same.zip/same.zip/leaf.txt", wantB)
	}
	multilevelNames(t, s, "left/same.zip/same.zip", "leaf.txt")
	multilevelNames(t, s, "right/same.zip/same.zip", "leaf.txt")
}

func TestServiceMultilevelCompressedRangeWithOnePermit(t *testing.T) {
	// The RAR and the middle archive both exceed the cache. An incompressible
	// solid prefix forces the child decoder to read an uncached ancestor block
	// while holding the only decode permit, rather than only hitting cached
	// ancestor headers. Reading the RAR's header and tail must use ranges.
	padding := bytes.Repeat([]byte("compressible padding"), (6<<20)/20)
	rar := multilevelFixture(t, "rar", map[string][]byte{
		"head.txt":    []byte("head payload"),
		"padding.bin": padding,
		"tail.txt":    []byte("tail payload"),
	})
	prefix := make([]byte, 5<<20)
	if _, err := rand.New(rand.NewSource(77)).Read(prefix); err != nil {
		t.Fatal(err)
	}
	middle := multilevelFixture(t, "7z", map[string][]byte{"aaa.bin": prefix, "deep.rar": rar})
	outer := multilevelFixture(t, "7z", map[string][]byte{"inner.7z": middle})
	const capacity = 4 << 20
	if len(rar) <= capacity || len(middle) <= capacity {
		t.Fatal("both inner archives must exceed cache capacity")
	}
	s := multilevelService(t, "7z", outer, Options{MaxConcurrentBuilds: 1, DisableReadAhead: true}, capacity)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, size, err := s.OpenArchiveMember(ctx, 77, "inner.7z/deep.rar")
	if err != nil {
		t.Fatalf("open raw archive with one decode permit: %v", err)
	}
	if closer, ok := r.(io.Closer); ok {
		defer closer.Close()
	}
	if size != int64(len(rar)) {
		t.Fatalf("raw archive size = %d, want %d", size, len(rar))
	}
	for _, off := range []int64{0, size - 64, 0} {
		got := make([]byte, 64)
		if n, err := r.ReadAt(got, off); n != len(got) || err != nil || !bytes.Equal(got, rar[off:off+64]) {
			t.Fatalf("raw archive range at %d = %d, %v", off, n, err)
		}
	}
	multilevelRead(t, s, "inner.7z/deep.rar/head.txt", []byte("head payload"))
	multilevelRead(t, s, "inner.7z/deep.rar/tail.txt", []byte("tail payload"))
	leaf, leafSize, err := s.OpenArchiveMember(ctx, 77, "inner.7z/deep.rar/padding.bin")
	if err != nil || leafSize != int64(len(padding)) {
		t.Fatalf("open leaf larger than cache = %d, %v", leafSize, err)
	}
	if closer, ok := leaf.(io.Closer); ok {
		defer closer.Close()
	}
	for _, off := range []int64{0, leafSize - 64, 0} {
		got := make([]byte, 64)
		if n, err := leaf.ReadAt(got, off); n != len(got) || err != nil || !bytes.Equal(got, padding[off:off+64]) {
			t.Fatalf("large leaf range at %d = %d, %v", off, n, err)
		}
	}
	stats := s.tree.cache.Stats()
	if stats.UsedBytes > capacity || stats.ENOSPC != 0 {
		t.Fatalf("range reads attempted full archive caching: %+v", stats)
	}
}

func TestServiceMultilevelArchiveDepth(t *testing.T) {
	t.Run("one nested archive", func(t *testing.T) {
		want := []byte("read within the allowed first nested layer")
		deep := multilevelFixture(t, "zip", map[string][]byte{"leaf.txt": want})
		inner := multilevelFixture(t, "zip", map[string][]byte{"next.zip": deep, "allowed.txt": want})
		outer := multilevelFixture(t, "zip", map[string][]byte{"one/two/inner.zip": inner})
		s := multilevelService(t, "zip", outer, Options{MaxArchiveDepth: 1}, 16<<20)
		multilevelNames(t, s, "one/two/inner.zip", "allowed.txt", "next.zip")
		multilevelRead(t, s, "one/two/inner.zip/allowed.txt", want)
		// Reading the next archive as a file does not enter a second layer.
		multilevelRead(t, s, "one/two/inner.zip/next.zip", deep)
		path := "one/two/inner.zip/next.zip"
		if _, err := s.ListArchive(context.Background(), 77, path); !errors.Is(err, syscall.ELOOP) {
			t.Fatalf("second nested archive listing = %v, want ELOOP", err)
		}
		if _, err := s.ListArchivePage(context.Background(), 77, path, "", 1); !errors.Is(err, syscall.ELOOP) {
			t.Fatalf("second nested archive page = %v, want ELOOP", err)
		}
		if _, _, err := s.OpenArchiveMember(context.Background(), 77, path+"/leaf.txt"); !errors.Is(err, syscall.ELOOP) {
			t.Fatalf("second nested archive read = %v, want ELOOP", err)
		}
	})
	t.Run("default eight nested archives", func(t *testing.T) {
		want := []byte("eighth nested layer is readable")
		for _, depth := range []int{8, 9} {
			data := multilevelFixture(t, "zip", map[string][]byte{"leaf.txt": want})
			path := "leaf.txt"
			for i := 0; i < depth; i++ {
				data = multilevelFixture(t, "zip", map[string][]byte{"inner.zip": data})
				path = "inner.zip/" + path
			}
			s := multilevelService(t, "zip", data, Options{}, 16<<20)
			if depth == 8 {
				multilevelRead(t, s, path, want)
				multilevelNames(t, s, strings.TrimSuffix(path, "/leaf.txt"), "leaf.txt")
			} else if _, _, err := s.OpenArchiveMember(context.Background(), 77, path); !errors.Is(err, syscall.ELOOP) {
				t.Fatalf("ninth nested archive read = %v, want ELOOP", err)
			}
		}
	})
}

func TestServiceNested7zEqualSizesDoNotProveCopy(t *testing.T) {
	// This real Delta+Copy archive has a single packed range with exactly the
	// uncompressed size. The filter changes every byte after the first, so a
	// direct mapping would expose transformed data instead of the member.
	data, err := os.ReadFile("testdata/nested-delta-copy-equal-size.7z")
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Repeat([]byte("AAAA-sevenzip-delta-noncopy\n"), 40)
	zr, err := sevenzip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	f := zr.File[0]
	r, err := f.Open()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil || !bytes.Equal(decoded, want) {
		t.Fatalf("ordinary decoder does not match independent fixture bytes: %v", err)
	}
	ranges, err := zr.PackedRanges(f.Stream)
	streams := zr.Streams()
	if err != nil || len(ranges) != 1 || ranges[0].Size != int64(streams[f.Stream].UncompressedSize) {
		t.Fatalf("fixture must have one equal-size packed range: %+v, %v", ranges, err)
	}
	offset, ok := f.StreamOffset()
	if !ok {
		t.Fatal("fixture has no member stream offset")
	}
	m := &member{name: f.Name, size: f.UncompressedSize, crc: f.CRC32, sevenCRCDefined: f.Checksum().Defined, ordinal: 0,
		sevenStream: &sevenStreamLocation{Stream: f.Stream, Offset: offset, Size: int64(streams[f.Stream].UncompressedSize)}}
	if _, _, direct, err := nestedCopyMemberReader(context.Background(), archivePlainInput{reader: bytes.NewReader(data)}, int64(len(data)), m, 0, nil); err != nil || direct {
		t.Fatalf("Delta+Copy direct mapping = %v, %v; want decoder fallback", direct, err)
	}
	outer := multilevelFixture(t, "zip", map[string][]byte{"nested.7z": data})
	s := multilevelService(t, "zip", outer, Options{}, 16<<20)
	multilevelRead(t, s, "nested.7z/inner.bin", want)
}
