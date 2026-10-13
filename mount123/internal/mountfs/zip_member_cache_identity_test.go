package mountfs

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func zipDisplayCacheFixture(t *testing.T, raw string, extra []byte, encryption string) ([]byte, []byte, []byte) {
	t.Helper()
	var data bytes.Buffer
	zw := zip.NewWriter(&data)
	want := append(bytes.Repeat([]byte("mount123 encrypted fixture\n"), 256), byteSliceRange()...)
	var password []byte
	if encryption == "plain" {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: raw, NonUTF8: !utf8.ValidString(raw), Extra: extra, Method: zip.Deflate})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = w.Write(want); err != nil {
			t.Fatal(err)
		}
	} else {
		password = []byte("mount-test-password")
		source, err := os.ReadFile("testdata/encrypted/" + encryption + ".zip")
		if err != nil {
			t.Fatal(err)
		}
		zr, err := zip.NewReader(bytes.NewReader(source), int64(len(source)))
		if err != nil {
			t.Fatal(err)
		}
		f := zr.File[0]
		f.Name = raw
		f.Flags &^= 0x800
		f.NonUTF8 = !utf8.ValidString(raw)
		// Match CreateHeader: ASCII names do not require the UTF-8 flag,
		// so their Unicode Path extra field remains authoritative.
		if !f.NonUTF8 && utf8.RuneCountInString(raw) < len(raw) {
			f.Flags |= 0x800
		}
		f.Extra = append(f.Extra, extra...)
		if err := zw.Copy(f); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes(), want, password
}

func readZIPDisplayCacheMember(t *testing.T, root *Node, adapter, display string, want []byte) {
	t.Helper()
	var got []byte
	if adapter == "service" {
		r, _, err := NewServiceFromNode(root).OpenArchiveMember(context.Background(), 77, display)
		if err != nil {
			t.Fatal(err)
		}
		got = make([]byte, len(want))
		n, err := r.ReadAt(got, 0)
		r.(io.Closer).Close()
		if err != nil || n != len(want) {
			t.Fatalf("service read: %d %v", n, err)
		}
	} else {
		file := lookup(t, root, "archive.zip")
		for _, part := range strings.Split(display, "/") {
			file = lookup(t, file, part)
		}
		got = readNode(t, file, 0, len(want))
	}
	if !bytes.Equal(got, want) {
		t.Fatal("decrypted payload differs")
	}
}

func TestZIPMemberCacheSharedAcrossAdaptersAndDisplayNames(t *testing.T) {
	for _, encoding := range []struct {
		label, raw, display string
		extra               []byte
	}{
		{label: "utf8", raw: "folder/member.bin", display: "folder/member.bin"},
		{label: "utf8-chinese", raw: "中文/图.bin", display: "中文/图.bin"},
		{label: "literal-replacement", raw: "�.bin", display: "�.bin"},
		{label: "gbk", raw: "\xd6\xd0\xce\xc4/\xcd\xbc.bin", display: "中文/图.bin"},
		{label: "cp437", raw: "caf\x82.bin", display: "café.bin"},
		{label: "unicode-extra", raw: "old.bin", display: "新.bin", extra: unicodeExtra("old.bin", "新.bin")},
	} {
		for _, encryption := range []string{"plain", "aes256-deflate", "zipcrypto-deflate"} {
			for _, first := range []string{"fuse", "service"} {
				t.Run(encoding.label+"/"+encryption+"/"+first, func(t *testing.T) {
					data, want, password := zipDisplayCacheFixture(t, encoding.raw, encoding.extra, encryption)
					root, api := otherArchiveFixture(t, "archive.zip", data, password, false)
					ctx := context.Background()
					if err := root.Prepare(ctx); err != nil {
						t.Fatal(err)
					}
					read := func(adapter string) { t.Helper(); readZIPDisplayCacheMember(t, root, adapter, encoding.display, want) }
					read(first)
					before := root.tree.cache.IOStats().Snapshot().Stages.Decompression
					if before.Samples == nil || *before.Samples != 1 {
						t.Fatalf("initial decompressions=%v", before.Samples)
					}
					second := "service"
					if first == second {
						second = "fuse"
					}
					read(second)
					after := root.tree.cache.IOStats().Snapshot().Stages.Decompression
					if after.Samples == nil || *after.Samples != *before.Samples {
						t.Fatalf("switching adapters decoded the same member again: before=%d after=%d", *before.Samples, *after.Samples)
					}
					root = New(ctx, api, root.tree.cache, 0, true)
					fs.NewNodeFS(root, &fs.Options{})
					if err := root.Prepare(ctx); err != nil {
						t.Fatal(err)
					}
					read(first)
					restored := root.tree.cache.IOStats().Snapshot().Stages.Decompression
					if restored.Samples == nil || *restored.Samples != *before.Samples {
						t.Fatal("index restoration lost shared member cache")
					}

				})
			}
		}
	}
}

func TestZIPLegacyWebMemberCacheSurvivesAdapterSwitch(t *testing.T) {
	for _, encryption := range []string{"plain", "aes256-deflate", "zipcrypto-deflate"} {
		for _, adapter := range []string{"fuse", "service"} {
			t.Run(encryption+"/"+adapter, func(t *testing.T) {
				const display = "新.bin"
				data, want, password := zipDisplayCacheFixture(t, "old.bin", unicodeExtra("old.bin", display), encryption)
				root, api := otherArchiveFixture(t, "archive.zip", data, password, false)
				ctx := context.Background()
				if err := root.Prepare(ctx); err != nil {
					t.Fatal(err)
				}
				idx, a, _, pw, err := NewServiceFromNode(root).outerArchiveIndex(ctx, 77)
				clear(pw)
				if err != nil {
					t.Fatal(err)
				}
				m := idx.members["old.bin"]
				// Reproduce the previous released web cache identity, not the new helper.
				legacy := root.tree.diskCacheScope() + ":" + rootZIPMemberIdentity(idx.prepared) + ":.zip:member:" + display + fmt.Sprintf(":%08x:%d", m.crc, m.size)
				if encryption != "plain" {
					legacy = root.tree.diskCacheScope() + ":" + rootZIPMemberIdentity(idx.prepared) + ":.zip:encrypted-member:" + display + fmt.Sprintf(":%08x:%d:%s", m.crc, m.size, root.tree.passwordTag(a, password))
				}
				if err := root.tree.cache.Store(ctx, legacy, want); err != nil {
					t.Fatal(err)
				}
				// A fresh Tree also restores the visible path from its persisted index.
				root = New(ctx, api, root.tree.cache, 0, true)
				fs.NewNodeFS(root, &fs.Options{})
				if err := root.Prepare(ctx); err != nil {
					t.Fatal(err)
				}
				readZIPDisplayCacheMember(t, root, adapter, display, want)
				stage := root.tree.cache.IOStats().Snapshot().Stages.Decompression
				if stage.Samples != nil && *stage.Samples != 0 {
					t.Fatalf("legacy cached member decoded again: %d", *stage.Samples)
				}
			})
		}
	}
}

func TestZIPLegacyRawNamesRemainDistinctAfterIndexRestore(t *testing.T) {
	adapters := []string{"fuse", "service"}
	if os.Getenv("MOUNT123_FUSE_TEST") == "1" {
		adapters = append(adapters, "kernel")
	}
	for _, parent := range []string{"root", "zip-store"} {
		for _, adapter := range adapters {
			t.Run(parent+"/"+adapter, func(t *testing.T) {
				var data bytes.Buffer
				zw := zip.NewWriter(&data)
				samples := []struct {
					raw, display string
					body         []byte
				}{
					{"\xd6\xd0.bin", "中.bin", bytes.Repeat([]byte("first distinct payload"), 100)},
					{"\xd6\xce.bin", "治.bin", bytes.Repeat([]byte("other distinct payload"), 100)},
				}
				for _, s := range samples {
					w, err := zw.CreateHeader(&zip.FileHeader{Name: s.raw, NonUTF8: true, Method: zip.Deflate})
					if err != nil {
						t.Fatal(err)
					}
					if _, err = w.Write(s.body); err != nil {
						t.Fatal(err)
					}
				}
				if err := zw.Close(); err != nil {
					t.Fatal(err)
				}
				body, prefix := data.Bytes(), ""
				if parent == "zip-store" {
					body = nestedDeflateLeaseZIP(t, "inner.zip", body, zip.Store)
					prefix = "inner.zip/"
				}
				root, api := otherArchiveFixture(t, "archive.zip", body, nil, false)
				ctx := context.Background()
				if err := root.Prepare(ctx); err != nil {
					t.Fatal(err)
				}
				if _, err := NewServiceFromNode(root).ListArchive(ctx, 77, strings.TrimSuffix(prefix, "/")); err != nil {
					t.Fatal(err)
				}
				root = New(ctx, api, root.tree.cache, 0, true)
				fs.NewNodeFS(root, &fs.Options{})
				if err := root.Prepare(ctx); err != nil {
					t.Fatal(err)
				}
				var point string
				if adapter == "kernel" {
					point = t.TempDir()
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
				}
				for _, s := range samples {
					if adapter == "kernel" {
						got, err := os.ReadFile(filepath.Join(point, "archive.zip", prefix, s.display))
						if err != nil || !bytes.Equal(got, s.body) {
							t.Fatalf("kernel restored name returned incorrect member: %v", err)
						}
					} else {
						readZIPDisplayCacheMember(t, root, adapter, prefix+s.display, s.body)
					}
				}
			})
		}
	}
}

func TestZIPConcurrentAdaptersJoinOneMemberFill(t *testing.T) {
	for _, encryption := range []string{"plain", "aes256-deflate", "zipcrypto-deflate"} {
		t.Run(encryption, func(t *testing.T) {
			const raw = "\xd6\xd0\xce\xc4.bin"
			data, want, password := zipDisplayCacheFixture(t, raw, nil, encryption)
			original, api := otherArchiveFixture(t, "archive.zip", data, password, false)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			root := NewWithOptions(ctx, api, original.tree.cache, 0, true, Options{MaxConcurrentBuilds: 1, StreamMemberThreshold: 1})
			fs.NewNodeFS(root, &fs.Options{})
			if err := root.Prepare(ctx); err != nil {
				t.Fatal(err)
			}
			node := lookup(t, lookup(t, root, "archive.zip"), "中文.bin")
			service := NewServiceFromNode(root)
			if _, err := service.ListArchive(ctx, 77, ""); err != nil {
				t.Fatal(err)
			}
			// Hold decoder admission after metadata is ready. Both open handles then
			// coexist while their shared producer is unable to finish.
			release, err := root.tree.acquireBuild(ctx)
			if err != nil {
				t.Fatal(err)
			}
			h, _, errno := node.Open(ctx, syscall.O_RDONLY)
			if errno != 0 {
				release()
				t.Fatal(errno)
			}
			defer h.(fs.FileReleaser).Release(ctx)
			web, _, err := service.OpenArchiveMember(ctx, 77, "中文.bin")
			release()
			if err != nil {
				t.Fatal(err)
			}
			defer web.(io.Closer).Close()
			r, errno := h.(fs.FileReader).Read(ctx, make([]byte, len(want)), 0)
			if errno != 0 {
				t.Fatal(errno)
			}
			got, status := r.Bytes(nil)
			equal := status == 0 && bytes.Equal(got, want)
			r.Done()
			if !equal {
				t.Fatal("FUSE stream differs")
			}
			p := make([]byte, len(want))
			n, err := web.ReadAt(p, 0)
			if err != nil || n != len(p) || !bytes.Equal(p, want) {
				t.Fatal("web stream differs")
			}
			if err := h.(*handle).growing.Wait(ctx); err != nil {
				t.Fatal(err)
			}
			stage := root.tree.cache.IOStats().Snapshot().Stages.Decompression
			if stage.Samples == nil || *stage.Samples != 1 {
				t.Fatal("adapters did not share one in-flight decoder")
			}
		})
	}
}
