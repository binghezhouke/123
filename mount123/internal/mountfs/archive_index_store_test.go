package mountfs

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/nwaples/rardecode/v2"
)

type indexStoreTestAPI struct{}

func (indexStoreTestAPI) CacheIdentity() string                              { return "archive-index-test-account" }
func (indexStoreTestAPI) List(context.Context, int64) ([]panapi.File, error) { return nil, nil }
func (indexStoreTestAPI) DownloadURL(context.Context, int64) (string, error) { return "", nil }

func removeExceptIndexBlob(t *testing.T, dir, key string) {
	t.Helper()
	hash := sha256.Sum256([]byte(key))
	base := hex.EncodeToString(hash[:])
	items, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if strings.HasSuffix(item.Name(), ".blob") && item.Name() != base+".blob" && !strings.HasPrefix(item.Name(), base+".") {
			if err := os.Remove(filepath.Join(dir, item.Name())); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func indexTestRemote(t *testing.T, ctx context.Context, tree *Tree, cache *storage.Cache, name string, data io.ReaderAt, size int64, denyHeaders *atomic.Bool, requests *atomic.Int64) (*storage.Remote, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		rangeHeader := r.Header.Get("Range")
		if denyHeaders.Load() && rangeHeader != "" {
			parts := strings.Split(strings.TrimPrefix(rangeHeader, "bytes="), "-")
			if len(parts) == 2 {
				start, err := strconv.ParseInt(parts[0], 10, 64)
				if err == nil && start > 0 {
					http.Error(w, "archive scan forbidden after restart", http.StatusRequestedRangeNotSatisfiable)
					return
				}
			}
		}
		w.Header().Set("ETag", `"persisted-index-v1"`)
		http.ServeContent(w, r, name, time.Unix(1, 0), io.NewSectionReader(data, 0, size))
	}))
	remote, err := storage.NewRemote(ctx, cache, tree.diskCacheScope()+":cloud:77:v1:"+strconv.FormatInt(size, 10), size, func(context.Context) (string, error) { return server.URL, nil })
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	return remote, server
}

func TestZIPIndexRestoresWithoutReadingCentralDirectory(t *testing.T) {
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	w, err := zw.Create("entry.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("persisted ZIP member"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	data := append([]byte(nil), archive.Bytes()...)
	dir := filepath.Join(t.TempDir(), "cache")
	ctx := context.Background()
	var requests atomic.Int64
	var deny atomic.Bool
	cache, err := storage.NewCache(dir, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	tree := New(ctx, indexStoreTestAPI{}, cache, 0, true).tree
	remote, server := indexTestRemote(t, ctx, tree, cache, "archive.zip", bytes.NewReader(data), int64(len(data)), &deny, &requests)
	a := &archiveDescriptor{id: 77, name: "archive.zip", version: "v1", size: int64(len(data))}
	idx, err := tree.getZIP(ctx, remote, int64(len(data)), a)
	if err != nil {
		t.Fatal(err)
	}
	identity := archiveIdentity(remote, a)
	key := tree.archiveIndexCacheKey(".zip", identity, a, nil)
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	server.Close()
	removeExceptIndexBlob(t, dir, key)

	requests.Store(0)
	deny.Store(true)
	cache, err = storage.NewCache(dir, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	tree2 := New(ctx, indexStoreTestAPI{}, cache, 0, true).tree
	remote2, server2 := indexTestRemote(t, ctx, tree2, cache, "archive.zip", bytes.NewReader(data), int64(len(data)), &deny, &requests)
	defer server2.Close()
	idx2, err := tree2.getZIP(ctx, remote2, int64(len(data)), a)
	if err != nil {
		t.Fatalf("restore ZIP index: %v", err)
	}
	if idx2 == idx || idx2.members["entry.txt"] == nil {
		t.Fatal("ZIP index was not restored as a fresh DTO")
	}
	if requests.Load() != 1 {
		t.Fatalf("restored ZIP index made %d requests; expected only the one-byte source probe", requests.Load())
	}
	if _, err := idx2.members["entry.txt"].dataOffset(ctx); err != nil {
		t.Fatalf("restored local header location: %v", err)
	}
}

func TestArchiveCheckpointMarkerAndLegacyCompletion(t *testing.T) {
	cache, err := storage.NewCache(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	tree := New(context.Background(), indexStoreTestAPI{}, cache, 0, true).tree
	ctx := context.Background()
	identity := cache.StableDigest("test", "rar")
	key := tree.archiveIndexCacheKey(".rar", "rar-identity", &archiveDescriptor{id: 7, name: "a.rar", size: 100, version: "v1"}, nil)
	dto := archiveIndexDTO{Version: archiveIndexFormatVersion, Kind: ".rar", ArchiveSize: 100, IdentityDigest: identity, Complete: false, ScanOffset: 42}
	data, _ := json.Marshal(dto)
	if err := cache.StoreArchiveIndex(ctx, key+":checkpoint", data); err != nil {
		t.Fatal(err)
	}
	idx, ok := tree.loadArchiveCheckpoint(ctx, key, ".rar", 100, identity)
	if !ok || idx.complete {
		t.Fatalf("checkpoint was treated as complete: ok=%v idx=%#v", ok, idx)
	}
	var raw archiveIndexDTO
	if err := json.Unmarshal(data, &raw); err != nil || raw.Complete || raw.ScanOffset != 42 {
		t.Fatalf("checkpoint marker lost: %#v", raw)
	}
	legacy := []byte(fmt.Sprintf(`{"version":%d,"kind":".rar","archive_size":100,"identity_digest":"%s"}`, archiveIndexFormatVersion, identity))
	if err := cache.StoreArchiveIndex(ctx, key, legacy); err != nil {
		t.Fatal(err)
	}
	if restored, ok := tree.loadPersistentArchiveIndex(ctx, key, ".rar", 100, identity); !ok || !restored.complete {
		t.Fatalf("legacy index was not completed: ok=%v idx=%#v", ok, restored)
	}
}

func TestSevenZipIndexRestoresWithoutRescanningHeaders(t *testing.T) {
	data, err := os.ReadFile("testdata/encrypted/plain.7z")
	if err != nil {
		t.Fatal(err)
	}
	assertPersistentNonZIPIndex(t, ".7z", "archive.7z", data)
}

func TestRARIndexRestoresWithoutRescanningHeaders(t *testing.T) {
	fixture := rarFixture(4, 128<<10)
	data := fixture
	assertPersistentSparseRARIndex(t, data)
}

func assertPersistentNonZIPIndex(t *testing.T, kind, name string, data []byte) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cache")
	ctx := context.Background()
	var requests atomic.Int64
	var deny atomic.Bool
	cache, err := storage.NewCache(dir, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	tree := New(ctx, indexStoreTestAPI{}, cache, 0, true).tree
	remote, server := indexTestRemote(t, ctx, tree, cache, name, bytes.NewReader(data), int64(len(data)), &deny, &requests)
	a := &archiveDescriptor{id: 77, name: name, version: "v1", size: int64(len(data))}
	idx, err := tree.otherIndex(ctx, remote, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	identity := archiveIdentity(remote, a)
	key := tree.archiveIndexCacheKey(kind, identity, a, nil)
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	server.Close()
	removeExceptIndexBlob(t, dir, key)

	requests.Store(0)
	deny.Store(true)
	cache, err = storage.NewCache(dir, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	tree2 := New(ctx, indexStoreTestAPI{}, cache, 0, true).tree
	remote2, server2 := indexTestRemote(t, ctx, tree2, cache, name, bytes.NewReader(data), int64(len(data)), &deny, &requests)
	defer server2.Close()
	idx2, err := tree2.otherIndex(ctx, remote2, a, nil)
	if err != nil {
		t.Fatalf("restore %s index: %v", kind, err)
	}
	if idx2 == idx || len(idx2.members) != len(idx.members) {
		t.Fatal("archive index was not restored as a complete DTO")
	}
	if requests.Load() != 1 {
		t.Fatalf("restored %s index made %d requests; expected only the one-byte source probe", kind, requests.Load())
	}
}

func assertPersistentSparseRARIndex(t *testing.T, data sparseRAR) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cache")
	ctx := context.Background()
	var requests atomic.Int64
	var deny atomic.Bool
	cache, err := storage.NewCache(dir, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	tree := New(ctx, indexStoreTestAPI{}, cache, 0, true).tree
	remote, server := indexTestRemote(t, ctx, tree, cache, "archive.rar", data, data.size, &deny, &requests)
	a := &archiveDescriptor{id: 77, name: "archive.rar", version: "v1", size: data.size}
	idx, err := tree.otherIndex(ctx, remote, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	identity := archiveIdentity(remote, a)
	key := tree.archiveIndexCacheKey(".rar", identity, a, nil)
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	server.Close()
	removeExceptIndexBlob(t, dir, key)

	requests.Store(0)
	deny.Store(true)
	cache, err = storage.NewCache(dir, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	tree2 := New(ctx, indexStoreTestAPI{}, cache, 0, true).tree
	remote2, server2 := indexTestRemote(t, ctx, tree2, cache, "archive.rar", data, data.size, &deny, &requests)
	defer server2.Close()
	idx2, err := tree2.otherIndex(ctx, remote2, a, nil)
	if err != nil {
		t.Fatalf("restore RAR index: %v", err)
	}
	if idx2 == idx || len(idx2.members) != len(idx.members) {
		t.Fatal("RAR index was not restored as a complete DTO")
	}
	for _, member := range idx2.members {
		if member.rarLocator == nil {
			t.Fatal("restored RAR member lost its locator")
		}
		if _, err := rardecode.ImportLocator(rardecode.ExportLocator(*member.rarLocator), data.size); err != nil {
			t.Fatalf("restored RAR locator: %v", err)
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("restored RAR index made %d requests; expected only the one-byte source probe", requests.Load())
	}
}

func TestArchiveIndexDTORejectsUnsupportedVersion(t *testing.T) {
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	w, err := zw.Create("entry.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("content"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	data := append([]byte(nil), archive.Bytes()...)
	cache, err := storage.NewCache(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	ctx := context.Background()
	var requests atomic.Int64
	var deny atomic.Bool
	tree := New(ctx, indexStoreTestAPI{}, cache, 0, true).tree
	remote, server := indexTestRemote(t, ctx, tree, cache, "archive.zip", bytes.NewReader(data), int64(len(data)), &deny, &requests)
	defer server.Close()
	a := &archiveDescriptor{id: 77, name: "archive.zip", version: "v1", size: int64(len(data))}
	key := tree.archiveIndexCacheKey(".zip", archiveIdentity(remote, a), a, nil)
	bad := fmt.Sprintf(`{"version":%d,"kind":".zip"}`, archiveIndexFormatVersion+1)
	if err := cache.Store(ctx, key, []byte(bad)); err != nil {
		t.Fatal(err)
	}
	idx, err := tree.getZIP(ctx, remote, int64(len(data)), a)
	if err != nil {
		t.Fatalf("unsupported DTO version did not rebuild: %v", err)
	}
	if idx.members["entry.txt"] == nil || requests.Load() < 2 {
		t.Fatal("unsupported DTO did not trigger an archive rebuild")
	}
}

func TestArchiveCacheIdentityScopesRemoteBlocksAndPasswordTags(t *testing.T) {
	cache, err := storage.NewCache(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	first := New(context.Background(), indexStoreTestAPI{}, cache, 0, true).tree
	otherAPI := New(context.Background(), indexStoreOtherAPI{}, cache, 0, true).tree
	f := &panapi.File{ID: 77, Name: "archive.rar", Size: 123, Version: "v1"}
	if first.cloudCacheKey(f) == otherAPI.cloudCacheKey(f) {
		t.Fatal("different API identities shared raw remote block keys")
	}
	a := &archiveDescriptor{id: f.ID, name: f.Name, size: f.Size, version: f.Version}
	password := []byte("private password")
	tag := first.passwordTag(a, password)
	reopened := New(context.Background(), indexStoreTestAPI{}, cache, 0, true).tree
	if tag != reopened.passwordTag(a, password) {
		t.Fatal("password cache identity changed between Tree instances")
	}
	if tag == first.passwordTag(a, []byte("different password")) {
		t.Fatal("password change reused the same identity")
	}
	if tag == first.passwordTag(&archiveDescriptor{id: a.id, name: a.name, size: a.size, version: "v2"}, password) {
		t.Fatal("archive version change reused the same identity")
	}
}

type indexStoreOtherAPI struct{}

func (indexStoreOtherAPI) CacheIdentity() string                              { return "other-archive-index-test-account" }
func (indexStoreOtherAPI) List(context.Context, int64) ([]panapi.File, error) { return nil, nil }
func (indexStoreOtherAPI) DownloadURL(context.Context, int64) (string, error) { return "", nil }
