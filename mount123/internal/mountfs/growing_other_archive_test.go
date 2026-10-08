package mountfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

const growingRARFixtureSHA256 = "2eaebb4c18cdef7f20089f8a2fa3475bc59c2a193f66e2f1513609a4bef13e22"

func setGrowingThreshold(root *Node) { root.tree.opts.StreamMemberThreshold = 1 }

func growingOtherMemberKey(t *testing.T, node *Node) string {
	t.Helper()
	password, err := node.tree.otherPassword(context.Background(), node.item.archive)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(password)
	_, _, identity, err := node.tree.archiveSource(context.Background(), node.item.source, node.item.archive)
	if err != nil {
		t.Fatal(err)
	}
	m, a := node.item.member, node.item.archive
	return node.tree.diskCacheScope() + ":" + identity + ":" + archiveKind(a.name) + ":archive-member:" + m.name + fmt.Sprintf(":%d:%08x:", m.size, m.crc) + node.tree.passwordTag(a, password)
}

func readGrowingOtherMember(t *testing.T, node *Node) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, _, errno := node.Open(ctx, syscall.O_RDONLY)
	if errno != 0 {
		t.Fatalf("Open growing member: %v", errno)
	}
	defer h.(fs.FileReleaser).Release(context.Background())
	mh, ok := h.(*handle)
	if !ok || mh.growing == nil {
		t.Fatal("member did not use the growing-cache path")
	}
	result, errno := h.(fs.FileReader).Read(ctx, make([]byte, int(node.item.member.size)), 0)
	if errno != 0 {
		t.Fatalf("Read growing member: %v", errno)
	}
	got, status := result.Bytes(nil)
	result.Done()
	if status != fuse.OK || len(got) != int(node.item.member.size) {
		t.Fatalf("read result status=%v size=%d want=%d", status, len(got), node.item.member.size)
	}
	waitForGrowingCacheEntry(t, ctx, node, growingOtherMemberKey(t, node), mh.growing)
	return got
}

func waitForGrowingCacheEntry(t *testing.T, ctx context.Context, node *Node, key string, growing *storage.GrowingHandle) {
	t.Helper()
	if err := growing.Wait(ctx); err != nil {
		t.Fatalf("wait for growing member: %v", err)
	}
	h, err := node.tree.cache.Open(key)
	if err != nil {
		t.Fatalf("open materialized growing member: %v", err)
	}
	_ = h.Close()
}

func TestGrowingOtherArchivesReadSplit7zAndSolidRAR(t *testing.T) {
	t.Run("split-encrypted-7z", func(t *testing.T) {
		data, err := os.ReadFile("testdata/encrypted/headers.7z")
		if err != nil {
			t.Fatal(err)
		}
		root, _ := otherArchiveFixture(t, "private.7z.001", data, []byte("mount-test-password"), true)
		archive := lookup(t, root, "private.7z.001")
		setGrowingThreshold(root)
		node := lookup(t, lookup(t, archive, "folder"), "hello.txt")
		got := readGrowingOtherMember(t, node)
		want := bytes.Repeat([]byte("mount 7z fixture contents\n"), 200)
		if !bytes.Equal(got, want) {
			t.Fatal("split 7z growing read did not match plaintext")
		}
	})
	t.Run("solid-rar", func(t *testing.T) {
		data, err := os.ReadFile(filepath.Join("../../../tests/fixtures/rar", "rar3-solid.rar"))
		if err != nil {
			t.Fatal(err)
		}
		root, _ := otherArchiveFixture(t, "private.rar", data, nil, false)
		archive := lookup(t, root, "private.rar")
		entries, err := archive.list(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		setGrowingThreshold(root)
		var gotAny bool
		for name, entry := range entries {
			if entry.directory {
				continue
			}
			got := readGrowingOtherMember(t, lookup(t, archive, name))
			if fmt.Sprintf("%x", sha256.Sum256(got)) != growingRARFixtureSHA256 {
				t.Fatalf("solid RAR member %q plaintext hash mismatch", name)
			}
			gotAny = true
			break
		}
		if !gotAny {
			t.Fatal("solid RAR fixture had no file member")
		}
	})
}

func TestGrowingOtherArchiveWrongPasswordAndCRCFailureAreNotCached(t *testing.T) {
	t.Run("wrong-password", func(t *testing.T) {
		data, err := os.ReadFile("testdata/encrypted/password.7z")
		if err != nil {
			t.Fatal(err)
		}
		root, _ := otherArchiveFixture(t, "private.7z", data, []byte("wrong-password"), false)
		archive := lookup(t, root, "private.7z")
		if _, err := archive.list(context.Background()); err != nil {
			t.Fatalf("list password-visible 7z headers: %v", err)
		}
		setGrowingThreshold(root)
		node := lookup(t, lookup(t, archive, "folder"), "hello.txt")
		assertGrowingOtherArchiveFailsAndIsNotCached(t, node, syscall.EACCES)
	})
	t.Run("corrupt-7z-payload", func(t *testing.T) {
		data, err := os.ReadFile("testdata/encrypted/plain.7z")
		if err != nil {
			t.Fatal(err)
		}
		if len(data) < 40 {
			t.Fatal("plain 7z fixture is too short")
		}
		data[32] ^= 0x40 // damage packed member bytes while leaving the next header intact
		root, _ := otherArchiveFixture(t, "private.7z", data, nil, false)
		archive := lookup(t, root, "private.7z")
		if _, err := archive.list(context.Background()); err != nil {
			t.Fatalf("corrupt packed payload should leave the member index readable: %v", err)
		}
		setGrowingThreshold(root)
		node := lookup(t, lookup(t, archive, "folder"), "hello.txt")
		assertGrowingOtherArchiveFailsAndIsNotCached(t, node, syscall.EIO)
	})
}

func assertGrowingOtherArchiveFailsAndIsNotCached(t *testing.T, node *Node, want syscall.Errno) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h, _, errno := node.Open(ctx, syscall.O_RDONLY)
	key := growingOtherMemberKey(t, node)
	if errno != 0 {
		if errno != want {
			t.Fatalf("Open errno=%v want=%v", errno, want)
		}
		assertGrowingCacheAbsent(t, node, key)
		return
	}
	defer h.(fs.FileReleaser).Release(context.Background())
	if mh, ok := h.(*handle); !ok || mh.growing == nil {
		t.Fatal("member did not use the growing-cache path")
	}
	for {
		result, readErrno := h.(fs.FileReader).Read(ctx, make([]byte, int(node.item.member.size)), 0)
		if result != nil {
			result.Done()
		}
		if readErrno != 0 {
			if readErrno != want {
				t.Fatalf("Read errno=%v want=%v", readErrno, want)
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("growing decoder did not report failure: %v", ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	assertGrowingCacheAbsent(t, node, key)
}

func assertGrowingCacheAbsent(t *testing.T, node *Node, key string) {
	t.Helper()
	if h, err := node.tree.cache.Open(key); err == nil {
		_ = h.Close()
		t.Fatal("failed growing archive member was published to the cache")
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache lookup after growing failure: %v", err)
	}
}

func TestGrowingOtherArchiveCoalescesReadersWhenOneCloses(t *testing.T) {
	data, err := os.ReadFile("testdata/encrypted/solid.7z")
	if err != nil {
		t.Fatal(err)
	}
	root, _ := otherArchiveFixture(t, "private.7z", data, []byte("mount-test-password"), false)
	archive := lookup(t, root, "private.7z")
	if _, err := archive.list(context.Background()); err != nil {
		t.Fatal(err)
	}
	setGrowingThreshold(root)
	node := lookup(t, lookup(t, archive, "folder"), "hello.txt")

	// Occupy every build slot so both Opens join the same not-yet-started fill.
	var releaseSlots []func()
	for i := 0; i < cap(root.tree.builds); i++ {
		release, err := root.tree.acquireBuild(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		releaseSlots = append(releaseSlots, release)
	}
	defer func() {
		for _, release := range releaseSlots {
			release()
		}
	}()
	first, _, errno := node.Open(context.Background(), syscall.O_RDONLY)
	if errno != 0 {
		t.Fatalf("first Open: %v", errno)
	}
	second, _, errno := node.Open(context.Background(), syscall.O_RDONLY)
	if errno != 0 {
		first.(fs.FileReleaser).Release(context.Background())
		t.Fatalf("second Open: %v", errno)
	}
	firstGrowing, firstOK := first.(*handle)
	secondGrowing, secondOK := second.(*handle)
	if !firstOK || !secondOK || firstGrowing.growing == nil || secondGrowing.growing == nil {
		t.Fatal("concurrent Opens did not both use growing handles")
	}
	if errno := first.(fs.FileReleaser).Release(context.Background()); errno != 0 {
		t.Fatalf("close first reader: %v", errno)
	}
	for _, release := range releaseSlots {
		release()
	}
	releaseSlots = nil
	got := make([]byte, int(node.item.member.size))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, errno := second.(fs.FileReader).Read(ctx, got, 0)
	if errno != 0 {
		t.Fatalf("remaining reader failed after sibling closed: %v", errno)
	}
	data, status := result.Bytes(nil)
	result.Done()
	if status != fuse.OK || !bytes.Equal(data, bytes.Repeat([]byte("mount 7z fixture contents\n"), 200)) {
		t.Fatal("remaining growing reader returned incorrect archive data")
	}
	waitForGrowingCacheEntry(t, ctx, node, growingOtherMemberKey(t, node), secondGrowing.growing)
	if errno := second.(fs.FileReleaser).Release(context.Background()); errno != 0 {
		t.Fatalf("close second reader: %v", errno)
	}
}
