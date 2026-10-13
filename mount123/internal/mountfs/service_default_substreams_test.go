package mountfs

import (
	"archive/zip"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// A valid 7z may omit SubStreamsInfo: each folder then contains one stream.
// Different payload sizes expose a mistaken assignment of both files to group 0.
func TestService7zDefaultSubstreams(t *testing.T) {
	data, err := os.ReadFile("testdata/solid7z/default-substreams.7z")
	if err != nil {
		t.Fatal(err)
	}
	for _, parent := range []string{"raw", "store", "deflate"} {
		t.Run(parent, func(t *testing.T) {
			name, body, prefix := "archive.7z", data, ""
			if parent != "raw" {
				method := uint16(zip.Store)
				if parent == "deflate" {
					method = zip.Deflate
				}
				name, prefix = "outer.zip", "inner.7z"
				body = nestedDeflateLeaseZIP(t, prefix, data, method)
			}
			root, _ := otherArchiveFixture(t, name, body, nil, false)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := root.Prepare(ctx); err != nil {
				t.Fatal(err)
			}
			service := NewServiceFromNode(root)
			rows, err := service.ListArchive(ctx, 77, prefix)
			if err != nil || len(rows) != 2 {
				t.Fatalf("valid default layout listing: count=%d err=%v", len(rows), err)
			}
			for i, want := range []string{"A", "BC"} {
				path := string(rune('a' + i))
				if prefix != "" {
					path = prefix + "/" + path
				}
				reader, size, err := service.OpenArchiveMember(ctx, 77, path)
				if err != nil {
					t.Fatal(err)
				}
				got := make([]byte, len(want))
				n, readErr := reader.ReadAt(got, 0)
				closeErr := reader.(io.Closer).Close()
				if size != int64(len(want)) || n != len(want) || readErr != nil || closeErr != nil || string(got) != want {
					t.Fatalf("member %d: size=%d bytes=%x n=%d read=%v close=%v", i, size, got, n, readErr, closeErr)
				}
			}
		})
	}
}

func TestActualFUSEDefaultSubstreams(t *testing.T) {
	if os.Getenv("MOUNT123_FUSE_TEST") != "1" {
		t.Skip("set MOUNT123_FUSE_TEST=1 to exercise the kernel FUSE path")
	}
	data, err := os.ReadFile("testdata/solid7z/default-substreams.7z")
	if err != nil {
		t.Fatal(err)
	}
	outer := nestedDeflateLeaseZIP(t, "inner.7z", data, zip.Deflate)
	root, _ := otherArchiveFixture(t, "outer.zip", outer, nil, false)
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
	path := filepath.Join(point, "outer.zip", "inner.7z")
	if rows, err := os.ReadDir(path); err != nil || len(rows) != 2 {
		t.Fatalf("kernel listing count=%d err=%v", len(rows), err)
	}
	for i, want := range []string{"A", "BC"} {
		got, err := os.ReadFile(filepath.Join(path, string(rune('a'+i))))
		if err != nil || string(got) != want {
			t.Fatalf("kernel member %d bytes=%x err=%v", i, got, err)
		}
	}
}
