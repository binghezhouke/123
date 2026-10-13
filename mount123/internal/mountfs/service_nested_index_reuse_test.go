package mountfs

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/nwaples/rardecode/v2"
)

const nestedIndexReuseEntries = 4000

// A large central directory makes repeated parsing measurable independently
// of HTTP downloads. Every member is tiny and stored, so payload decompression
// and a large member cache are not prerequisites for this regression.
func nestedIndexReuseFixture(t *testing.T, layout string) ([]byte, string) {
	t.Helper()
	tool, err := exec.LookPath("7z")
	if err != nil {
		t.Skip("7z fixture generator unavailable")
	}
	var encoded bytes.Buffer
	writer := zip.NewWriter(&encoded)
	for i := 0; i < nestedIndexReuseEntries; i++ {
		name := fmt.Sprintf("entry-%05d-synthetic-metadata.txt", i)
		w, err := writer.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = w.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write := func(name string, body []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	pack := func(name string, members ...string) {
		t.Helper()
		args := append([]string{"a", "-t7z", "-m0=Copy", "-ms=off", "-mhe=on", "-p" + serviceAESCopyPassword, name}, members...)
		cmd := exec.Command(tool, args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("generate synthetic index fixture: %v: %s", err, out)
		}
		verify := exec.Command(tool, "l", "-slt", "-p"+serviceAESCopyPassword, name)
		verify.Dir = dir
		out, err := verify.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "Method = Copy 7zAES") {
			t.Fatalf("fixture must contain independent encrypted Copy members: %v", err)
		}
	}
	path := "inner.zip"
	switch layout {
	case "single":
		write("inner.zip", encoded.Bytes())
		pack("outer.7z", "inner.zip")
	case "two-ancestors":
		write("inner.zip", encoded.Bytes())
		pack("middle.7z", "inner.zip")
		pack("outer.7z", "middle.7z")
		path = "middle.7z/inner.zip"
	case "byte-split":
		body := encoded.Bytes()
		mid := len(body) / 2
		write("inner.zip.001", body[:mid])
		write("inner.zip.002", body[mid:])
		pack("outer.7z", "inner.zip.001", "inner.zip.002")
		path = "inner.zip.001"
	default:
		t.Fatalf("unknown fixture layout %q", layout)
	}
	data, err := os.ReadFile(filepath.Join(dir, "outer.7z"))
	if err != nil {
		t.Fatal(err)
	}
	return data, path
}

func TestServiceNestedIndexReuseAvoidsWarmSourceReads(t *testing.T) {
	for _, layout := range []string{"single", "two-ancestors", "byte-split"} {
		t.Run(layout, func(t *testing.T) {
			data, path := nestedIndexReuseFixture(t, layout)
			var wire atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", `"synthetic-nested-index-reuse"`)
				http.ServeContent(serviceAESCopyCountWriter{w, &wire}, r, "outer.7z", time.Unix(100, 0), bytes.NewReader(data))
			}))
			defer server.Close()
			cacheDir := t.TempDir()
			cache, err := storage.NewCache(cacheDir, 32<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cache.Close() }()
			api := &serviceAESCopyAPI{url: server.URL, archive: data, password: serviceAESCopyPassword}
			opts := Options{DisableReadAhead: true, DisableStreamMembers: true, MaxConcurrentBuilds: 1}
			openService := func() *Service {
				t.Helper()
				ctx, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)
				s, err := NewService(ctx, api, cache, 0, true, opts)
				if err != nil {
					t.Fatal(err)
				}
				return s
			}
			service := openService()
			list := func(label string, warm bool, want []ArchiveEntry) []ArchiveEntry {
				t.Helper()
				// Each call is an independent Web-style request. The previous
				// request has ended before any cached metadata can be reused.
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				before := cache.Stats()
				httpBefore, started := wire.Load(), time.Now()
				rows, err := service.ListArchive(ctx, 77, path)
				elapsed := time.Since(started)
				if err != nil || len(rows) != nestedIndexReuseEntries {
					t.Fatalf("%s listing count=%d error=%v", label, len(rows), err)
				}
				if want != nil && !reflect.DeepEqual(rows, want) {
					t.Fatalf("%s listing changed across requests", label)
				}
				after := cache.Stats()
				requested := after.Foreground.RequestedBytes - before.Foreground.RequestedBytes
				httpBytes := wire.Load() - httpBefore
				fills := after.FillSuccesses - before.FillSuccesses
				t.Logf("%s source_requested=%d HTTP_body=%d source_fills=%d elapsed_us=%d", label, requested, httpBytes, fills, elapsed.Microseconds())
				if warm {
					if requested >= 128<<10 {
						t.Errorf("%s requested %d source bytes, want <128 KiB for a reused nested index", label, requested)
					}
					if httpBytes != 0 || fills != 0 {
						t.Errorf("%s warmed source unexpectedly downloaded=%d bytes or filled=%d ranges", label, httpBytes, fills)
					}
				}
				return rows
			}
			// Seed the complete inner index and the source ranges it needed.
			// This phase is setup, not a claim about cold performance.
			want := list("seed", false, nil)
			for i, row := range want {
				if row.Name != fmt.Sprintf("entry-%05d-synthetic-metadata.txt", i) || row.Size != 1 || row.IsDir {
					t.Fatalf("unexpected synthetic entry at %d", i)
				}
			}
			list("same_tree", true, want)
			if err = cache.Close(); err != nil {
				t.Fatal(err)
			}
			cache, err = storage.NewCache(cacheDir, 32<<20)
			if err != nil {
				t.Fatal(err)
			}
			service = openService()
			list("restored_tree", true, want)
		})
	}
}

func nestedIndexReuseBoundarySource(t *testing.T, data []byte) (*Service, *Node, *storage.Cache, *serviceAESCopyAPI) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"synthetic-nested-index-boundary"`)
		http.ServeContent(w, r, "outer.7z", time.Unix(100, 0), bytes.NewReader(data))
	}))
	t.Cleanup(server.Close)
	cache, err := storage.NewCache(t.TempDir(), 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	api := &serviceAESCopyAPI{url: server.URL, archive: data, password: serviceAESCopyPassword}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	root := NewWithOptions(ctx, api, cache, 0, true, Options{DisableReadAhead: true, DisableStreamMembers: true, MaxConcurrentBuilds: 1})
	if err := root.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	return NewServiceFromNode(root), root, cache, api
}

func nestedIndexReuseRead(ctx context.Context, service *Service, path string) error {
	reader, size, err := service.OpenArchiveMember(ctx, 77, path+"/entry-00000-synthetic-metadata.txt")
	if err != nil {
		return err
	}
	if closer, ok := reader.(io.Closer); ok {
		defer closer.Close()
	}
	got := make([]byte, 1)
	if n, err := reader.ReadAt(got, 0); size != 1 || n != 1 || err != nil || string(got) != "x" {
		return fmt.Errorf("synthetic payload size=%d n=%d error=%v", size, n, err)
	}
	return nil
}

func TestServiceNestedIndexReuseRequestLifetimesAndContents(t *testing.T) {
	for _, layout := range []string{"single", "two-ancestors", "byte-split"} {
		t.Run(layout, func(t *testing.T) {
			data, path := nestedIndexReuseFixture(t, layout)
			service, _, cache, api := nestedIndexReuseBoundarySource(t, data)
			seedCtx, endSeed := context.WithCancel(context.Background())
			want, err := service.ListArchive(seedCtx, 77, path)
			endSeed()
			if err != nil || len(want) != nestedIndexReuseEntries {
				t.Fatalf("seed count=%d error=%v", len(want), err)
			}
			if err := nestedIndexReuseRead(context.Background(), service, path); err != nil {
				t.Fatalf("new request after seed ended: %v", err)
			}
			if _, err := service.ListArchive(seedCtx, 77, path); !errors.Is(err, context.Canceled) {
				t.Fatalf("pre-canceled cached listing error=%v", err)
			}
			// Independent requests must not share readers, crypto state or a
			// completed request's context through the cached index.
			errs := make(chan error, 8)
			for i := 0; i < cap(errs); i++ {
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					rows, err := service.ListArchive(ctx, 77, path)
					if err == nil && !reflect.DeepEqual(rows, want) {
						err = fmt.Errorf("concurrent listing changed")
					}
					if err == nil {
						err = nestedIndexReuseRead(ctx, service, path)
					}
					errs <- err
				}()
			}
			for i := 0; i < cap(errs); i++ {
				if err := <-errs; err != nil {
					t.Fatal(err)
				}
			}
			if err := cache.Close(); err != nil {
				t.Fatal(err)
			}
			restored, err := storage.NewCache(cache.Directory(), cache.Capacity())
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			service, err = NewService(context.Background(), api, restored, 0, true, Options{DisableReadAhead: true, DisableStreamMembers: true})
			if err != nil {
				t.Fatal(err)
			}
			rows, err := service.ListArchive(context.Background(), 77, path)
			if err != nil || !reflect.DeepEqual(rows, want) {
				t.Fatalf("restored listing count=%d error=%v", len(rows), err)
			}
			if err := nestedIndexReuseRead(context.Background(), service, path); err != nil {
				t.Fatalf("restored payload: %v", err)
			}
		})
	}
}

func TestServiceNestedIndexReusePasswordRefreshCannotBypassAuthentication(t *testing.T) {
	data, path := nestedIndexReuseFixture(t, "single")
	service, root, _, api := nestedIndexReuseBoundarySource(t, data)
	ctx := context.Background()
	if rows, err := service.ListArchive(ctx, 77, path); err != nil || len(rows) != nestedIndexReuseEntries {
		t.Fatalf("seed count=%d error=%v", len(rows), err)
	}
	// Keep sidecar ID, version and size unchanged. An explicit directory
	// refresh must still select the updated value and miss the old index.
	api.password = strings.Repeat("!", len(serviceAESCopyPassword))
	if _, err := root.RefreshDirectory(ctx, "."); err != nil {
		t.Fatal(err)
	}
	if rows, err := service.ListArchive(ctx, 77, path); !errors.Is(err, syscall.EACCES) {
		t.Fatalf("changed password listing count=%d error=%v, want EACCES", len(rows), err)
	}
	if err := nestedIndexReuseRead(ctx, service, path); !errors.Is(err, syscall.EACCES) {
		t.Fatalf("changed password payload error=%v, want EACCES", err)
	}
	api.password = serviceAESCopyPassword
	if _, err := root.RefreshDirectory(ctx, "."); err != nil {
		t.Fatal(err)
	}
	if rows, err := service.ListArchive(ctx, 77, path); err != nil || len(rows) != nestedIndexReuseEntries {
		t.Fatalf("corrected password count=%d error=%v", len(rows), err)
	}
	if err := nestedIndexReuseRead(ctx, service, path); err != nil {
		t.Fatal(err)
	}
}

func TestServiceNestedIndexReuseFailedSourceCanRetry(t *testing.T) {
	var outer bytes.Buffer
	writer := zip.NewWriter(&outer)
	inner := rarSparseStoredZIP(t, "payload.bin", []byte("synthetic retry payload"))
	for _, member := range []struct {
		name string
		body []byte
	}{{"inner.zip", inner}, {"padding.bin", bytes.Repeat([]byte("p"), 4<<20)}} {
		w, err := writer.CreateHeader(&zip.FileHeader{Name: member.name, Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(member.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	var deny atomic.Bool
	var failures atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if deny.Load() {
			failures.Add(1)
			http.Error(w, "synthetic source temporarily unavailable", http.StatusForbidden)
			return
		}
		w.Header().Set("ETag", `"synthetic-nested-index-retry"`)
		http.ServeContent(w, r, "outer.zip", time.Unix(100, 0), bytes.NewReader(outer.Bytes()))
	}))
	defer server.Close()
	cache, err := storage.NewCache(t.TempDir(), 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	api := &warmArchiveRestartAPI{file: panapi.File{ID: 77, Name: "outer.zip", Size: int64(outer.Len()), Version: "fixture-v1"}, url: server.URL}
	service, err := NewService(context.Background(), api, cache, 0, true, Options{DisableReadAhead: true, DisableStreamMembers: true})
	if err != nil {
		t.Fatal(err)
	}
	if rows, err := service.ListArchive(context.Background(), 77, ""); err != nil || len(rows) != 2 {
		t.Fatalf("outer seed count=%d error=%v", len(rows), err)
	}
	deny.Store(true)
	if rows, err := service.ListArchive(context.Background(), 77, "inner.zip"); err == nil || len(rows) != 0 {
		t.Fatalf("failed nested listing count=%d error=%v", len(rows), err)
	}
	if failures.Load() == 0 {
		t.Fatal("failure phase did not reach the source")
	}
	deny.Store(false)
	rows, err := service.ListArchive(context.Background(), 77, "inner.zip")
	if err != nil || len(rows) != 1 || rows[0].Name != "payload.bin" {
		t.Fatalf("retry count=%d error=%v", len(rows), err)
	}
	reader, size, err := service.OpenArchiveMember(context.Background(), 77, "inner.zip/payload.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.(io.Closer).Close()
	got := make([]byte, size)
	if _, err := reader.ReadAt(got, 0); err != nil || string(got) != "synthetic retry payload" {
		t.Fatalf("retry payload error=%v", err)
	}
}

func TestServiceNestedIndexReuseCanRebuildAfterCapacityEviction(t *testing.T) {
	data, path := nestedIndexReuseFixture(t, "single")
	service, _, cache, api := nestedIndexReuseBoundarySource(t, data)
	ctx := context.Background()
	want, err := service.ListArchive(ctx, 77, path)
	if err != nil || len(want) != nestedIndexReuseEntries {
		t.Fatalf("seed count=%d error=%v", len(want), err)
	}
	before := cache.Stats().CapacityEvictions
	if err := cache.Store(ctx, "synthetic-cache-pressure", make([]byte, cache.Capacity())); err != nil {
		t.Fatal(err)
	}
	if cache.Stats().CapacityEvictions <= before {
		t.Fatal("capacity pressure did not evict cached objects")
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := storage.NewCache(cache.Directory(), cache.Capacity())
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	service, err = NewService(ctx, api, restored, 0, true, Options{DisableReadAhead: true, DisableStreamMembers: true})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := service.ListArchive(ctx, 77, path)
	if err != nil || !reflect.DeepEqual(rows, want) {
		t.Fatalf("rebuilt count=%d error=%v", len(rows), err)
	}
	if err := nestedIndexReuseRead(ctx, service, path); err != nil {
		t.Fatal(err)
	}
}

func TestServiceNestedIndexReuseBodyEncryptedRARKeepsPasswordDiscovery(t *testing.T) {
	data, err := os.ReadFile("../../../tests/fixtures/rar/rar5-psw.rar")
	if err != nil {
		t.Fatal(err)
	}
	// The ordinary library reader independently selects an encrypted member
	// and provides its bytes. The Service listing is seeded without opening it.
	oracle, err := rardecode.NewReader(bytes.NewReader(data), rardecode.Password("password"))
	if err != nil {
		t.Fatal(err)
	}
	var name string
	var want []byte
	for {
		h, err := oracle.Next()
		if err != nil {
			t.Fatal(err)
		}
		if h.Encrypted && !h.IsDir && h.UnPackedSize > 0 {
			name = h.Name
			want, err = io.ReadAll(oracle)
			if err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	outer := rarSparseStoredZIP(t, "inner.rar", data)
	root, api := otherArchiveFixture(t, "outer.zip", outer, []byte("password"), false)
	if err := root.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	service := NewServiceFromNode(root)
	seed, endSeed := context.WithCancel(context.Background())
	rows, err := service.ListArchive(seed, 77, "inner.rar")
	endSeed()
	if err != nil || len(rows) == 0 {
		t.Fatalf("clear inner RAR listing count=%d error=%v", len(rows), err)
	}
	for phase := 0; phase < 2; phase++ {
		rows, err := service.ListArchive(context.Background(), 77, "inner.rar")
		if err != nil || len(rows) == 0 {
			t.Fatalf("reused RAR listing phase=%d count=%d error=%v", phase, len(rows), err)
		}
		multilevelRead(t, service, "inner.rar/"+name, want)
	}
	cache := service.tree.cache
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := storage.NewCache(cache.Directory(), cache.Capacity())
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	service, err = NewService(context.Background(), api, restored, 0, true, Options{DisableReadAhead: true, DisableStreamMembers: true})
	if err != nil {
		t.Fatal(err)
	}
	rows, err = service.ListArchive(context.Background(), 77, "inner.rar")
	if err != nil || len(rows) == 0 {
		t.Fatalf("restored clear RAR listing count=%d error=%v", len(rows), err)
	}
	multilevelRead(t, service, "inner.rar/"+name, want)
}

func TestServiceNestedIndexReuseInnerEncryptedHeadersUseCurrentPassword(t *testing.T) {
	data, err := os.ReadFile("testdata/encrypted/headers.7z")
	if err != nil {
		t.Fatal(err)
	}
	outer := rarSparseStoredZIP(t, "inner.7z", data)
	root, api := otherArchiveFixture(t, "outer.zip", outer, []byte("mount-test-password"), false)
	ctx := context.Background()
	if err := root.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	service := NewServiceFromNode(root)
	rows, err := service.ListArchive(ctx, 77, "inner.7z")
	if err != nil || len(rows) == 0 {
		t.Fatalf("seed inner encrypted header listing count=%d error=%v", len(rows), err)
	}
	want := bytes.Repeat([]byte("mount 7z fixture contents\n"), 200)
	multilevelRead(t, service, "inner.7z/folder/hello.txt", want)
	api.password = bytes.Repeat([]byte("!"), len(api.password))
	if _, err := root.RefreshDirectory(ctx, "."); err != nil {
		t.Fatal(err)
	}
	assertDenied := func() {
		t.Helper()
		if rows, err := service.ListArchive(ctx, 77, "inner.7z"); !errors.Is(err, syscall.EACCES) {
			t.Fatalf("wrong inner-header password listing count=%d error=%v, want EACCES", len(rows), err)
		}
		reader, _, err := service.OpenArchiveMember(ctx, 77, "inner.7z/folder/hello.txt")
		if reader != nil {
			_ = reader.(io.Closer).Close()
		}
		if !errors.Is(err, syscall.EACCES) {
			t.Fatalf("wrong inner-header password open error=%v, want EACCES", err)
		}
	}
	assertDenied()
	cache := service.tree.cache
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := storage.NewCache(cache.Directory(), cache.Capacity())
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	root = NewWithOptions(ctx, api, restored, 0, true, Options{DisableReadAhead: true, DisableStreamMembers: true})
	if err := root.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	service = NewServiceFromNode(root)
	assertDenied()
	api.password = []byte("mount-test-password")
	if _, err := root.RefreshDirectory(ctx, "."); err != nil {
		t.Fatal(err)
	}
	rows, err = service.ListArchive(ctx, 77, "inner.7z")
	if err != nil || len(rows) == 0 {
		t.Fatalf("corrected inner header listing count=%d error=%v", len(rows), err)
	}
	multilevelRead(t, service, "inner.7z/folder/hello.txt", want)
}
