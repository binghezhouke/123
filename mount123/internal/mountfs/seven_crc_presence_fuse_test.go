package mountfs

import (
	"archive/zip"
	"bytes"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func TestActualFUSE7zCRCPresence(t *testing.T) {
	if os.Getenv("MOUNT123_FUSE_TEST") != "1" {
		t.Skip("set MOUNT123_FUSE_TEST=1 to exercise the kernel FUSE path")
	}
	for _, tc := range []struct {
		name    string
		payload []byte
		defined bool
		bad     bool
	}{
		{"absent", []byte("BC"), false, false},
		{"declared-zero", []byte{0x9d, 0x0a, 0xd9, 0x6d}, true, false},
		{"corrupt-zero", []byte{0x9c, 0x0a, 0xd9, 0x6d}, true, true},
	} {
		for _, nested := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/nested=%t", tc.name, nested), func(t *testing.T) {
				payloads := [][]byte{[]byte("A"), tc.payload}
				archive := crcPresence7z(payloads, []bool{true, tc.defined}, []uint32{crc32.ChecksumIEEE(payloads[0]), 0}, true, crc32.ChecksumIEEE(bytes.Join(payloads, nil)))
				name, memberPath := "archive.7z", "archive.7z/b"
				if nested {
					archive = nestedDeflateLeaseZIP(t, "inner.7z", archive, zip.Deflate)
					name, memberPath = "outer.zip", "outer.zip/inner.7z/b"
				}
				root, _ := otherArchiveFixture(t, name, archive, nil, false)
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
				for attempt := 0; attempt < 2; attempt++ {
					got, readErr := os.ReadFile(filepath.Join(point, memberPath))
					if tc.bad {
						if readErr == nil {
							t.Fatal("kernel read accepted corrupt declared-zero member")
						}
					} else if readErr != nil || !bytes.Equal(got, tc.payload) {
						t.Fatalf("kernel member bytes=%x err=%v", got, readErr)
					}
				}
			})
		}
	}
}
