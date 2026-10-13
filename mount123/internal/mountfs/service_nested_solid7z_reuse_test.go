package mountfs

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/hanwen/go-fuse/v2/fs"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/bodgit/sevenzip"
	"github.com/ulikunitz/xz/lzma"
)

type solid7zReuseCounts struct {
	initializations atomic.Int64
	returnedOutput  atomic.Int64
}

// This test-only adapter mirrors sevenzip's pinned internal/lzma2/reader.go
// constructor, read errors and Close ownership/semantics. Attribution/license:
// testdata/solid7z/LICENSE.sevenzip. The real pinned xz decoder does all work.
// Count only bytes actually returned by its Read, not internal codec lookahead
// or CPU time. One plain LZMA2 coder makes this the group-output boundary.
type solid7zReuseLZMA2 struct {
	closer io.Closer
	reader io.Reader
	counts *solid7zReuseCounts
}

var solid7zReuseAlreadyClosed = errors.New("lzma2: already closed")

func (r *solid7zReuseLZMA2) Read(p []byte) (int, error) {
	if r.reader == nil {
		return 0, solid7zReuseAlreadyClosed
	}
	n, err := r.reader.Read(p)
	r.counts.returnedOutput.Add(int64(n))
	if err != nil && !errors.Is(err, io.EOF) {
		err = fmt.Errorf("lzma2: error reading: %w", err)
	}
	return n, err
}

func (r *solid7zReuseLZMA2) Close() error {
	if r.closer == nil || r.reader == nil {
		return solid7zReuseAlreadyClosed
	}
	if err := r.closer.Close(); err != nil {
		return fmt.Errorf("lzma2: error closing: %w", err)
	}
	r.closer, r.reader = nil, nil
	return nil
}

func (c *solid7zReuseCounts) factory(properties []byte, _ uint64, readers []io.ReadCloser) (io.ReadCloser, error) {
	if len(readers) != 1 {
		return nil, errors.New("lzma2: need exactly one reader")
	}
	if len(properties) != 1 {
		return nil, errors.New("lzma2: not enough properties")
	}
	config := lzma.Reader2Config{DictCap: (2 | (int(properties[0]) & 1)) << (properties[0]/2 + 11)}
	if err := config.Verify(); err != nil {
		return nil, fmt.Errorf("lzma2: error verifying config: %w", err)
	}
	reader, err := config.NewReader2(readers[0])
	if err != nil {
		return nil, fmt.Errorf("lzma2: error creating reader: %w", err)
	}
	c.initializations.Add(1)
	return &solid7zReuseLZMA2{closer: readers[0], reader: reader, counts: c}, nil
}

func solid7zReusePayload(index int) []byte {
	state := uint32(4100 + index)
	block := make([]byte, 16384)
	for i := range block {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		block[i] = byte(state)
	}
	payload := make([]byte, (4<<20)+13)
	for offset := 0; offset < len(payload); offset += len(block) {
		copy(payload[offset:], block)
	}
	return payload
}

type solid7zReuseAPI struct {
	*serviceNestedAPI
	password string
}

func (a *solid7zReuseAPI) ReadSmallFile(_ context.Context, id, limit int64) ([]byte, error) {
	if id != 78 {
		return nil, os.ErrNotExist
	}
	return []byte(a.password), nil
}

func (*solid7zReuseAPI) CacheIdentity() string { return "synthetic-solid7z-reuse" }

// Keep the three public handles simultaneously open: a member must borrow a
// group decoder for each missing range, rather than retain an exclusive decoder
// for the whole handle lifetime. Observe work through the existing public codec
// registry in an isolated process so registration never affects other tests.
func TestServiceNestedSolid7zSequentialHeadsShareDemandedDecode(t *testing.T) {
	const childFlag = "MOUNT123_SOLID7Z_REUSE_CHILD"
	if os.Getenv(childFlag) != "1" {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, executable,
			"-test.run=^TestServiceNestedSolid7zSequentialHeadsShareDemandedDecode$", "-test.count=1", "-test.v")
		command.Env = append(os.Environ(), childFlag+"=1")
		output, err := command.CombinedOutput()
		t.Logf("isolated codec observation:\n%s", output)
		if err != nil {
			t.Fatalf("nested solid7z demanded-decode regression: %v", err)
		}
		return
	}

	counts := new(solid7zReuseCounts)
	sevenzip.RegisterDecompressor([]byte{0x21}, counts.factory)
	inner, err := os.ReadFile("testdata/solid7z/inner.7z")
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(inner)); got != "08b38858bb256efc500eda012e60887e6d852a122b6a04457d39e2ba1400f978" {
		t.Fatalf("immutable synthetic archive hash=%s", got)
	}
	reader, err := sevenzip.NewReader(bytes.NewReader(inner), int64(len(inner)))
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"member-a.bin", "member-b.bin", "member-c.bin"}
	hashes := []string{
		"eacaba81605ceb1a05168bbc3b5b7776afbaa6a2ca356a7a7a74e2852b8e816b",
		"f3b67116ecaa30a554d6a58c52c9defbf6eda9588a0dad2230eab19af66a06d7",
		"2281935fa2a58077ef8f1fc83d468318eb01be4409946cac3694d80e24f61f68",
	}
	streams := reader.Streams()
	if len(streams) != 1 || streams[0].Stored || len(reader.File) != 3 || streams[0].UncompressedSize != 12582951 {
		t.Fatal("fixture must have one compressed solid stream with three members")
	}
	payloads := make([][]byte, 3)
	for i, file := range reader.File {
		offset, ok := file.StreamOffset()
		if !ok || file.Stream != 0 || file.Name != names[i] || offset != int64(i)*4194317 || file.UncompressedSize != 4194317 {
			t.Fatalf("unexpected fixture member %d layout", i)
		}
		payloads[i] = solid7zReusePayload(i)
		if got := fmt.Sprintf("%x", sha256.Sum256(payloads[i])); got != hashes[i] {
			t.Fatalf("independent member %d oracle hash=%s", i, got)
		}
	}
	encrypted, err := os.ReadFile("testdata/solid7z/encrypted.7z")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name                    string
		method                  uint16
		shared, root, encrypted bool
	}{
		{"store", zip.Store, true, false, false},
		{"deflate_single_build_slot", zip.Deflate, true, false, false},
		{"budget_fallback", zip.Store, false, false, false},
		{"root_plain", zip.Store, true, true, false},
		{"root_encrypted", zip.Store, true, true, true},
		{"encrypted_store", zip.Store, true, false, true},
		{"encrypted_deflate_single_build_slot", zip.Deflate, true, false, true},
		{"encrypted_budget_fallback", zip.Store, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			counts.initializations.Store(0)
			counts.returnedOutput.Store(0)
			fixture, password := inner, ""
			if tc.encrypted {
				fixture, password = encrypted, "synthetic-solid-password"
			}
			testServiceNestedSolid7zReuse(t, fixture, names, payloads, counts, tc.method, tc.shared, tc.root, password)
		})
	}
}

func testServiceNestedSolid7zReuse(t *testing.T, inner []byte, names []string, payloads [][]byte, counts *solid7zReuseCounts, method uint16, shared, root bool, password string) {
	var outer bytes.Buffer
	writer := zip.NewWriter(&outer)
	member, err := writer.CreateHeader(&zip.FileHeader{Name: "inner.7z", Method: method})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := member.Write(inner); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	body, archiveName, memberPrefix := outer.Bytes(), "outer.zip", "inner.7z/"
	if root {
		body, archiveName, memberPrefix = inner, "outer.7z", ""
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"synthetic-solid7z-immutable"`)
		http.ServeContent(w, r, "outer.zip", time.Unix(100, 0), bytes.NewReader(body))
	}))
	defer server.Close()
	cache, err := storage.NewCache(t.TempDir(), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	lifetime, stop := context.WithCancel(context.Background())
	defer stop()
	file := panapi.File{ID: 77, Name: archiveName, Size: int64(len(body)), Version: "synthetic-solid7z-v1"}
	files := []panapi.File{file}
	if password != "" {
		files = append(files, panapi.File{ID: 78, Name: archiveName + ".pwd", Size: int64(len(password))})
	}
	api := &solid7zReuseAPI{serviceNestedAPI: &serviceNestedAPI{url: server.URL, files: files, detail: file}, password: password}
	rootNode := NewWithOptions(lifetime, api, cache, 0, true, Options{
		DisableReadAhead: true, DisableStreamMembers: true, MaxConcurrentBuilds: 1, MaxArchiveDepth: 2,
	})
	if err := rootNode.Prepare(lifetime); err != nil {
		t.Fatal(err)
	}
	fs.NewNodeFS(rootNode, &fs.Options{})
	service := NewServiceFromNode(rootNode)
	defer service.tree.nestedDeflatePool.Close()
	defer service.tree.nestedSolid7zPool.Close()
	if !shared {
		service.tree.nestedSolid7zPool.maxBytes = 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	entries, err := service.ListArchive(ctx, 77, strings.TrimSuffix(memberPrefix, "/"))
	if err != nil || len(entries) != 3 {
		t.Fatalf("public nested listing count=%d, error=%v", len(entries), err)
	}
	if counts.initializations.Load() != 0 || counts.returnedOutput.Load() != 0 {
		t.Fatal("uncompressed headers/index seed initialized a body decoder")
	}
	// All handles remain live until after both phases, including cold reads.
	handles := make([]io.ReaderAt, 3)
	for i, name := range names {
		handle, size, err := service.OpenArchiveMember(ctx, 77, memberPrefix+name)
		if err != nil || size != 4194317 {
			t.Fatalf("public open member %d size=%d, error=%v", i, size, err)
		}
		handles[i] = handle
		defer handle.(io.Closer).Close()
	}
	readHead := func(i int) {
		t.Helper()
		got := make([]byte, 64<<10)
		request, finish := context.WithCancel(ctx)
		defer finish()
		n, err := solidReuseRead(request, handles[i], got, 0)
		if n != len(got) || err != nil || !bytes.Equal(got, payloads[i][:len(got)]) {
			t.Fatalf("public member %d head n=%d, error=%v, bytes_match=%v", i, n, err, bytes.Equal(got, payloads[i][:len(got)]))
		}
	}
	for _, i := range []int{0, 1, 2} {
		if i != 1 {
			readHead(i)
			continue
		}
		// Alternate adapters against one Tree. The FUSE open has its own archive
		// view, but must continue the same cursor and populate the web range cache.
		node := lookup(t, rootNode, archiveName)
		for _, part := range strings.Split(memberPrefix+names[i], "/") {
			node = lookup(t, node, part)
		}
		h, _, errno := node.Open(ctx, syscall.O_RDONLY)
		if errno != 0 {
			t.Fatal(errno)
		}
		result, errno := h.(fs.FileReader).Read(ctx, make([]byte, 64<<10), 0)
		if errno != 0 {
			h.(fs.FileReleaser).Release(ctx)
			t.Fatal(errno)
		}
		got, status := result.Bytes(nil)
		matched := bytes.Equal(got, payloads[i][:64<<10])
		result.Done()
		h.(fs.FileReleaser).Release(ctx)
		if status != 0 || !matched {
			t.Fatal("FUSE adapter head differs from plaintext")
		}
	}
	coldInitializations, coldOutput := counts.initializations.Load(), counts.returnedOutput.Load()
	t.Logf("cold A/B/C: decoder_initializations=%d returned_output=%d", coldInitializations, coldOutput)
	expected := int64(1)
	if !shared {
		expected = 3
	}
	if coldInitializations != expected {
		t.Errorf("same solid group initialized %d decoders, want %d", coldInitializations, expected)
	}
	if shared && coldOutput > 10<<20 {
		t.Errorf("heads decoded %d returned bytes, want <=10 MiB without unrelated group suffix", coldOutput)
	}
	for _, i := range []int{2, 1, 0} {
		readHead(i)
	}
	warmInitializations := counts.initializations.Load() - coldInitializations
	warmOutput := counts.returnedOutput.Load() - coldOutput
	t.Logf("warm C/B/A: decoder_initializations=%d returned_output=%d", warmInitializations, warmOutput)
	if warmInitializations != 0 || warmOutput != 0 {
		t.Errorf("warm reversed heads initialized=%d returned=%d bytes, want zero new decode", warmInitializations, warmOutput)
	}

	if !shared {
		return
	}
	// An uncached earlier range may replay privately, but must not replace the
	// group cursor. The later member's next miss then continues that cursor.
	for _, i := range []int{0, 2} {
		got := make([]byte, 64<<10)
		request, finish := context.WithCancel(ctx)
		n, err := solidReuseRead(request, handles[i], got, 2<<20)
		finish()
		if n != len(got) || err != nil || !bytes.Equal(got, payloads[i][2<<20:(2<<20)+len(got)]) {
			t.Fatalf("backward/forward miss member %d: n=%d error=%v", i, n, err)
		}
	}
	if got := counts.initializations.Load() - coldInitializations; got != 1 {
		t.Fatalf("backward fallback reset forward group: extra initializations=%d want 1", got)
	}
}

func solidReuseRead(ctx context.Context, reader io.ReaderAt, p []byte, off int64) (int, error) {
	if r, ok := reader.(archiveReaderAt); ok {
		return r.ReadAtContext(ctx, p, off)
	}
	return reader.ReadAt(p, off)
}
