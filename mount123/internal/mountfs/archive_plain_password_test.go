package mountfs

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func TestClear7zMembersIgnoreUnrelatedPasswordFailures(t *testing.T) {
	for _, fixture := range []string{"inner.7z", "mixed-encryption.7z"} {
		t.Run(fixture, func(t *testing.T) { testClear7zMembersIgnoreUnrelatedPasswordFailures(t, fixture) })
	}
}

func testClear7zMembersIgnoreUnrelatedPasswordFailures(t *testing.T, fixture string) {
	data, err := os.ReadFile("testdata/solid7z/" + fixture)
	if err != nil {
		t.Fatal(err)
	}
	adapters := []string{"service", "fuse"}
	if os.Getenv("MOUNT123_FUSE_TEST") == "1" {
		adapters = append(adapters, "kernel")
	}
	for _, parent := range []string{"root", "zip-store", "zip-deflate"} {
		for _, adapter := range adapters {
			t.Run(parent+"/"+adapter, func(t *testing.T) {
				body, name, path := data, "archive.7z", "member-a.bin"
				if parent != "root" {
					method := uint16(zip.Store)
					if parent == "zip-deflate" {
						method = zip.Deflate
					}
					body, name, path = nestedDeflateLeaseZIP(t, "inner.7z", data, method), "archive.zip", "inner.7z/member-a.bin"
				}
				root, api := otherArchiveFixture(t, name, body, []byte("unused-synthetic-password"), false)
				reject := &rejectSolidPasswordAPI{archiveAPI: api}
				root.tree.api = reject
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := root.Prepare(ctx); err != nil {
					t.Fatal(err)
				}
				var got []byte
				if adapter == "service" {
					service := NewServiceFromNode(root)
					r, _, err := service.OpenArchiveMember(ctx, 77, path)
					if err != nil {
						t.Fatalf("plain member must not require unrelated password: %v", err)
					}
					defer r.(io.Closer).Close()
					got = make([]byte, 64<<10)
					if n, err := solidReuseRead(ctx, r, got, 0); err != nil || n != len(got) {
						t.Fatalf("plain member read: n=%d err=%v", n, err)
					}
				} else if adapter == "kernel" {
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
					f, err := os.Open(filepath.Join(point, name, path))
					if err != nil {
						t.Fatal(err)
					}
					got = make([]byte, 64<<10)
					n, err := f.ReadAt(got, 0)
					f.Close()
					if err != nil || n != len(got) {
						t.Fatalf("kernel read: %d %v", n, err)
					}
				} else {
					fs.NewNodeFS(root, &fs.Options{})
					node := lookup(t, root, name)
					for _, part := range strings.Split(path, "/") {
						node = lookup(t, node, part)
					}
					h, _, errno := node.Open(ctx, syscall.O_RDONLY)
					if errno != 0 {
						t.Fatalf("plain FUSE open must not require unrelated password: %v", errno)
					}
					defer h.(fs.FileReleaser).Release(ctx)
					r, errno := h.(fs.FileReader).Read(ctx, make([]byte, 64<<10), 0)
					if errno != 0 {
						t.Fatal(errno)
					}
					defer r.Done()
					content, fuseStatus := r.Bytes(nil)
					if fuseStatus != 0 {
						t.Fatal(fuseStatus)
					}
					got = content
				}
				// ZIP currently discovers a password once for its existing index
				// identity; failure is ignored. The 7z child must add no discovery.
				wantReads := 0
				if parent != "root" {
					wantReads = 1
				}
				if !bytes.Equal(got, solid7zReusePayload(0)[:64<<10]) || reject.reads != wantReads {
					t.Fatalf("clear member mismatched or discovered unrelated passwords: reads=%d", reject.reads)
				}
			})
		}
	}
}

// The outer header needs a secret, but its stored inner archive and that
// archive's content are both clear. Reopening metadata must preserve the secret.
func Test7zHeaderOnlyPasswordSurvivesNestedExpansion(t *testing.T) {
	data, err := os.ReadFile("testdata/encrypted/header-only.7z")
	if err != nil {
		t.Fatal(err)
	}
	for _, adapter := range []string{"service", "fuse"} {
		t.Run(adapter, func(t *testing.T) {
			root, api := otherArchiveFixture(t, "archive.7z", data, []byte("mount-test-password"), false)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			want := bytes.Repeat([]byte("mount 7z fixture contents\n"), 200)
			for pass := 0; pass < 2; pass++ {
				if pass > 0 {
					root = New(ctx, api, root.tree.cache, 0, true)
					fs.NewNodeFS(root, &fs.Options{})
				}
				if err := root.Prepare(ctx); err != nil {
					t.Fatal(err)
				}
				var got []byte
				if adapter == "service" {
					r, _, err := NewServiceFromNode(root).OpenArchiveMember(ctx, 77, "inner.7z/folder/hello.txt")
					if err != nil {
						t.Fatal(err)
					}
					got = make([]byte, len(want))
					n, err := solidReuseRead(ctx, r, got, 0)
					r.(io.Closer).Close()
					if err != nil || n != len(want) {
						t.Fatalf("read: %d %v", n, err)
					}
				} else {
					file := lookup(t, lookup(t, lookup(t, lookup(t, root, "archive.7z"), "inner.7z"), "folder"), "hello.txt")
					got = readNode(t, file, 0, len(want))
				}
				if !bytes.Equal(got, want) {
					t.Fatal("header-only nested payload differs")
				}
			}
		})
	}
}

func Test7zEncryptionMetadataRestores(t *testing.T) {
	for _, tc := range []struct {
		fixture, password string
		header, body      bool
	}{
		{"encrypted/plain.7z", "", false, false},
		{"encrypted/password.7z", "mount-test-password", false, true},
		{"encrypted/headers.7z", "mount-test-password", true, true},
		{"encrypted/header-only.7z", "mount-test-password", true, false},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			data, err := os.ReadFile("testdata/" + tc.fixture)
			if err != nil {
				t.Fatal(err)
			}
			root, _ := otherArchiveFixture(t, "archive.7z", data, []byte(tc.password), false)
			ctx := context.Background()
			var entries []archiveMember
			err = scanArchive(ctx, ".7z", bytes.NewReader(data), int64(len(data)), []byte(tc.password), func(m archiveMember) error { entries = append(entries, m); return nil })
			if err != nil {
				t.Fatal(err)
			}
			idx, err := nestedIndexFromMembers(entries, defaultNestedArchivePolicy)
			if err != nil {
				t.Fatal(err)
			}
			assert := func(idx *zipIndex) {
				t.Helper()
				if len(idx.members) == 0 {
					t.Fatal("empty test index")
				}
				for _, m := range idx.members {
					if m.sevenHeadersEncrypted != tc.header || m.encrypted != tc.body {
						t.Fatalf("encryption facts = header:%t body:%t", m.sevenHeadersEncrypted, m.encrypted)
					}
				}
			}
			assert(idx)
			root.tree.persistArchiveIndex(ctx, "synthetic-index", ".7z", int64(len(data)), "synthetic-identity", idx)
			restored, ok := root.tree.loadPersistentArchiveIndex(ctx, "synthetic-index", ".7z", int64(len(data)), "synthetic-identity")
			if !ok {
				t.Fatal("could not restore encryption metadata")
			}
			assert(restored)
		})
	}
}
