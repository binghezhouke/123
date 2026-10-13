package mountfs

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func TestRoot7zDemandReusesVerifiedLegacyCaches(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		name, password := "inner.7z", ""
		if encrypted {
			name, password = "encrypted.7z", "synthetic-solid-password"
		}
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile("testdata/solid7z/" + name)
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.ServeContent(w, r, "archive.7z", time.Unix(100, 0), bytes.NewReader(data))
			}))
			defer server.Close()
			cache, err := storage.NewCache(t.TempDir(), 64<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer cache.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			file := panapi.File{ID: 77, Name: "archive.7z", Size: int64(len(data)), Version: "immutable"}
			files := []panapi.File{file}
			if password != "" {
				files = append(files, panapi.File{ID: 78, Name: file.Name + ".pwd", Size: int64(len(password))})
			}
			api := &solid7zReuseAPI{serviceNestedAPI: &serviceNestedAPI{url: server.URL, files: files, detail: file}, password: password}
			service, err := NewService(ctx, api, cache, 0, true, Options{DisableReadAhead: true, DisableStreamMembers: true, MaxConcurrentBuilds: 1})
			if err != nil {
				t.Fatal(err)
			}
			defer service.tree.nestedSolid7zPool.Close()
			defer service.tree.nestedDeflatePool.Close()
			// Fill through the actual old route, rather than seeding unverified
			// bytes directly under a cache key.
			service.tree.nestedSolid7zPool.maxBytes = 1
			read := func(member string, index int) {
				t.Helper()
				r, _, err := service.OpenArchiveMember(ctx, 77, member)
				if err != nil {
					t.Fatal(err)
				}
				defer r.(io.Closer).Close()
				p := make([]byte, 64<<10)
				n, err := solidReuseRead(ctx, r, p, 0)
				if err != nil || n != len(p) || !bytes.Equal(p, solid7zReusePayload(index)[:len(p)]) {
					t.Fatalf("cached head: n=%d err=%v", n, err)
				}
			}
			read("member-a.bin", 0)
			idx, archive, source, pw, err := service.outerArchiveIndex(ctx, 77)
			clear(pw)
			if err != nil {
				t.Fatal(err)
			}
			_, _, identity, err := service.tree.archiveSource(ctx, source, archive)
			if err != nil {
				t.Fatal(err)
			}
			key := service.tree.solid7zKey(identity, archive, idx.members["member-a.bin"], []byte(password))
			// A growing fill is separate from Cache.Open's ordinary flights.
			for {
				group, err := cache.OpenCached(key, idx.members["member-a.bin"].sevenStream.Size)
				if err != nil {
					t.Fatal(err)
				}
				if group != nil {
					group.Close()
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(time.Millisecond):
				}
			}
			service.tree.nestedSolid7zPool.maxBytes = nestedSolid7zMemoryBytes
			before := *cache.IOStats().Snapshot().Stages.Decompression.Samples
			read("member-b.bin", 1) // Only the group existed for B.
			if err := cache.Remove(key); err != nil {
				t.Fatal(err)
			}
			read("member-a.bin", 0) // Only A's old complete-member cache remains.
			after := *cache.IOStats().Snapshot().Stages.Decompression.Samples
			if after != before {
				t.Fatalf("verified caches were decoded again: before=%d after=%d", before, after)
			}
		})
	}
}

func TestActualFUSESolid7zDemand(t *testing.T) {
	if os.Getenv("MOUNT123_FUSE_TEST") != "1" {
		t.Skip("set MOUNT123_FUSE_TEST=1 to exercise the kernel FUSE path")
	}
	data, err := os.ReadFile("testdata/solid7z/encrypted.7z")
	if err != nil {
		t.Fatal(err)
	}
	for _, nested := range []bool{false, true} {
		name, body, prefix := "archive.7z", data, ""
		if nested {
			name, body, prefix = "outer.zip", nestedDeflateLeaseZIP(t, "inner.7z", data, zip.Deflate), "inner.7z/"
		}
		t.Run(name, func(t *testing.T) {
			root, _ := otherArchiveFixture(t, name, body, []byte("synthetic-solid-password"), false)
			point := t.TempDir()
			mounted, err := fs.Mount(point, root, &fs.Options{MountOptions: fuse.MountOptions{Options: []string{"ro"}}})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := mounted.Unmount(); err != nil {
					t.Error(err)
				}
				mounted.Wait()
			}()
			service := NewServiceFromNode(root)
			for i, member := range []string{"member-a.bin", "member-b.bin", "member-c.bin"} {
				f, err := os.Open(filepath.Join(point, name, prefix, member))
				if err != nil {
					t.Fatal(err)
				}
				p := make([]byte, 64<<10)
				n, err := f.ReadAt(p, 0)
				f.Close()
				if err != nil || n != len(p) || !bytes.Equal(p, solid7zReusePayload(i)[:len(p)]) {
					t.Fatalf("kernel head: n=%d err=%v", n, err)
				}
				r, _, err := service.OpenArchiveMember(context.Background(), 77, prefix+member)
				if err != nil {
					t.Fatal(err)
				}
				web := make([]byte, len(p))
				n, err = r.ReadAt(web, 0)
				r.(io.Closer).Close()
				if err != nil || n != len(web) || !bytes.Equal(web, p) {
					t.Fatalf("web/kernel mismatch: n=%d err=%v", n, err)
				}
			}
		})
	}
}

type rejectSolidPasswordAPI struct {
	*archiveAPI
	reads int
}

func (a *rejectSolidPasswordAPI) ReadSmallFile(context.Context, int64, int64) ([]byte, error) {
	a.reads++
	return nil, os.ErrPermission
}

func TestPlain7zReadDoesNotDiscoverUnusedPassword(t *testing.T) {
	for _, name := range []string{"inner.7z", "mixed-encryption.7z"} {
		t.Run(name, func(t *testing.T) { testPlain7zReadDoesNotDiscoverUnusedPassword(t, name) })
	}
}

func testPlain7zReadDoesNotDiscoverUnusedPassword(t *testing.T, name string) {
	data, err := os.ReadFile("testdata/solid7z/" + name)
	if err != nil {
		t.Fatal(err)
	}
	root, api := otherArchiveFixture(t, "archive.7z", data, []byte("unused-synthetic-password"), false)
	reject := &rejectSolidPasswordAPI{archiveAPI: api}
	root.tree.api = reject
	if err := root.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	service := NewServiceFromNode(root)
	r, _, err := service.OpenArchiveMember(context.Background(), 77, "member-a.bin")
	if err != nil {
		t.Fatalf("plain body must not depend on password sidecar: %v", err)
	}
	defer r.(io.Closer).Close()
	p := make([]byte, 64<<10)
	if n, e := r.ReadAt(p, 0); e != nil || n != len(p) || !bytes.Equal(p, solid7zReusePayload(0)[:len(p)]) {
		t.Fatalf("plain head: n=%d err=%v", n, e)
	}
	if reject.reads != 0 {
		t.Fatalf("unused password reads=%d", reject.reads)
	}
}

func TestRoot7zWarmOpenSkipsHeaderReads(t *testing.T) {
	for _, name := range []string{"inner.7z", "encrypted.7z", "mixed-encryption.7z"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile("testdata/solid7z/" + name)
			if err != nil {
				t.Fatal(err)
			}
			root, _ := otherArchiveFixture(t, "archive.7z", data, []byte("synthetic-solid-password"), false)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := root.Prepare(ctx); err != nil {
				t.Fatal(err)
			}
			service := NewServiceFromNode(root)
			// B belongs to the encrypted group in the mixed fixture. Closing
			// each handle makes the next open construct a fresh root view.
			for i := 0; i < 3; i++ {
				before := root.tree.cache.Stats().Foreground.ReadRequests
				r, size, err := service.OpenArchiveMember(ctx, 77, "/member-b.bin/")
				if err != nil || size != 4194317 {
					t.Fatalf("open %d: size=%d err=%v", i, size, err)
				}
				after := root.tree.cache.Stats().Foreground.ReadRequests
				if i > 0 && after != before {
					t.Fatalf("warm open reparsed source: range reads %d -> %d", before, after)
				}
				p := make([]byte, 64<<10)
				n, err := r.ReadAt(p, 0)
				r.(io.Closer).Close()
				if err != nil || n != len(p) || !bytes.Equal(p, solid7zReusePayload(1)[:len(p)]) {
					t.Fatalf("head %d: n=%d err=%v", i, n, err)
				}
			}
		})
	}
}

func TestRootSolid7zSplitColdSourceSingleSlot(t *testing.T) {
	for _, stage := range []string{"password-fact", "root-plan"} {
		t.Run(stage, func(t *testing.T) {
			f := newRootSolid7zSplitFixture(t)
			f.run(t, stage, func(ctx context.Context) error {
				if stage == "password-fact" {
					if f.idx.pathNeedsPassword("member-b.bin") {
						return fmt.Errorf("plain group incorrectly classified as encrypted")
					}
					return nil
				}
				plan, err := f.tree.rootSolid7zPlan(ctx, f.view, f.idx.members["member-b.bin"], nil)
				if err == nil && plan == nil {
					return fmt.Errorf("supported split LZMA2 group lost demand admission")
				}
				return err
			})
		})
	}
}
