package mountfs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

const splitFixturePassword = "fixture-password"

// Generate both ZIP layouts with independent archive tools. In particular,
// Info-ZIP writes real disk-relative offsets; splitting an ordinary byte slice
// cannot exercise that layout. Every payload spans a 64 KiB volume boundary.
func generatedSplitZIP(t *testing.T, layout, method, encryption string) (string, map[string][]byte, map[string][]byte) {
	t.Helper()
	sevenzip, err := exec.LookPath("7z")
	if err != nil {
		t.Skip("7z fixture generator unavailable")
	}
	dir := t.TempDir()
	payloads := make(map[string][]byte)
	var names []string
	random := rand.New(rand.NewSource(77))
	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("payload-%d.bin", i)
		data := make([]byte, 90000)
		_, _ = random.Read(data)
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
		payloads[name] = data
	}
	run := func(program string, args ...string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, program, args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("synthetic ZIP tool failed: %v: %s", err, out)
		}
	}
	args := []string{"a", "-tzip", "-mx=0"}
	if method == "deflate" {
		args[2] = "-mx=1"
	}
	if encryption != "" {
		args = append(args, "-mem="+encryption, "-p"+splitFixturePassword)
	}
	entryName := "album.zip"
	if layout == "bytes" {
		args = append(args, "-v64k", "album.zip")
		entryName += ".001"
	} else {
		args = append(args, "source.zip")
	}
	run(sevenzip, append(args, names...)...)
	if layout == "disks" {
		zipcmd, err := exec.LookPath("zip")
		if err != nil {
			t.Skip("Info-ZIP fixture generator unavailable")
		}
		// Copying preserves encrypted member bytes and AES extra fields.
		run(zipcmd, "-s", "64k", "source.zip", "--out", "album.zip")
	}
	verify := []string{"t", "-p" + splitFixturePassword, entryName}
	run(sevenzip, verify...)
	paths, err := filepath.Glob(filepath.Join(dir, "album.*"))
	if err != nil || len(paths) < 2 {
		t.Fatalf("expected a genuine multivolume fixture: parts=%d error=%v", len(paths), err)
	}
	parts := make(map[string][]byte)
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		parts[filepath.Base(path)] = data
	}
	return entryName, parts, payloads
}

func TestNestedZIPSplitFormatsAndPasswords(t *testing.T) {
	for _, layout := range []string{"bytes", "disks"} {
		for _, method := range []string{"store", "deflate"} {
			for _, encryption := range []string{"", "ZipCrypto", "AES256"} {
				t.Run(layout+"/"+method+"/"+encryption, func(t *testing.T) {
					entryName, parts, payloads := generatedSplitZIP(t, layout, method, encryption)
					for _, outerFormat := range []string{"zip", "7z", "rar"} {
						t.Run(outerFormat, func(t *testing.T) {
							data := multilevelFixture(t, outerFormat, parts)
							var password []byte
							if encryption != "" {
								password = []byte(splitFixturePassword)
							}
							root, base := otherArchiveFixture(t, "outer."+outerFormat, data, password, false)
							root.tree.api = &nestedPasswordAPI{archiveAPI: base}
							root.tree.builds = make(chan struct{}, 1)
							root.tree.opts.MaxConcurrentBuilds = 1
							service := NewServiceFromNode(root)
							ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
							defer cancel()
							rows, err := service.ListArchive(ctx, 77, entryName)
							if err != nil || len(rows) != len(payloads) {
								t.Fatalf("nested split listing: count=%d error=%v", len(rows), err)
							}
							var names []string
							for name := range payloads {
								names = append(names, name)
							}
							sort.Strings(names)
							// Demand offsets deliberately go backwards after a tail read.
							for _, name := range names {
								want := payloads[name]
								r, size, err := service.OpenArchiveMember(ctx, 77, entryName+"/"+name)
								if err != nil {
									t.Fatalf("nested split open: %v", err)
								}
								if size != int64(len(want)) {
									t.Fatalf("member size = %d, want %d", size, len(want))
								}
								for _, offset := range []int64{int64(len(want) - 4096), 0, 63000} {
									got := make([]byte, 4096)
									n, err := r.ReadAt(got, offset)
									if n != len(got) || (err != nil && err != io.EOF) || !bytes.Equal(got, want[offset:offset+4096]) {
										t.Fatalf("nested split range offset=%d bytes=%d error=%v", offset, n, err)
									}
								}
								if closer, ok := r.(io.Closer); ok {
									_ = closer.Close()
								}
								// Actual public FUSE Lookup/Open/Read, sharing the same cache.
								node := root
								for _, component := range []string{"outer." + outerFormat, entryName, name} {
									node = nestedPasswordLookup(t, ctx, node, component)
								}
								h, _, errno := node.Open(ctx, syscall.O_RDONLY)
								if errno != 0 {
									t.Fatalf("split FUSE open: %v", errno)
								}
								result, errno := h.(fs.FileReader).Read(ctx, make([]byte, len(want)), 0)
								if errno != 0 {
									t.Fatalf("split FUSE read: %v", errno)
								}
								got, status := result.Bytes(nil)
								if status != fuse.OK || !bytes.Equal(got, want) {
									t.Fatalf("split FUSE payload: length=%d status=%v", len(got), status)
								}
								result.Done()
								h.(fs.FileReleaser).Release(ctx)
							}
							// Canonical entry and later volumes still expose raw bytes
							// through the explicit member-open API.
							for name, want := range parts {
								r, size, err := service.OpenArchiveMember(ctx, 77, name)
								if err != nil {
									t.Fatal(err)
								}
								got := make([]byte, len(want))
								n, err := r.ReadAt(got, 0)
								if closer, ok := r.(io.Closer); ok {
									_ = closer.Close()
								}
								if size != int64(len(want)) || n != len(want) || (err != nil && err != io.EOF) || !bytes.Equal(got, want) {
									t.Fatalf("raw volume %s changed", name)
								}
								if name != entryName && (strings.HasSuffix(name, ".001") || strings.HasSuffix(name, ".zip")) {
									t.Fatal("fixture has multiple canonical entries")
								}
							}
						})
					}
				})
			}
		}
	}
}

func TestActualFUSEZIPSplitAfterDirectoryEnumeration(t *testing.T) {
	if os.Getenv("MOUNT123_FUSE_TEST") != "1" {
		t.Skip("set MOUNT123_FUSE_TEST=1 to exercise kernel FUSE")
	}
	for _, layout := range []string{"bytes", "disks"} {
		entryName, parts, payloads := generatedSplitZIP(t, layout, "deflate", "AES256")
		for _, nested := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/nested=%t", layout, nested), func(t *testing.T) {
				var root *Node
				var components []string
				if nested {
					outer := multilevelFixture(t, "7z", parts)
					root, _ = otherArchiveFixture(t, "outer.7z", outer, []byte(splitFixturePassword), false)
					components = []string{"outer.7z", entryName}
				} else {
					_, root, _ = rootZIPSplitPartsFixture(t, parts, strings.TrimSuffix(entryName, ".001"), []byte(splitFixturePassword), Options{PrefetchFiles: -1, DisableReadAhead: true})
					components = []string{entryName}
				}
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
				directory := point
				for _, component := range components {
					entries, err := os.ReadDir(directory)
					if err != nil {
						t.Fatal(err)
					}
					found := false
					for _, entry := range entries {
						if entry.Name() == component {
							found = entry.IsDir()
						}
					}
					if !found {
						t.Fatal("canonical ZIP volume must remain a directory after enumeration")
					}
					directory = filepath.Join(directory, component)
				}
				entries, err := os.ReadDir(directory)
				if err != nil || len(entries) != len(payloads) {
					t.Fatalf("kernel split listing: count=%d error=%v", len(entries), err)
				}
				for _, entry := range entries {
					want, ok := payloads[entry.Name()]
					if !ok || entry.IsDir() {
						t.Fatal("kernel returned repeated outer entries instead of ZIP members")
					}
					file, err := os.Open(filepath.Join(directory, entry.Name()))
					if err != nil {
						t.Fatal(err)
					}
					got := make([]byte, 4096)
					n, err := file.ReadAt(got, int64(len(want)-len(got)))
					_ = file.Close()
					if n != len(got) || err != nil || !bytes.Equal(got, want[len(want)-len(got):]) {
						t.Fatalf("kernel split tail: bytes=%d error=%v", n, err)
					}
				}
			})
		}
	}
}
