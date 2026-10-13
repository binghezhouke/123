package mountfs

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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

type rootZIPSplitAPI struct {
	mu          sync.Mutex
	url         string
	files       []panapi.File
	bodies      map[int64][]byte
	password    []byte
	requests    int
	denyNonzero bool
}

func (a *rootZIPSplitAPI) CacheIdentity() string { return "synthetic-root-zip-split" }

func (a *rootZIPSplitAPI) List(context.Context, int64) ([]panapi.File, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]panapi.File(nil), a.files...), nil
}
func (a *rootZIPSplitAPI) Detail(_ context.Context, id int64) (panapi.File, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, f := range a.files {
		if f.ID == id {
			return f, nil
		}
	}
	return panapi.File{}, syscall.ENOENT
}
func (a *rootZIPSplitAPI) Infos(ctx context.Context, ids []int64) ([]panapi.File, error) {
	var out []panapi.File
	for _, id := range ids {
		f, err := a.Detail(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}
func (a *rootZIPSplitAPI) DownloadURL(_ context.Context, id int64) (string, error) {
	return a.url + "/" + strconv.FormatInt(id, 10), nil
}
func (a *rootZIPSplitAPI) ReadSmallFile(context.Context, int64, int64) ([]byte, error) {
	return append([]byte(nil), a.password...), nil
}

func rootZIPSplitFixture(t *testing.T, data []byte, chunk int, password []byte, opts Options) (*Service, *Node, *rootZIPSplitAPI) {
	t.Helper()
	parts := map[string][]byte{}
	for off, ordinal := 0, 1; off < len(data); ordinal++ {
		end := min(off+chunk, len(data))
		parts[fmt.Sprintf("sample.zip.%03d", ordinal)] = append([]byte(nil), data[off:end]...)
		off = end
	}
	return rootZIPSplitPartsFixture(t, parts, "sample.zip", password, opts)
}

func rootZIPSplitPartsFixture(t *testing.T, parts map[string][]byte, logicalName string, password []byte, opts Options) (*Service, *Node, *rootZIPSplitAPI) {
	t.Helper()
	a := &rootZIPSplitAPI{bodies: map[int64][]byte{}, password: password}
	var names []string
	for name := range parts {
		names = append(names, name)
	}
	sort.Strings(names)
	for i, name := range names {
		id := int64(i + 1)
		data := parts[name]
		a.files = append(a.files, panapi.File{ID: id, Name: name, Size: int64(len(data)), Version: "v1"})
		a.bodies[id] = append([]byte(nil), data...)
	}
	if len(password) != 0 {
		a.files = append(a.files, panapi.File{ID: 9000, Name: logicalName + ".pwd", Size: int64(len(password)), Version: "p1"})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/"), 10, 64)
		a.mu.Lock()
		data, ok := a.bodies[id]
		a.requests++
		deny := a.denyNonzero
		a.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		if deny && strings.HasPrefix(r.Header.Get("Range"), "bytes=") && !strings.HasPrefix(r.Header.Get("Range"), "bytes=0-") {
			http.Error(w, "metadata scan prohibited after restore", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("ETag", fmt.Sprintf("\"part-%d\"", id))
		http.ServeContent(w, r, "sample", time.Unix(1, 0), bytes.NewReader(data))
	}))
	t.Cleanup(server.Close)
	a.url = server.URL
	cache, err := storage.NewCache(t.TempDir(), 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	root := NewWithOptions(context.Background(), a, cache, 0, true, opts)
	fs.NewNodeFS(root, &fs.Options{})
	return &Service{tree: root.tree}, root, a
}

func TestRootZIPDiskServiceAndFUSE(t *testing.T) {
	for _, method := range []string{"store", "deflate"} {
		for _, encryption := range []string{"", "ZipCrypto", "AES256"} {
			t.Run(method+"/"+encryption, func(t *testing.T) {
				entry, parts, payloads := generatedSplitZIP(t, "disks", method, encryption)
				var password []byte
				if encryption != "" {
					password = []byte(splitFixturePassword)
				}
				s, root, api := rootZIPSplitPartsFixture(t, parts, entry, password, Options{StreamMemberThreshold: 32 << 10})
				var finalID int64
				for _, f := range api.files {
					if f.Name == entry {
						finalID = f.ID
					}
				}
				if finalID == 0 {
					t.Fatal("missing final volume")
				}
				entries, err := s.ListArchive(context.Background(), finalID, "")
				if err != nil || len(entries) != len(payloads) {
					t.Fatalf("list=%v err=%v", entries, err)
				}
				dir := lookup(t, root, entry)
				for name, want := range payloads {
					rootZIPRead(t, s, finalID, name, want)
					readOther(t, lookup(t, dir, name), want)
				}
				// Opening the final file as raw data retains the final volume bytes.
				r, size, err := s.OpenFile(context.Background(), finalID)
				if err != nil {
					t.Fatal(err)
				}
				got := make([]byte, size)
				if _, err := r.ReadAt(got, 0); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, parts[entry]) {
					t.Fatal("raw final ZIP changed")
				}
			})
		}
	}
}

func TestRootZIPSplitPreparedInputsPersistAndFreezeManifest(t *testing.T) {
	entry, parts, payloads := generatedSplitZIP(t, "disks", "deflate", "")
	s, root, api := rootZIPSplitPartsFixture(t, parts, entry, nil, Options{})
	ctx := context.Background()
	var finalID int64
	for _, f := range api.files {
		if f.Name == entry {
			finalID = f.ID
		}
	}
	idx, a, source, password, err := s.outerArchiveIndex(ctx, finalID)
	clear(password)
	if err != nil {
		t.Fatal(err)
	}
	rootZIPRead(t, s, finalID, "payload-0.bin", payloads["payload-0.bin"])
	key := root.tree.archiveIndexCacheKey(".zip", idx.prepared.identity, a, nil)
	digest := root.tree.cache.StableDigest("archive-index-identity-v1", idx.prepared.identity)
	if _, ok := root.tree.loadPersistentArchiveIndex(ctx, key, ".zip", idx.prepared.size, digest); !ok {
		t.Fatal("split index was not persisted with raw size")
	}
	oldInput := idx.prepared.input.(*volumeReaderAt)
	oldPartID := oldInput.parts[0].ID
	// Replace a predecessor with a distinct immutable ID, keeping the final
	// volume unchanged. Existing entries must retain their original manifest.
	api.mu.Lock()
	for i := range api.files {
		if api.files[i].ID == oldPartID {
			api.files[i].ID = 8000
			api.bodies[8000] = api.bodies[oldPartID]
		}
	}
	api.mu.Unlock()
	if _, err := root.RefreshDirectory(ctx, "."); err != nil {
		t.Fatal(err)
	}
	newIdx, err := root.tree.getZIP(ctx, source, a.size, a)
	if err != nil {
		t.Fatal(err)
	}
	if newIdx == idx || newIdx.prepared.identity == idx.prepared.identity || rootZIPMemberIdentity(newIdx.prepared) == rootZIPMemberIdentity(idx.prepared) {
		t.Fatal("replacement predecessor reused index or member identity")
	}
	if oldInput.parts[0].ID != oldPartID || newIdx.prepared.input.(*volumeReaderAt).parts[0].ID != 8000 {
		t.Fatal("prepared manifest changed or was not refreshed")
	}
	oldEntry := idx.children("", source, a.size, a)["payload-0.bin"]
	newEntry := newIdx.children("", source, a.size, a)["payload-0.bin"]
	if stableAttrForEntry(1, oldEntry).Ino == stableAttrForEntry(1, newEntry).Ino {
		t.Fatal("replacement predecessor reused FUSE member inode")
	}
	prefetch := &imagePrefetch{}
	if prefetch.targetIdentity(&Node{item: oldEntry}) == prefetch.targetIdentity(&Node{item: newEntry}) {
		t.Fatal("replacement predecessor reused speculative member identity")
	}
	view, err := root.tree.rootArchiveView(ctx, source, a, idx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	if view.input != idx.prepared.input || view.size != idx.prepared.size {
		t.Fatal("root view rediscovered input after indexing")
	}
	if _, err := rootZIPDataOffset(ctx, idx.members["payload-0.bin"], idx.prepared); err != nil {
		t.Fatalf("old frozen input unreadable: %v", err)
	}
}

func TestRootZIPByteSplitWarmRestoreWithoutPayloadRead(t *testing.T) {
	var data bytes.Buffer
	zw := zip.NewWriter(&data)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "payload.bin", Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Repeat([]byte("payload"), 20000)
	_, _ = w.Write(want)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	s, root, api := rootZIPSplitFixture(t, data.Bytes(), 64<<10, nil, Options{})
	// Immutable archive IDs are durable even when the cloud API omits versions.
	api.mu.Lock()
	for i := range api.files {
		api.files[i].Version = ""
	}
	api.mu.Unlock()
	idx, a, _, password, err := s.outerArchiveIndex(context.Background(), 1)
	clear(password)
	if err != nil {
		t.Fatal(err)
	}
	if !idx.prepared.durable {
		t.Fatal("immutable split input was not durable")
	}
	key := root.tree.archiveIndexCacheKey(".zip", idx.prepared.identity, a, nil)
	dir := root.tree.cache.Directory()
	if err := root.tree.cache.Close(); err != nil {
		t.Fatal(err)
	}
	removeExceptIndexBlob(t, dir, key)
	api.mu.Lock()
	api.requests = 0
	api.denyNonzero = true
	api.mu.Unlock()
	cache, err := storage.NewCache(dir, 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	restarted := NewWithOptions(context.Background(), api, cache, 0, true, Options{})
	service := NewServiceFromNode(restarted)
	restored, _, _, password, err := service.outerArchiveIndex(context.Background(), 1)
	clear(password)
	if err != nil {
		t.Fatalf("warm restore: %v", err)
	}
	if restored.members["payload.bin"].file != nil || restored.prepared.size != int64(data.Len()) {
		t.Fatal("index was rescanned or synthetic size persisted")
	}
	api.mu.Lock()
	calls := api.requests
	api.denyNonzero = false
	api.mu.Unlock()
	if calls != 0 {
		t.Fatalf("warm split restore performed %d requests, want no network requests before a cache miss", calls)
	}
	rootZIPRead(t, service, 1, "payload.bin", want)
}

func TestRootZIPSplitReadDirPlusIsMetadataOnly(t *testing.T) {
	var data bytes.Buffer
	zw := zip.NewWriter(&data)
	w, err := zw.Create("payload.bin")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("data"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	_, root, api := rootZIPSplitFixture(t, data.Bytes(), 100, nil, Options{})
	h, _, errno := root.OpendirHandle(context.Background(), 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	defer h.(fs.FileReleasedirer).Releasedir(context.Background(), 0)
	for {
		de, errno := h.(fs.FileReaddirenter).Readdirent(context.Background())
		if errno != 0 {
			t.Fatal(errno)
		}
		if de == nil {
			break
		}
		if _, errno := h.(fs.FileLookuper).Lookup(context.Background(), de.Name, &fuse.EntryOut{}); errno != 0 {
			t.Fatal(errno)
		}
	}
	api.mu.Lock()
	calls := api.requests
	api.mu.Unlock()
	if calls != 0 {
		t.Fatalf("READDIRPLUS initiated %d source reads", calls)
	}
	readOther(t, lookup(t, lookup(t, root, "sample.zip.001"), "payload.bin"), []byte("data"))
}

func TestRootZIPDiskIndexProgressAndStatusUseRawTotal(t *testing.T) {
	entry, parts, _ := generatedSplitZIP(t, "disks", "store", "")
	_, root, api := rootZIPSplitPartsFixture(t, parts, entry, nil, Options{})
	ctx := context.Background()
	var file panapi.File
	var total int64
	for _, f := range api.files {
		total += f.Size
		if f.Name == entry {
			file = f
		}
	}
	source, err := root.tree.cloudFileSource(ctx, &file, root.tree.opts.SourceTTL)
	if err != nil {
		t.Fatal(err)
	}
	a := &archiveDescriptor{id: file.ID, name: file.Name, version: file.Version, size: file.Size}
	var offsets []int64
	idx, err := root.tree.getZIPWithProgress(ctx, source, a.size, a, func(off int64) { offsets = append(offsets, off) })
	if err != nil {
		t.Fatal(err)
	}
	if len(offsets) == 0 {
		t.Fatal("raw metadata reads did not report progress")
	}
	for _, off := range offsets {
		if off < 0 || off > total {
			t.Fatalf("synthetic metadata offset %d exceeded raw extent %d", off, total)
		}
	}
	if idx.prepared.size != total || total == a.size {
		t.Fatal("fixture did not retain raw combined size")
	}
	status, err := root.StatusArchiveIndex(ctx, entry)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "complete" || status.ArchiveSize != total || status.ScanOffset != total {
		t.Fatalf("split status=%+v want complete raw total %d", status, total)
	}
}

func rootZIPRead(t *testing.T, s *Service, id int64, name string, want []byte) {
	t.Helper()
	r, size, err := s.OpenArchiveMember(context.Background(), id, name)
	if err != nil {
		t.Fatal(err)
	}
	if closer, ok := r.(io.Closer); ok {
		defer closer.Close()
	}
	if size != int64(len(want)) {
		t.Fatalf("size=%d want=%d", size, len(want))
	}
	for _, off := range []int64{0, size / 2, max(0, size-4096)} {
		got := make([]byte, min(4096, size-off))
		if _, err := r.ReadAt(got, off); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want[off:off+int64(len(got))]) {
			t.Fatalf("member bytes differ at %d", off)
		}
	}
}

func TestRootZIPByteSplitServiceAndFUSE(t *testing.T) {
	for _, method := range []uint16{zip.Store, zip.Deflate} {
		t.Run(fmt.Sprint(method), func(t *testing.T) {
			content := make([]byte, 270000)
			_, _ = rand.New(rand.NewSource(77)).Read(content)
			var data bytes.Buffer
			zw := zip.NewWriter(&data)
			w, err := zw.CreateHeader(&zip.FileHeader{Name: "payload.bin", Method: method})
			if err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write(content)
			if err := zw.Close(); err != nil {
				t.Fatal(err)
			}
			s, root, api := rootZIPSplitFixture(t, data.Bytes(), 64<<10, nil, Options{StreamMemberThreshold: 32 << 10, DisableReadAhead: true})
			entries, err := s.ListArchive(context.Background(), 1, "")
			if err != nil || len(entries) != 1 {
				t.Fatalf("list=%v err=%v", entries, err)
			}
			rootZIPRead(t, s, 1, "payload.bin", content)
			readOther(t, lookup(t, lookup(t, root, "sample.zip.001"), "payload.bin"), content)
			// Later volumes continue to expose their original bytes as cloud files.
			r, size, err := s.OpenFile(context.Background(), 2)
			if err != nil {
				t.Fatal(err)
			}
			raw := make([]byte, size)
			if _, err := r.ReadAt(raw, 0); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(raw, api.bodies[2]) {
				t.Fatal("raw later volume changed")
			}
		})
	}
}

func TestRootZIPByteSplitEncryptedServiceAndFUSE(t *testing.T) {
	fixtures, err := filepath.Glob("testdata/encrypted/*.zip")
	if err != nil {
		t.Fatal(err)
	}
	want := append(bytes.Repeat([]byte("mount123 encrypted fixture\n"), 256), byteSliceRange()...)
	for _, fixture := range fixtures {
		t.Run(filepath.Base(fixture), func(t *testing.T) {
			data, err := os.ReadFile(fixture)
			if err != nil {
				t.Fatal(err)
			}
			s, root, _ := rootZIPSplitFixture(t, data, 100, []byte("mount-test-password"), Options{StreamMemberThreshold: 1024})
			rootZIPRead(t, s, 1, "hello.txt", want)
			readOther(t, lookup(t, lookup(t, root, "sample.zip.001"), "hello.txt"), want)
		})
	}
}

func TestRootZIPStoreUsesRequestContextAndRawBounds(t *testing.T) {
	var data bytes.Buffer
	zw := zip.NewWriter(&data)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "payload.bin", Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write(bytes.Repeat([]byte("x"), 1000))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	s, _, _ := rootZIPSplitFixture(t, data.Bytes(), 100, nil, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	reader, _, err := s.OpenArchiveMember(ctx, 1, "payload.bin")
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := reader.ReadAt(make([]byte, 1), 0); err != context.Canceled {
		t.Fatalf("canceled Store read=%v", err)
	}
}
