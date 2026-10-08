package mountfs

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func TestGrowingEncryptedZIPValidationAndCache(t *testing.T) {
	fixtures, err := filepath.Glob("testdata/encrypted/*.zip")
	if err != nil || len(fixtures) == 0 {
		t.Fatalf("fixtures: %v", err)
	}
	want := append(bytes.Repeat([]byte("mount123 encrypted fixture\n"), 256), byteSliceRange()...)
	for _, fixture := range fixtures {
		t.Run(filepath.Base(fixture), func(t *testing.T) {
			data, err := os.ReadFile(fixture)
			if err != nil {
				t.Fatal(err)
			}
			root, _ := otherArchiveFixture(t, "private.zip", data, []byte("mount-test-password"), false)
			setGrowingThreshold(root)
			node := lookup(t, lookup(t, root, "private.zip"), "hello.txt")
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			h, _, errno := node.Open(ctx, syscall.O_RDONLY)
			if errno != 0 {
				t.Fatal(errno)
			}
			defer h.(fs.FileReleaser).Release(context.Background())
			growing := h.(*handle).growing
			if growing == nil {
				t.Fatal("large encrypted member did not stream")
			}
			if err := growing.Wait(ctx); err != nil {
				t.Fatal(err)
			}
			result, errno := h.(fs.FileReader).Read(ctx, make([]byte, len(want)), 0)
			if errno != 0 {
				t.Fatal(errno)
			}
			defer result.Done()
			got, status := result.Bytes(nil)
			if status != fuse.OK || !bytes.Equal(got, want) {
				t.Fatal("streamed encrypted plaintext differs")
			}
			if !growing.Materialized() {
				t.Fatal("validated member was not published")
			}
		})
	}
	for _, mode := range []string{"wrong-password", "corrupt-auth"} {
		t.Run(mode, func(t *testing.T) {
			data, err := os.ReadFile("testdata/encrypted/aes256-ae2-store.zip")
			if err != nil {
				t.Fatal(err)
			}
			password := []byte("mount-test-password")
			if mode == "wrong-password" {
				password = []byte("wrong")
			} else {
				central := bytes.Index(data, []byte("PK\x01\x02"))
				if central < 10 {
					t.Fatal("missing directory")
				}
				data[central-1] ^= 1
			}
			root, _ := otherArchiveFixture(t, "private.zip", data, password, false)
			setGrowingThreshold(root)
			node := lookup(t, lookup(t, root, "private.zip"), "hello.txt")
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			h, _, errno := node.Open(ctx, syscall.O_RDONLY)
			if errno != 0 {
				t.Fatal(errno)
			}
			defer h.(fs.FileReleaser).Release(context.Background())
			growing := h.(*handle).growing
			if err := growing.Wait(ctx); err == nil {
				t.Fatal("invalid encrypted member completed")
			}
			if growing.Materialized() {
				t.Fatal("invalid member marked materialized")
			}
			result, errno := h.(fs.FileReader).Read(ctx, make([]byte, 1), 0)
			if result != nil {
				result.Done()
			}
			if errno == 0 {
				t.Fatal("validation error lost at public Read boundary")
			}
			m := node.item.member
			key := node.tree.diskCacheScope() + ":" + node.item.source.Key() + ":.zip:encrypted-member:" + m.name + fmt.Sprintf(":%08x:%d:%s", m.crc, m.size, node.tree.passwordTag(node.item.archive, password))
			cached, err := node.tree.cache.Open(key)
			if err == nil {
				cached.Close()
				t.Fatal("invalid plaintext was cached")
			}
			if !os.IsNotExist(err) {
				t.Fatal(err)
			}
		})
	}
}
