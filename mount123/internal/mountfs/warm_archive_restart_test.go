package mountfs

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type warmArchiveRestartAPI struct {
	file     panapi.File
	files    []panapi.File
	url      string
	deny     bool
	infos    atomic.Int32
	details  atomic.Int32
	resolves atomic.Int32
}

func TestWarmImmutableRenamedAndSplitArchiveRestart(t *testing.T) {
	for _, layout := range []string{"renamed", "renamed-size-version", "byte-split"} {
		for _, frontend := range []string{"service", "fuse"} {
			t.Run(layout+"/"+frontend, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				want := bytes.Repeat([]byte("synthetic stored nested payload\n"), 100)
				storedZIP := func(name string, body []byte) []byte {
					var buf bytes.Buffer
					writer := zip.NewWriter(&buf)
					w, err := writer.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
					if err != nil {
						t.Fatal(err)
					}
					if _, err = w.Write(body); err != nil {
						t.Fatal(err)
					}
					if err = writer.Close(); err != nil {
						t.Fatal(err)
					}
					return buf.Bytes()
				}
				data := storedZIP("inner.zip", storedZIP("payload.txt", want))
				files := []panapi.File{{ID: 77, Name: "sample.data", Size: int64(len(data))}}
				if layout == "renamed-size-version" {
					files[0].Version = fmt.Sprintf(":%d", len(data))
				}
				bodies := map[int64][]byte{77: data}
				entryName := "sample.data.zip"
				if layout == "byte-split" {
					mid := len(data) / 2
					files = []panapi.File{{ID: 77, Name: "sample.zip.001", Size: int64(mid)}, {ID: 78, Name: "sample.zip.002", Size: int64(len(data) - mid)}}
					bodies = map[int64][]byte{77: data[:mid], 78: data[mid:]}
					entryName = files[0].Name
				}
				var requests atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					var id, start, end int64
					if _, err := fmt.Sscanf(r.URL.Path, "/%d", &id); err != nil {
						http.Error(w, "bad ID", 400)
						return
					}
					body := bodies[id]
					if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil || start < 0 || end < start || end >= int64(len(body)) {
						http.Error(w, "bad range", 400)
						return
					}
					w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
					w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
					w.WriteHeader(http.StatusPartialContent)
					_, _ = w.Write(body[start : end+1])
				}))
				defer server.Close()
				dir := t.TempDir()
				cache, err := storage.NewCache(dir, 32<<20)
				if err != nil {
					t.Fatal(err)
				}
				opts := Options{DisableReadAhead: true, DisableStreamMembers: true, DisableArchivePageCache: true}
				api := &warmArchiveRestartAPI{file: files[0], files: files, url: server.URL}
				root := NewWithOptions(ctx, api, cache, 0, true, opts)
				fs.NewNodeFS(root, &fs.Options{})
				service := NewServiceFromNode(root)
				if _, err = service.ListDirectory(ctx, 0); err != nil {
					t.Fatal(err)
				}
				if layout != "byte-split" {
					if _, err = service.ProbeDirectory(ctx, 0); err != nil {
						t.Fatal(err)
					}
					if status := waitProbeStopped(t, root); status.Detected != 1 || status.Persistence != "disk" {
						t.Fatalf("probe: %+v", status)
					}
				}
				if _, err = service.ListArchive(ctx, 77, "inner.zip"); err != nil {
					t.Fatal(err)
				}
				r, size, err := service.OpenArchiveMember(ctx, 77, "inner.zip/payload.txt")
				if err != nil {
					t.Fatal(err)
				}
				got := make([]byte, size)
				_, err = r.ReadAt(got, 0)
				if c, ok := r.(io.Closer); ok {
					_ = c.Close()
				}
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("seed: %v", err)
				}
				if err = cache.Close(); err != nil {
					t.Fatal(err)
				}
				cache, err = storage.NewCache(dir, 32<<20)
				if err != nil {
					t.Fatal(err)
				}
				defer cache.Close()
				for _, file := range files {
					if err = cache.Remove("download-link-v1:" + service.tree.cloudCacheKey(&file)); err != nil {
						t.Fatal(err)
					}
				}
				requests.Store(0)
				api = &warmArchiveRestartAPI{file: files[0], files: files, deny: true}
				root = NewWithOptions(ctx, api, cache, 0, true, opts)
				fs.NewNodeFS(root, &fs.Options{})
				service = NewServiceFromNode(root)
				if frontend == "service" {
					if _, err = service.ListDirectory(ctx, 0); err != nil {
						t.Fatal(err)
					}
					entries, listErr := service.ListArchive(ctx, 77, "inner.zip")
					if listErr != nil || len(entries) != 1 || entries[0].Name != "payload.txt" {
						t.Fatalf("warm list: %v %#v", listErr, entries)
					}
					r, size, err = service.OpenArchiveMember(ctx, 77, "inner.zip/payload.txt")
					if err != nil {
						t.Fatal(err)
					}
					got = make([]byte, size)
					_, err = r.ReadAt(got, 0)
					if c, ok := r.(io.Closer); ok {
						_ = c.Close()
					}
					if err != nil || !bytes.Equal(got, want) {
						t.Fatalf("warm read: %v", err)
					}
				} else {
					leaf := lookup(t, lookup(t, lookup(t, root, entryName), "inner.zip"), "payload.txt")
					if !bytes.Equal(readNode(t, leaf, 0, len(want)), want) {
						t.Fatal("warm FUSE mismatch")
					}
				}
				if api.resolves.Load()+api.details.Load()+api.infos.Load()+requests.Load() != 0 {
					t.Fatalf("warm I/O: URLs=%d Detail=%d Infos=%d HTTP=%d", api.resolves.Load(), api.details.Load(), api.infos.Load(), requests.Load())
				}
			})
		}
	}
}

func (a *warmArchiveRestartAPI) CacheIdentity() string { return "synthetic-warm-archive-account" }
func (a *warmArchiveRestartAPI) List(context.Context, int64) ([]panapi.File, error) {
	if a.files != nil {
		return append([]panapi.File(nil), a.files...), nil
	}
	return []panapi.File{a.file}, nil
}
func (a *warmArchiveRestartAPI) Infos(context.Context, []int64) ([]panapi.File, error) {
	a.infos.Add(1)
	return []panapi.File{a.file}, nil
}
func (a *warmArchiveRestartAPI) Detail(_ context.Context, id int64) (panapi.File, error) {
	a.details.Add(1)
	for _, f := range a.files {
		if f.ID == id {
			return f, nil
		}
	}
	return a.file, nil
}
func (a *warmArchiveRestartAPI) DownloadURL(_ context.Context, id int64) (string, error) {
	a.resolves.Add(1)
	if a.deny {
		return "", fmt.Errorf("synthetic URL resolution denied on warm restart")
	}
	return fmt.Sprintf("%s/%d", a.url, id), nil
}

// Archive contents and IDs are immutable. A CDN without validators must not
// force source initialization before durable index/member hits are consulted.
// Ordinary file safety is intentionally outside this archive-only contract.
func TestWarmImmutableArchiveRestartAvoidsEagerAPIs(t *testing.T) {
	for _, version := range []string{"v1", "", ":size"} {
		for _, frontend := range []string{"service-list", "service-read", "fuse-read"} {
			for _, denyURL := range []bool{false, true} {
				name := fmt.Sprintf("version=%q/%s/deny-url=%t", version, frontend, denyURL)
				t.Run(name, func(t *testing.T) {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					want := bytes.Repeat([]byte("synthetic nested archive payload\n"), 100)
					inner := multilevelFixture(t, "zip", map[string][]byte{"payload.txt": want})
					data := multilevelFixture(t, "zip", map[string][]byte{"inner.zip": inner})
					var downloads atomic.Int32
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						downloads.Add(1)
						var start, end int64
						if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil || start < 0 || end < start || end >= int64(len(data)) {
							http.Error(w, "bad range", http.StatusBadRequest)
							return
						}
						// No ETag or Last-Modified: stable archive identity is known
						// independently of transport validator availability.
						w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
						w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
						w.WriteHeader(http.StatusPartialContent)
						_, _ = w.Write(data[start : end+1])
					}))
					defer server.Close()
					file := panapi.File{ID: 77, Name: "outer.zip", Size: int64(len(data)), Version: version}
					if version == ":size" {
						file.Version = fmt.Sprintf(":%d", len(data))
					}
					dir := t.TempDir()
					opts := Options{DisableReadAhead: true, DisableStreamMembers: true, DisableArchivePageCache: true}
					cache, err := storage.NewCache(dir, 32<<20)
					if err != nil {
						t.Fatal(err)
					}
					seed, err := NewService(ctx, &warmArchiveRestartAPI{file: file, url: server.URL}, cache, 0, true, opts)
					if err != nil {
						t.Fatal(err)
					}
					if _, err = seed.ListDirectory(ctx, 0); err != nil {
						t.Fatal(err)
					}
					if _, err = seed.ListArchive(ctx, file.ID, "inner.zip"); err != nil {
						t.Fatal(err)
					}
					r, size, err := seed.OpenArchiveMember(ctx, file.ID, "inner.zip/payload.txt")
					if err != nil {
						t.Fatal(err)
					}
					got := make([]byte, size)
					_, readErr := r.ReadAt(got, 0)
					if c, ok := r.(io.Closer); ok {
						_ = c.Close()
					}
					if readErr != nil || !bytes.Equal(got, want) {
						t.Fatalf("seed read: %v", readErr)
					}
					if err = cache.Close(); err != nil {
						t.Fatal(err)
					}
					cache, err = storage.NewCache(dir, 32<<20)
					if err != nil {
						t.Fatal(err)
					}
					defer cache.Close()
					// A signed link can expire independently of immutable archive
					// contents. Remove only the saved link to model that condition.
					if err = cache.Remove("download-link-v1:" + seed.tree.cloudCacheKey(&file)); err != nil {
						t.Fatal(err)
					}
					api := &warmArchiveRestartAPI{file: file, url: server.URL, deny: denyURL}
					downloads.Store(0)
					before := cache.Stats()
					switch frontend {
					case "service-list", "service-read":
						service, initErr := NewService(ctx, api, cache, 0, true, opts)
						err = initErr
						if err == nil {
							// Navigation has restored the directory snapshot before
							// entering the immutable archive by its stable file ID.
							_, err = service.ListDirectory(ctx, 0)
						}
						if err == nil && frontend == "service-list" {
							var entries []ArchiveEntry
							entries, err = service.ListArchive(ctx, file.ID, "inner.zip")
							if err == nil && (len(entries) != 1 || entries[0].Name != "payload.txt") {
								t.Fatalf("warm listing: %#v", entries)
							}
						} else if err == nil {
							r, size, err = service.OpenArchiveMember(ctx, file.ID, "inner.zip/payload.txt")
							if err == nil {
								got = make([]byte, size)
								_, err = r.ReadAt(got, 0)
								if c, ok := r.(io.Closer); ok {
									_ = c.Close()
								}
								if err == nil && !bytes.Equal(got, want) {
									t.Fatal("warm read mismatch")
								}
							}
						}
					case "fuse-read":
						root := NewWithOptions(ctx, api, cache, 0, true, opts)
						fs.NewNodeFS(root, &fs.Options{})
						node := root
						for _, part := range []string{"outer.zip", "inner.zip", "payload.txt"} {
							inode, errno := node.Lookup(ctx, part, &fuse.EntryOut{})
							if errno != 0 {
								err = errno
								break
							}
							node = inode.Operations().(*Node)
						}
						if err == nil && !bytes.Equal(readNode(t, node, 0, len(want)), want) {
							t.Fatal("warm FUSE read mismatch")
						}
					}
					if err != nil {
						t.Errorf("warm cached operation failed: %v", err)
					}
					if api.infos.Load()+api.details.Load()+api.resolves.Load() != 0 || downloads.Load() != 0 {
						t.Errorf("eager warm I/O: Infos=%d Detail=%d DownloadURL=%d HTTP=%d", api.infos.Load(), api.details.Load(), api.resolves.Load(), downloads.Load())
					}
					if after := cache.Stats(); after.FillSuccesses != before.FillSuccesses {
						t.Errorf("warm fills=%d", after.FillSuccesses-before.FillSuccesses)
					}
				})
			}
		}
	}
}
