package mountfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type archiveAPI struct {
	url      string
	files    []panapi.File
	password []byte
}

func (a *archiveAPI) List(context.Context, int64) ([]panapi.File, error) { return a.files, nil }
func (a *archiveAPI) DownloadURL(_ context.Context, id int64) (string, error) {
	return fmt.Sprintf("%s/%d", a.url, id), nil
}
func (a *archiveAPI) ReadSmallFile(context.Context, int64, int64) ([]byte, error) {
	return append([]byte(nil), a.password...), nil
}

func otherArchiveFixture(t *testing.T, name string, data, password []byte, split bool) (*Node, *archiveAPI) {
	t.Helper()
	parts := map[string][]byte{}
	api := &archiveAPI{password: password}
	if split {
		for i, off := 0, 0; off < len(data); i++ {
			end := off + 100
			if end > len(data) {
				end = len(data)
			}
			id := int64(77 + i)
			parts[fmt.Sprintf("/%d", id)] = data[off:end]
			api.files = append(api.files, panapi.File{ID: id, Name: fmt.Sprintf("private.7z.%03d", i+1), Size: int64(end - off), Version: "v1"})
			off = end
		}
	} else {
		parts["/77"] = data
		api.files = []panapi.File{{ID: 77, Name: name, Size: int64(len(data)), Version: "v1"}}
	}
	if len(password) > 0 {
		pwd := name + ".pwd"
		if split {
			pwd = "private.7z.pwd"
		}
		api.files = append(api.files, panapi.File{ID: 9999, Name: pwd, Size: int64(len(password)), Version: "p1"})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, ok := parts[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("ETag", `"fixture"`)
		http.ServeContent(w, r, "archive", time.Unix(100, 0), bytes.NewReader(data))
	}))
	t.Cleanup(server.Close)
	api.url = server.URL
	cache, err := storage.NewCache(t.TempDir(), 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cache.Close() })
	root := New(context.Background(), api, cache, 0, true)
	fs.NewNodeFS(root, &fs.Options{})
	return root, api
}

func Test7zPasswordDirectoryAndReads(t *testing.T) {
	for _, fixture := range []string{"plain.7z", "password.7z", "headers.7z", "solid.7z", "copy.7z"} {
		for _, split := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/split=%v", fixture, split), func(t *testing.T) {
				data, err := os.ReadFile(filepath.Join("testdata/encrypted", fixture))
				if err != nil {
					t.Fatal(err)
				}
				password := []byte("mount-test-password")
				if fixture == "plain.7z" {
					password = nil
				}
				name := "private.7z"
				if split {
					name += ".001"
				}
				root, _ := otherArchiveFixture(t, name, data, password, split)
				archive := lookup(t, root, name)
				dir := lookup(t, archive, "folder")
				node := lookup(t, dir, "hello.txt")
				readOther(t, node, bytes.Repeat([]byte("mount 7z fixture contents\n"), 200))
				if fixture == "solid.7z" {
					readOther(t, lookup(t, archive, "last.txt"), []byte("last solid member\n"))
				}
			})
		}
	}
}
func readOther(t *testing.T, node *Node, want []byte) {
	t.Helper()
	for i := 0; i < 2; i++ {
		h, _, errno := node.Open(context.Background(), syscall.O_RDONLY)
		if errno != 0 {
			t.Fatalf("open: %v", errno)
		}
		result, errno := h.(fs.FileReader).Read(context.Background(), make([]byte, len(want)), 0)
		if errno != 0 {
			t.Fatal(errno)
		}
		got, status := result.Bytes(nil)
		if status != fuse.OK || !bytes.Equal(got, want) {
			t.Fatalf("read mismatch (%d bytes)", len(got))
		}
		h.(fs.FileReleaser).Release(context.Background())
	}
}
func Test7zWrongOrMissingPasswordNeverPublishes(t *testing.T) {
	for _, fixture := range []string{"password.7z", "headers.7z", "copy.7z"} {
		for _, password := range []string{"", "wrong"} {
			t.Run(fixture+password, func(t *testing.T) {
				data, err := os.ReadFile(filepath.Join("testdata/encrypted", fixture))
				if err != nil {
					t.Fatal(err)
				}
				root, _ := otherArchiveFixture(t, "private.7z", data, []byte(password), false)
				archive := lookup(t, root, "private.7z")
				_, err = archive.list(context.Background())
				if err == nil {
					node := lookup(t, lookup(t, archive, "folder"), "hello.txt")
					h, _, errno := node.Open(context.Background(), syscall.O_RDONLY)
					if errno != syscall.EACCES {
						if h != nil {
							h.(fs.FileReleaser).Release(context.Background())
						}
						t.Fatalf("wrong password open = %v", errno)
					}
				} else if toErrno(err) != syscall.EACCES {
					t.Fatalf("wrong password list = %v", err)
				}
			})
		}
	}
}
func TestRARPasswordAndSolidRead(t *testing.T) {
	for _, fixture := range []string{"rar5-psw.rar", "rar5-hpsw.rar", "rar3-solid.rar", "rar5-crc.rar"} {
		t.Run(fixture, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("../../../tests/fixtures/rar", fixture))
			if err != nil {
				t.Fatal(err)
			}
			password := []byte("password")
			if !strings.Contains(fixture, "psw") {
				password = nil
			}
			root, _ := otherArchiveFixture(t, "private.rar", data, password, false)
			archive := lookup(t, root, "private.rar")
			entries, err := archive.list(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			var walk func(*Node, map[string]*entry)
			walk = func(parent *Node, entries map[string]*entry) {
				for name, e := range entries {
					node := lookup(t, parent, name)
					if e.directory {
						children, err := node.list(context.Background())
						if err != nil {
							t.Fatal(err)
						}
						walk(node, children)
						continue
					}
					h, _, errno := node.Open(context.Background(), syscall.O_RDONLY)
					if errno != 0 {
						t.Fatalf("%s open: %v", name, errno)
					}
					result, errno := h.(fs.FileReader).Read(context.Background(), make([]byte, e.member.size), 0)
					if errno != 0 {
						t.Fatal(errno)
					}
					got, status := result.Bytes(nil)
					if status != fuse.OK || len(got) != int(e.member.size) {
						t.Fatal("RAR size mismatch")
					}
					if fmt.Sprintf("%x", sha256.Sum256(got)) != "2eaebb4c18cdef7f20089f8a2fa3475bc59c2a193f66e2f1513609a4bef13e22" {
						t.Fatal("RAR plaintext hash mismatch")
					}
					h.(fs.FileReleaser).Release(context.Background())
					count++
				}
			}
			walk(archive, entries)
			if count == 0 {
				t.Fatal("no RAR members")
			}
		})
	}
}

func TestRARWrongAndMissingPasswords(t *testing.T) {
	for _, fixture := range []string{"rar5-psw.rar", "rar5-hpsw.rar"} {
		for _, password := range []string{"", "wrong"} {
			t.Run(fixture+password, func(t *testing.T) {
				data, err := os.ReadFile(filepath.Join("../../../tests/fixtures/rar", fixture))
				if err != nil {
					t.Fatal(err)
				}
				root, _ := otherArchiveFixture(t, "private.rar", data, []byte(password), false)
				archive := lookup(t, root, "private.rar")
				_, err = archive.list(context.Background())
				if err == nil {
					node := lookup(t, archive, "stest1.txt")
					h, _, errno := node.Open(context.Background(), syscall.O_RDONLY)
					if h != nil {
						h.(fs.FileReleaser).Release(context.Background())
					}
					if errno != syscall.EACCES {
						t.Fatalf("wrong RAR password open: %v", errno)
					}
				} else if toErrno(err) != syscall.EACCES {
					t.Fatalf("wrong RAR password list: %v", err)
				}
			})
		}
	}
}

func TestArchiveCachedPlaintextRequiresCurrentPassword(t *testing.T) {
	data, err := os.ReadFile("testdata/encrypted/password.7z")
	if err != nil {
		t.Fatal(err)
	}
	root, api := otherArchiveFixture(t, "private.7z", data, []byte("mount-test-password"), false)
	archive := lookup(t, root, "private.7z")
	node := lookup(t, lookup(t, archive, "folder"), "hello.txt")
	readOther(t, node, bytes.Repeat([]byte("mount 7z fixture contents\n"), 200))
	api.password = []byte("wrong")
	// Model expiry of the password metadata TTL while the inode and disk cache survive.
	root.tree.mu.Lock()
	for key, item := range root.tree.meta {
		if strings.HasPrefix(key, "password:") {
			delete(root.tree.meta, key)
			root.tree.metaBytes -= item.bytes
		}
	}
	root.tree.mu.Unlock()
	h, _, errno := node.Open(context.Background(), syscall.O_RDONLY)
	if h != nil {
		h.(fs.FileReleaser).Release(context.Background())
	}
	if errno != syscall.EACCES {
		t.Fatalf("cached member bypassed password check: %v", errno)
	}
}

func TestActualFUSE7zAndRAR(t *testing.T) {
	if os.Getenv("MOUNT123_FUSE_TEST") != "1" {
		t.Skip("requires FUSE")
	}
	for _, tc := range []struct {
		fixture, name, member, password string
		split                           bool
	}{
		{"testdata/encrypted/solid.7z", "private.7z", "last.txt", "mount-test-password", false},
		{"testdata/encrypted/headers.7z", "private.7z.001", "folder/hello.txt", "mount-test-password", true},
		{"../../../tests/fixtures/rar/rar5-hpsw.rar", "private.rar", "stest1.txt", "password", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := os.ReadFile(tc.fixture)
			if err != nil {
				t.Fatal(err)
			}
			root, _ := otherArchiveFixture(t, tc.name, data, []byte(tc.password), tc.split)
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
			got, err := os.ReadFile(filepath.Join(point, tc.name, tc.member))
			if err != nil {
				t.Fatal(err)
			}
			if len(got) == 0 {
				t.Fatal("empty read")
			}
			if err := os.WriteFile(filepath.Join(point, tc.name, "new.txt"), []byte("x"), 0600); err == nil {
				t.Fatal("writable mount")
			}
		})
	}
}

func TestArchiveLimitsAndMissingVolume(t *testing.T) {
	data, err := os.ReadFile("testdata/encrypted/solid.7z")
	if err != nil {
		t.Fatal(err)
	}
	t.Run("entries", func(t *testing.T) {
		root, _ := otherArchiveFixture(t, "private.7z", data, []byte("mount-test-password"), false)
		root.tree.opts.MaxZIPEntries = 1
		archive := lookup(t, root, "private.7z")
		if _, err := archive.list(context.Background()); toErrno(err) != syscall.EFBIG {
			t.Fatalf("entry limit: %v", err)
		}
	})
	t.Run("missing-volume", func(t *testing.T) {
		root, api := otherArchiveFixture(t, "private.7z.001", data, []byte("mount-test-password"), true)
		api.files = append(api.files[:1], api.files[2:]...)
		archive := lookup(t, root, "private.7z.001")
		if _, err := archive.list(context.Background()); err == nil {
			t.Fatal("accepted missing volume")
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		root, _ := otherArchiveFixture(t, "private.7z", data, []byte("mount-test-password"), false)
		archive := lookup(t, root, "private.7z")
		node := lookup(t, lookup(t, archive, "folder"), "hello.txt")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		h, _, errno := node.Open(ctx, syscall.O_RDONLY)
		if h != nil {
			h.(fs.FileReleaser).Release(context.Background())
		}
		if errno != syscall.EINTR {
			t.Fatalf("cancelled open: %v", errno)
		}
	})
}

func Test7zCorruptionIsNotPublished(t *testing.T) {
	data, err := os.ReadFile("testdata/encrypted/copy.7z")
	if err != nil {
		t.Fatal(err)
	}
	data[32] ^= 1
	root, _ := otherArchiveFixture(t, "private.7z", data, []byte("mount-test-password"), false)
	archive := lookup(t, root, "private.7z")
	node := lookup(t, lookup(t, archive, "folder"), "hello.txt")
	for i := 0; i < 2; i++ {
		h, _, errno := node.Open(context.Background(), syscall.O_RDONLY)
		if h != nil {
			h.(fs.FileReleaser).Release(context.Background())
		}
		if errno != syscall.EACCES {
			t.Fatalf("corrupt ciphertext open: %v", errno)
		}
	}
}

func TestRARLocatorDoesNotRetainOldPassword(t *testing.T) {
	for _, fixture := range []string{"rar5-psw.rar", "rar5-hpsw.rar"} {
		t.Run(fixture, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("../../../tests/fixtures/rar", fixture))
			if err != nil {
				t.Fatal(err)
			}
			root, api := otherArchiveFixture(t, "private.rar", data, []byte("password"), false)
			archive := lookup(t, root, "private.rar")
			if _, err := archive.list(context.Background()); err != nil {
				t.Fatal(err)
			}
			node := lookup(t, archive, "stest1.txt")
			h, _, errno := node.Open(context.Background(), syscall.O_RDONLY)
			if errno != 0 {
				t.Fatal(errno)
			}
			h.(fs.FileReleaser).Release(context.Background())
			api.password = []byte("wrong")
			root.tree.mu.Lock()
			for key, item := range root.tree.meta {
				if strings.HasPrefix(key, "password:") {
					delete(root.tree.meta, key)
					root.tree.metaBytes -= item.bytes
				}
			}
			root.tree.mu.Unlock()
			h, _, errno = node.Open(context.Background(), syscall.O_RDONLY)
			if h != nil {
				h.(fs.FileReleaser).Release(context.Background())
			}
			if errno != syscall.EACCES {
				t.Fatalf("old locator bypassed changed password: %v", errno)
			}
		})
	}
}
