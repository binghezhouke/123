package mountfs

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/hanwen/go-fuse/v2/fs"
)

func TestCacheSourceQuotaSharedByNestedAndRootReads(t *testing.T) {
	data := make([]byte, 2<<20)
	_, _ = rand.New(rand.NewSource(42)).Read(data)
	for _, nested := range []bool{false, true} {
		name := "root"
		if nested {
			name = "nested"
		}
		t.Run(name, func(t *testing.T) {
			payload := nestedDeflateLeaseZIP(t, "data.bin", data, zip.Deflate)
			member := "data.bin"
			if nested {
				payload = nestedDeflateLeaseZIP(t, "inner.zip", payload, zip.Deflate)
				member = "inner.zip/data.bin"
			}
			svc := newServiceNestedNamed(t, payload, "outer.zip")
			cache := svc.tree.cache
			if err := cache.ConfigureSourceLimit(1); err != nil {
				t.Fatal(err)
			}
			otherOwner := cache.StableDigest("test", "other")[:32]
			if err := cache.Store(storage.WithCacheOwner(context.Background(), otherOwner), "other-warm", make([]byte, 100<<10)); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			reader, size, err := svc.OpenArchiveMember(ctx, 77, member)
			if err != nil {
				t.Fatal(err)
			}
			if size != int64(len(data)) {
				t.Fatal(size)
			}
			for _, offset := range []int64{0, 1 << 20, 1500 << 10, 32 << 10} {
				buf := make([]byte, 4096)
				n, err := reader.ReadAt(buf, offset)
				if err != nil || n != len(buf) || !bytes.Equal(buf, data[offset:offset+int64(n)]) {
					t.Fatalf("offset=%d n=%d err=%v", offset, n, err)
				}
			}
			if err := reader.(io.Closer).Close(); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(3 * time.Second)
			for cache.Stats().ReservedBytes != 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			s := cache.Stats()
			if s.SourceCount != 2 || s.LargestSourceBytes > s.SourceLimitBytes || s.SourceOverLimitCount != 0 || s.SourceEvictions == 0 {
				t.Fatalf("source accounting after reads: %+v", s)
			}
			h, err := cache.OpenCached("other-warm", 100<<10)
			if err != nil || h == nil {
				t.Fatalf("unrelated warm object evicted: %v", err)
			}
			h.Close()
		})
	}
}

func TestCacheSourceQuotaFUSEReadsOversizedRootMember(t *testing.T) {
	data := make([]byte, 2<<20)
	_, _ = rand.New(rand.NewSource(43)).Read(data)
	root := fixtureArchive(t, nestedDeflateLeaseZIP(t, "data.bin", data, zip.Deflate))
	if err := root.tree.cache.ConfigureSourceLimit(1); err != nil {
		t.Fatal(err)
	}
	fs.NewNodeFS(root, &fs.Options{})
	file := lookup(t, lookup(t, root, "photos.zip"), "data.bin")
	for _, offset := range []int64{0, 1 << 20, 1500 << 10, 32 << 10} {
		if got := readNode(t, file, offset, 4096); !bytes.Equal(got, data[offset:offset+4096]) {
			t.Fatalf("FUSE read mismatch at %d", offset)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for root.tree.cache.Stats().ReservedBytes != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s := root.tree.cache.Stats(); s.SourceCount != 1 || s.LargestSourceBytes > s.SourceLimitBytes || s.SourceOverLimitCount != 0 || s.SourceEvictions == 0 {
		t.Fatalf("FUSE quota: %+v", s)
	}
}

func TestCacheSourceQuotaFUSEDecryptsOversizedMember(t *testing.T) {
	archive, err := os.ReadFile("testdata/encrypted/aes256-ae2-deflate.zip")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"fixture"`)
		http.ServeContent(w, r, "private.zip", time.Unix(100, 0), bytes.NewReader(archive))
	}))
	defer server.Close()
	cache, err := storage.NewCache(t.TempDir(), 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	if err := cache.ConfigureSourceLimit(10); err != nil {
		t.Fatal(err)
	}
	api := &encryptedFakeAPI{url: server.URL, archive: archive, password: []byte("mount-test-password")}
	root := New(context.Background(), api, cache, 0, true)
	fs.NewNodeFS(root, &fs.Options{})
	file := lookup(t, lookup(t, root, "private.zip"), "hello.txt")
	want := append(bytes.Repeat([]byte("mount123 encrypted fixture\n"), 256), byteSliceRange()...)
	if got := readNode(t, file, 0, len(want)); !bytes.Equal(got, want) {
		t.Fatal("encrypted demand read mismatch")
	}
}
