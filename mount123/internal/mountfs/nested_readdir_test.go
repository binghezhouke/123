package mountfs

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func nestedReaddirFixture(t *testing.T, fixture string) *Node {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata/encrypted", fixture))
	if err != nil {
		t.Fatal(err)
	}
	root, _ := otherArchiveFixture(t, "outer.7z", data, nil, false)
	return root
}

// The kernel obtains child inodes through the directory handle during
// READDIRPLUS, and can subsequently open them without calling Node.Lookup.
func nestedReaddirPlusChildren(t *testing.T, node *Node) map[string]*Node {
	t.Helper()
	ctx := context.Background()
	h, _, errno := node.OpendirHandle(ctx, 0)
	if errno != 0 {
		t.Fatalf("OpendirHandle(%q): %v", node.item.name, errno)
	}
	defer h.(fs.FileReleasedirer).Releasedir(ctx, 0)
	children := make(map[string]*Node)
	for {
		before := node.tree.cache.Stats()
		de, errno := h.(fs.FileReaddirenter).Readdirent(ctx)
		if errno != 0 {
			t.Fatalf("Readdirent(%q): %v", node.item.name, errno)
		}
		if de == nil {
			return children
		}
		inode, errno := h.(fs.FileLookuper).Lookup(ctx, de.Name, &fuse.EntryOut{})
		after := node.tree.cache.Stats()
		if before.Foreground.ReadRequests != after.Foreground.ReadRequests || before.Background.ReadRequests != after.Background.ReadRequests {
			t.Fatalf("directory Lookup(%q) read archive data during READDIRPLUS", de.Name)
		}
		if errno == syscall.EAGAIN {
			if inode != nil {
				t.Fatalf("deferred directory Lookup(%q) returned an inode", de.Name)
			}
			// READDIRPLUS omits the inode when metadata lookup is deferred;
			// pathname traversal then issues the normal LOOKUP operation.
			inode, errno = node.Lookup(ctx, de.Name, &fuse.EntryOut{})
		}
		if errno != 0 {
			t.Fatalf("directory Lookup(%q): %v", de.Name, errno)
		}
		child := inode.Operations().(*Node)
		if (de.Mode&syscall.S_IFMT == syscall.S_IFDIR) != child.item.directory {
			t.Fatalf("directory type for %q disagrees: dirent=%#o directory=%v", de.Name, de.Mode, child.item.directory)
		}
		children[de.Name] = child
	}
}

type nestedReaddirCase struct {
	name, fixture, inner string
	leaves               []string
	member               []string
	prefix               []byte
}

func nestedReaddirCases() []nestedReaddirCase {
	return []nestedReaddirCase{
		{"split", "nested-vol-outer.7z", "inner.7z.001", []string{"plain.7z"}, []string{"plain.7z"}, []byte{'7', 'z', 0xbc, 0xaf, 0x27, 0x1c}},
		{"plain", "nested-outer.7z", "inner.7z", []string{"folder"}, []string{"folder", "hello.txt"}, []byte("mount 7z fixture contents\n")},
	}
}

func TestNestedSplit7zAfterReaddirPlus(t *testing.T) {
	for _, tc := range nestedReaddirCases() {
		t.Run(tc.name, func(t *testing.T) {
			root := nestedReaddirFixture(t, tc.fixture)
			outer := nestedReaddirPlusChildren(t, root)["outer.7z"]
			if outer == nil {
				t.Fatal("outer archive missing from root directory")
			}
			inner := nestedReaddirPlusChildren(t, outer)[tc.inner]
			if inner == nil {
				t.Fatal("nested archive missing from outer directory")
			}
			leaves := nestedReaddirPlusChildren(t, inner)
			names := make([]string, 0, len(leaves))
			for name := range leaves {
				names = append(names, name)
			}
			sort.Strings(names)
			if !reflect.DeepEqual(names, tc.leaves) {
				t.Fatalf("nested directory after READDIRPLUS = %v, want %v; outer entries must not repeat", names, tc.leaves)
			}
			member := leaves[tc.member[0]]
			for _, name := range tc.member[1:] {
				member = nestedReaddirPlusChildren(t, member)[name]
			}
			if member == nil {
				t.Fatal("nested payload missing")
			}
			got := readNode(t, member, 0, len(tc.prefix))
			if !bytes.Equal(got, tc.prefix) {
				t.Fatalf("nested payload prefix = %x, want %x", got, tc.prefix)
			}
		})
	}
}

func TestActualFUSENestedSplit7zAfterReaddirPlus(t *testing.T) {
	if os.Getenv("MOUNT123_FUSE_TEST") != "1" {
		t.Skip("set MOUNT123_FUSE_TEST=1 to exercise the kernel FUSE path")
	}
	for _, tc := range nestedReaddirCases() {
		t.Run(tc.name, func(t *testing.T) {
			root := nestedReaddirFixture(t, tc.fixture)
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
			if _, err := os.ReadDir(point); err != nil {
				t.Fatal(err)
			}
			outer := filepath.Join(point, "outer.7z")
			entries, err := os.ReadDir(outer)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, entry := range entries {
				if entry.Name() == tc.inner {
					found = true
					if !entry.IsDir() {
						t.Fatalf("nested archive %q is not a directory dirent", tc.inner)
					}
				}
			}
			if !found {
				t.Fatal("nested archive missing from outer directory")
			}
			inner := filepath.Join(outer, tc.inner)
			leaves, err := os.ReadDir(inner)
			if err != nil {
				t.Fatal(err)
			}
			names := make([]string, 0, len(leaves))
			for _, leaf := range leaves {
				names = append(names, leaf.Name())
			}
			if !reflect.DeepEqual(names, tc.leaves) {
				t.Fatalf("nested directory after ReadDir(outer) = %v, want %v; outer entries must not repeat", names, tc.leaves)
			}
			path := filepath.Join(append([]string{inner}, tc.member...)...)
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			got := make([]byte, len(tc.prefix))
			if _, err := io.ReadFull(file, got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, tc.prefix) {
				t.Fatalf("nested payload prefix = %x, want %x", got, tc.prefix)
			}
		})
	}
}
