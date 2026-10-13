package mountfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func nestedFUSEChain(t *testing.T, formats []string, payload []byte) ([]byte, []string) {
	t.Helper()
	data := multilevelFixture(t, formats[len(formats)-1], map[string][]byte{"folder/video.bin": payload})
	names := []string{"outer." + formats[0]}
	for _, format := range formats[1:] {
		names = append(names, "inner."+format)
	}
	for i := len(formats) - 2; i >= 0; i-- {
		data = multilevelFixture(t, formats[i], map[string][]byte{names[i+1]: data})
	}
	return data, names
}

func TestNestedFUSEExplicitLookupMultipleLevels(t *testing.T) {
	for _, formats := range [][]string{{"zip", "zip", "zip"}, {"7z", "7z", "rar"}, {"rar", "zip", "7z"}} {
		t.Run(formats[0]+"-"+formats[1]+"-"+formats[2], func(t *testing.T) {
			want := bytes.Repeat([]byte("nested video bytes\n"), 200)
			data, names := nestedFUSEChain(t, formats, want)
			root, _ := otherArchiveFixture(t, names[0], data, nil, false)
			// A single decoder slot must suffice for reads through ancestors.
			root.tree.builds = make(chan struct{}, 1)
			root.tree.opts.MaxConcurrentBuilds = 1
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			node := root
			var archives []*nestedArchive
			for _, name := range names {
				// Directory listings must not pre-expand any level. READDIRPLUS
				// leaves archive inodes to an explicit Lookup.
				h, _, errno := node.OpendirHandle(ctx, 0)
				if errno != 0 {
					t.Fatal(errno)
				}
				before := node.tree.cache.Stats()
				inode, plusErrno := h.(fs.FileLookuper).Lookup(ctx, name, &fuse.EntryOut{})
				after := node.tree.cache.Stats()
				h.(fs.FileReleasedirer).Releasedir(ctx, 0)
				if node != root && (plusErrno != syscall.EAGAIN || inode != nil) {
					t.Fatalf("READDIRPLUS %q: inode=%v errno=%v", name, inode, plusErrno)
				}
				if before.UsedBytes != after.UsedBytes || before.FillSuccesses != after.FillSuccesses {
					t.Fatalf("READDIRPLUS %q performed archive work", name)
				}
				inode, errno = node.Lookup(ctx, name, &fuse.EntryOut{})
				if errno != 0 {
					t.Fatalf("Lookup %q: %v", name, errno)
				}
				node = inode.Operations().(*Node)
				if !node.item.directory {
					t.Fatalf("archive %q was not expanded", name)
				}
				if node.item.nested != nil {
					archives = append(archives, node.item.nested)
					if node.item.zipPath != "" || node.item.source != nil || node.item.archive != nil {
						t.Fatalf("archive %q retained its parent's path or source", name)
					}
				}
			}
			cancel() // These nodes must not retain the completed Lookup request.
			leaf := lookup(t, lookup(t, node, "folder"), "video.bin")
			openCtx, openCancel := context.WithTimeout(context.Background(), 5*time.Second)
			h, _, errno := leaf.Open(openCtx, syscall.O_RDONLY)
			openCancel()
			if errno != 0 {
				t.Fatal(errno)
			}
			defer h.(fs.FileReleaser).Release(context.Background())
			// Open pins the view independently of the inode's owner lease.
			for _, archive := range archives {
				if err := archive.Close(); err != nil {
					t.Fatal(err)
				}
			}
			result, errno := h.(fs.FileReader).Read(context.Background(), make([]byte, len(want)), 0)
			if errno != 0 {
				t.Fatalf("read after request cancellation and owner release: %v", errno)
			}
			got, status := result.Bytes(make([]byte, len(want)))
			if status != fuse.OK || !bytes.Equal(got, want) {
				t.Fatal("nested FUSE bytes differ")
			}
		})
	}
}

func TestNestedFUSEPrefixReadDoesNotMaterializeLeaf(t *testing.T) {
	want := bytes.Repeat([]byte("video frame payload 0123456789\n"), 1<<18)
	innerZIP := multilevelFixture(t, "zip", map[string][]byte{"folder/video.bin": want})
	central := bytes.LastIndex(innerZIP, []byte{'P', 'K', 1, 2})
	if central < 0 {
		t.Fatal("fixture has no ZIP central header")
	}
	// A bad final checksum makes eager full extraction fail. Prefix reads
	// may still succeed because only the requested block has been decoded.
	innerZIP[central+16] ^= 1
	data := multilevelFixture(t, "zip", map[string][]byte{"inner.zip": innerZIP})
	root, _ := otherArchiveFixture(t, "outer.zip", data, nil, false)
	inner := lookup(t, lookup(t, root, "outer.zip"), "inner.zip")
	leaf := lookup(t, lookup(t, inner, "folder"), "video.bin")
	before := root.tree.cache.Stats()
	h, _, errno := leaf.Open(context.Background(), syscall.O_RDONLY)
	if errno != 0 {
		t.Fatal(errno)
	}
	defer h.(fs.FileReleaser).Release(context.Background())
	if after := root.tree.cache.Stats(); after.UsedBytes != before.UsedBytes {
		t.Fatalf("Open materialized %d bytes", after.UsedBytes-before.UsedBytes)
	}
	read := func(ctx context.Context, off int64) syscall.Errno {
		result, errno := h.(fs.FileReader).Read(ctx, make([]byte, 32), off)
		if errno == 0 {
			got, status := result.Bytes(make([]byte, 32))
			if status != fuse.OK || !bytes.Equal(got, want[off:off+32]) {
				t.Fatal("prefix bytes differ")
			}
		}
		return errno
	}
	if errno := read(context.Background(), 0); errno != 0 {
		t.Fatal(errno)
	}
	if added := root.tree.cache.Stats().UsedBytes - before.UsedBytes; added <= 0 || added > nestedMemberBlockBytes {
		t.Fatalf("32-byte demand cached %d bytes, want at most one block", added)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if errno := read(canceled, nestedMemberBlockBytes); errno != syscall.EINTR {
		t.Fatalf("cancelled read errno=%v", errno)
	}
	if errno := read(context.Background(), nestedMemberBlockBytes); errno != 0 {
		t.Fatalf("same handle could not retry cancelled read: %v", errno)
	}
	if added := root.tree.cache.Stats().UsedBytes - before.UsedBytes; added > 2*nestedMemberBlockBytes {
		t.Fatalf("two block demands cached %d bytes", added)
	}
	if errno := read(context.Background(), int64(len(want)-32)); errno != syscall.EIO {
		t.Fatalf("final checksum failure errno=%v, want EIO", errno)
	}
}

func TestNestedFUSEEncryptedColdPasswordSingleSlot(t *testing.T) {
	data, err := os.ReadFile("testdata/encrypted/nested-encrypted-outer.7z")
	if err != nil {
		t.Fatal(err)
	}
	root, _ := otherArchiveFixture(t, "outer.7z", data, []byte("secret"), false)
	root.tree.builds = make(chan struct{}, 1)
	root.tree.opts.MaxConcurrentBuilds = 1
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	node := root
	for _, name := range []string{"outer.7z", "inner.7z", "payload"} {
		inode, errno := node.Lookup(ctx, name, &fuse.EntryOut{})
		if errno != 0 {
			t.Fatalf("cold password lookup %q: %v", name, errno)
		}
		node = inode.Operations().(*Node)
	}
	cancel()
	if got := readNode(t, node, 0, 218); len(got) != 218 {
		t.Fatalf("encrypted payload length=%d, want 218", len(got))
	}
}

func TestNestedInodeIdentityIncludesAncestorChain(t *testing.T) {
	a := &entry{name: "video.bin", member: &member{name: "video.bin", size: 16}, nested: &nestedArchive{identity: "outer/left.zip/inner.zip", cachePrefix: "input"}}
	b := *a
	b.nested = &nestedArchive{identity: "outer/right.zip/inner.zip", cachePrefix: "input"}
	if stableAttrForEntry(42, a).Ino == stableAttrForEntry(42, &b).Ino {
		t.Fatal("distinct ancestor chains shared an inode")
	}
}

func TestNestedSameNameDirectoriesStayVisible(t *testing.T) {
	leaf := &zipDir{dirs: map[string]*zipDir{}, files: map[string]*member{"video.bin": {name: "video.bin"}}}
	child := &zipDir{dirs: map[string]*zipDir{"inner.7z.001": leaf}, files: map[string]*member{}}
	root := &zipDir{dirs: map[string]*zipDir{"inner.7z.001": child}, files: map[string]*member{}}
	archive := &nestedArchive{}
	first := indexEntryNested(root, "", "inner.7z.001", archive)
	if first == nil || first.zipPath != "inner.7z.001" {
		t.Fatalf("first same-name directory collapsed: %#v", first)
	}
	second := indexEntryNested(child, first.zipPath, "inner.7z.001", archive)
	if second == nil || second.zipPath != "inner.7z.001/inner.7z.001" {
		t.Fatalf("second same-name directory hidden: %#v", second)
	}
}

type requestAwareNestedReader struct {
	started chan context.Context
}

func (r requestAwareNestedReader) ReadAt([]byte, int64) (int, error) {
	return 0, errors.New("read bypassed request context")
}

func (r requestAwareNestedReader) ReadAtContext(ctx context.Context, _ []byte, _ int64) (int, error) {
	r.started <- ctx
	<-ctx.Done()
	return 0, ctx.Err()
}

func TestNestedFUSEHandleCancelsActiveRead(t *testing.T) {
	started := make(chan context.Context, 1)
	h := &handle{reader: requestAwareNestedReader{started: started}, size: 32}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan syscall.Errno, 1)
	go func() {
		_, errno := h.Read(ctx, make([]byte, 16), 0)
		done <- errno
	}()
	select {
	case seen := <-started:
		if seen != ctx {
			t.Fatal("handle did not pass the current request context")
		}
	case errno := <-done:
		t.Fatalf("handle bypassed ReadAtContext: %v", errno)
	case <-time.After(time.Second):
		t.Fatal("active read did not enter the request reader")
	}
	cancel()
	select {
	case errno := <-done:
		if errno != syscall.EINTR {
			t.Fatalf("cancelled active read errno=%v", errno)
		}
	case <-time.After(time.Second):
		t.Fatal("active read ignored request cancellation")
	}
}

func TestActualFUSENestedMultipleLevelsAfterReaddirPlus(t *testing.T) {
	if os.Getenv("MOUNT123_FUSE_TEST") != "1" {
		t.Skip("set MOUNT123_FUSE_TEST=1 to exercise the kernel FUSE path")
	}
	for _, formats := range [][]string{{"zip", "zip", "zip"}, {"7z", "7z", "rar"}} {
		t.Run(formats[0], func(t *testing.T) {
			want := bytes.Repeat([]byte("nested video bytes\n"), 200)
			data, names := nestedFUSEChain(t, formats, want)
			root, _ := otherArchiveFixture(t, names[0], data, nil, false)
			point := t.TempDir()
			ttl := time.Hour
			mounted, err := fs.Mount(point, root, &fs.Options{
				MountOptions: fuse.MountOptions{Options: []string{"ro"}},
				EntryTimeout: &ttl, AttrTimeout: &ttl, NegativeTimeout: &ttl,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := mounted.Unmount(); err != nil {
					t.Error(err)
				}
				mounted.Wait()
			}()
			path := point
			for _, name := range names {
				entries, err := os.ReadDir(path)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, entry := range entries {
					if entry.Name() == name {
						found = true
						if !entry.IsDir() {
							t.Fatalf("archive %q cached as a file", name)
						}
					}
				}
				if !found {
					t.Fatalf("archive %q missing", name)
				}
				path = filepath.Join(path, name)
			}
			if _, err := os.ReadDir(path); err != nil {
				t.Fatal(err)
			}
			file, err := os.Open(filepath.Join(path, "folder", "video.bin"))
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			got := make([]byte, len(want))
			if _, err := io.ReadFull(file, got); err != nil || !bytes.Equal(got, want) {
				t.Fatalf("nested kernel read mismatch: %v", err)
			}
		})
	}
}
