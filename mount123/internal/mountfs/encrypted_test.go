package mountfs

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"syscall"
)

func encryptedRemote(t *testing.T, data []byte) *storage.Remote {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"encrypted-fixture"`)
		http.ServeContent(w, r, "archive.zip", time.Unix(100, 0), bytes.NewReader(data))
	}))
	t.Cleanup(server.Close)
	cache, err := storage.NewCache(t.TempDir(), 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	r, err := storage.NewRemote(context.Background(), cache, "encrypted-fixture", int64(len(data)), func(context.Context) (string, error) { return server.URL, nil })
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestEncryptedZIPFixtures(t *testing.T) {
	want := append(bytes.Repeat([]byte("mount123 encrypted fixture\n"), 256), byteSliceRange()...)
	files, err := filepath.Glob("testdata/encrypted/*.zip")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			src := encryptedRemote(t, data)
			tree := &Tree{opts: defaults(Options{MetadataBytes: 64 << 20})}
			idx, err := buildZIP(context.Background(), tree, src, int64(len(data)))
			if err != nil {
				t.Fatal(err)
			}
			var member *member
			for _, m := range idx.members {
				member = m
				break
			}
			if member == nil {
				t.Fatal("fixture had no member")
			}
			var got bytes.Buffer
			if err := encryptedMember(context.Background(), src, member, []byte("mount-test-password"), &got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Bytes(), want) {
				t.Fatal("decrypted fixture payload mismatch")
			}
			if err := ValidateZIPPassword(context.Background(), src, int64(len(data)), []byte("mount-test-password")); err != nil {
				t.Fatalf("validator: %v", err)
			}
			if err := ValidateZIPPassword(context.Background(), src, int64(len(data)), []byte("wrong")); !errors.Is(err, ErrWrongZIPPassword) {
				t.Fatalf("wrong password error = %v", err)
			}
		})
	}
}

func TestEncryptedZIPRejectsAESAuthenticationCorruption(t *testing.T) {
	data, err := os.ReadFile("testdata/encrypted/aes256-ae2-store.zip")
	if err != nil {
		t.Fatal(err)
	}
	data = append([]byte(nil), data...)
	// The auth code is the final ten bytes before the central directory.
	central := bytes.Index(data, []byte("PK\x01\x02"))
	if central < 10 {
		t.Fatal("central directory missing")
	}
	data[central-1] ^= 0x80
	src := encryptedRemote(t, data)
	err = ValidateZIPPassword(context.Background(), src, int64(len(data)), []byte("mount-test-password"))
	if !errors.Is(err, ErrWrongZIPPassword) {
		t.Fatalf("corrupt authentication error = %v", err)
	}
}

func TestEncryptedOpenRejectsCorruptAuthentication(t *testing.T) {
	data, err := os.ReadFile("testdata/encrypted/aes256-ae2-store.zip")
	if err != nil {
		t.Fatal(err)
	}
	data = append([]byte(nil), data...)
	central := bytes.Index(data, []byte("PK\x01\x02"))
	if central < 10 {
		t.Fatal("central directory missing")
	}
	data[central-1] ^= 1
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"bad-auth"`)
		http.ServeContent(w, r, "archive.zip", time.Unix(1, 0), bytes.NewReader(data))
	}))
	defer server.Close()
	api := &encryptedFakeAPI{url: server.URL, archive: data, password: []byte("mount-test-password")}
	cache, err := storage.NewCache(t.TempDir(), 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	root := New(context.Background(), api, cache, 0, true)
	fs.NewNodeFS(root, &fs.Options{})
	member := lookup(t, lookup(t, root, "private.zip"), "hello.txt")
	h, _, errno := member.Open(context.Background(), syscall.O_RDONLY)
	if h != nil {
		_ = h.(fs.FileReleaser).Release(context.Background())
	}
	if errno != syscall.EACCES {
		t.Fatalf("corrupt auth open errno=%v", errno)
	}
}

func TestPasswordNewlineAndBounds(t *testing.T) {
	for _, tc := range []struct{ in, want string }{{"pass\n", "pass"}, {"pass\r\n", "pass"}, {"pass\r", "pass\r"}, {" pass \n", " pass "}} {
		got, err := normalizedPassword([]byte(tc.in))
		if err != nil || string(got) != tc.want {
			t.Fatalf("normalized %q = %q, %v", tc.in, got, err)
		}
	}
	if _, err := normalizedPassword(bytes.Repeat([]byte{'x'}, maxPasswordBytes+1)); err == nil {
		t.Fatal("oversized password accepted")
	}
	if _, err := normalizedPassword([]byte{0xff}); !errors.Is(err, syscall.EACCES) {
		t.Fatalf("invalid UTF-8 password error = %v", err)
	}
}

func TestParseAESExtraRejectsMalformedFields(t *testing.T) {
	valid := []byte{1, 0, 'A', 'E', 3, byte(zip.Deflate), 0}
	extra := []byte{1, 0x99, byte(len(valid)), 0}
	extra = append(extra, valid...)
	got, ok, err := parseAESExtra(extra)
	if err != nil || !ok || got.strength != 3 || got.method != zip.Deflate || got.version != 1 {
		t.Fatalf("valid AES extra: %+v %v %v", got, ok, err)
	}
	for _, malformed := range [][]byte{{1, 0x99, 6, 0, 1, 0, 'A', 'E', 3, 8, 0}, {1, 0x99, 7, 0, 1, 0, 'A', 'E', 3}} {
		if _, _, err := parseAESExtra(malformed); err == nil {
			t.Fatalf("accepted malformed extra %x", malformed)
		}
	}
	if _, _, err := parseAESExtra([]byte{0xff, 0xee}); err != nil {
		t.Fatalf("stdlib-compatible trailing padding rejected: %v", err)
	}
}

type encryptedFakeAPI struct {
	url               string
	archive, password []byte
	version           string
}

func (a *encryptedFakeAPI) CacheIdentity() string { return "encrypted-test-account-api" }

func (a *encryptedFakeAPI) List(_ context.Context, id int64) ([]panapi.File, error) {
	if id != 0 {
		return nil, nil
	}
	version := a.version
	if version == "" {
		version = "v1"
	}
	return []panapi.File{{ID: 77, ParentID: 0, Name: "private.zip", Size: int64(len(a.archive)), Version: version}, {ID: 78, ParentID: 0, Name: "private.zip.pwd", Size: int64(len(a.password)), Version: "p1"}}, nil
}
func (a *encryptedFakeAPI) DownloadURL(context.Context, int64) (string, error) { return a.url, nil }
func (a *encryptedFakeAPI) ReadSmallFile(_ context.Context, id, max int64) ([]byte, error) {
	if id != 78 || int64(len(a.password)) > max {
		return nil, syscall.EACCES
	}
	return append([]byte(nil), a.password...), nil
}

func TestEncryptedMemberUsesSiblingPasswordFile(t *testing.T) {
	archive, err := os.ReadFile("testdata/encrypted/aes256-ae2-deflate.zip")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		http.ServeContent(w, r, "private.zip", time.Unix(1, 0), bytes.NewReader(archive))
	}))
	defer server.Close()
	api := &encryptedFakeAPI{url: server.URL, archive: archive, password: []byte("mount-test-password\r\n")}
	cache, err := storage.NewCache(t.TempDir(), 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	root := New(context.Background(), api, cache, 0, true)
	fs.NewNodeFS(root, &fs.Options{})
	archiveNode := lookup(t, root, "private.zip")
	memberNode := lookup(t, archiveNode, "hello.txt")
	h, _, errno := memberNode.Open(context.Background(), syscall.O_RDONLY)
	if errno != 0 {
		t.Fatalf("open encrypted member: %v", errno)
	}
	defer h.(fs.FileReleaser).Release(context.Background())
	want := append(bytes.Repeat([]byte("mount123 encrypted fixture\n"), 256), byteSliceRange()...)
	result, errno := h.(fs.FileReader).Read(context.Background(), make([]byte, len(want)), 0)
	if errno != 0 {
		t.Fatalf("read encrypted member: %v", errno)
	}
	got, status := result.Bytes(make([]byte, len(want)))
	if status != fuse.OK || !bytes.Equal(got, want) {
		t.Fatal("sibling password decryption payload mismatch")
	}
}

func TestEncryptedMemberCacheSurvivesCacheReopenAndRejectsIdentityChanges(t *testing.T) {
	archive, err := os.ReadFile("testdata/encrypted/aes256-ae2-deflate.zip")
	if err != nil {
		t.Fatal(err)
	}
	var payloadStart, payloadEnd int64
	var payloadBytes int64
	dataReader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range dataReader.File {
		if f.Name == "hello.txt" {
			payloadStart, err = f.DataOffset()
			if err != nil {
				t.Fatal(err)
			}
			payloadEnd = payloadStart + int64(f.CompressedSize64)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if header := r.Header.Get("Range"); strings.HasPrefix(header, "bytes=") {
			parts := strings.Split(strings.TrimPrefix(header, "bytes="), "-")
			if len(parts) == 2 {
				start, e1 := strconv.ParseInt(parts[0], 10, 64)
				end, e2 := strconv.ParseInt(parts[1], 10, 64)
				if e1 == nil && e2 == nil {
					lo, hi := max(start, payloadStart), min(end+1, payloadEnd)
					if hi > lo {
						payloadBytes += hi - lo
					}
				}
			}
		}
		w.Header().Set("ETag", `"stable-encrypted-v1"`)
		http.ServeContent(w, r, "private.zip", time.Unix(1, 0), bytes.NewReader(archive))
	}))
	defer server.Close()
	api := &encryptedFakeAPI{url: server.URL, archive: archive, password: []byte("mount-test-password\n")}
	cacheDir := filepath.Join(t.TempDir(), "shared-cache")
	readOnce := func() {
		t.Helper()
		cache, e := storage.NewCache(cacheDir, 16<<20)
		if e != nil {
			t.Fatal(e)
		}
		root := New(context.Background(), api, cache, 0, true)
		fs.NewNodeFS(root, &fs.Options{})
		member := lookup(t, lookup(t, root, "private.zip"), "hello.txt")
		h, _, errno := member.Open(context.Background(), syscall.O_RDONLY)
		if errno != 0 {
			t.Fatalf("open member: %v", errno)
		}
		want := append(bytes.Repeat([]byte("mount123 encrypted fixture\n"), 256), byteSliceRange()...)
		result, errno := h.(fs.FileReader).Read(context.Background(), make([]byte, len(want)), 0)
		if errno != 0 {
			h.(fs.FileReleaser).Release(context.Background())
			t.Fatalf("read member: %v", errno)
		}
		got, status := result.Bytes(make([]byte, len(want)))
		h.(fs.FileReleaser).Release(context.Background())
		if status != fuse.OK || !bytes.Equal(got, want) {
			t.Fatal("decrypted payload mismatch")
		}
		if e := cache.Close(); e != nil {
			t.Fatal(e)
		}
	}
	readOnce()
	firstPayloadBytes := payloadBytes
	if firstPayloadBytes == 0 {
		t.Fatal("first read did not fetch encrypted member data")
	}
	payloadBytes = 0
	readOnce()
	if payloadBytes != 0 {
		t.Fatalf("cache reopen fetched %d member bytes; expected a disk cache hit", payloadBytes)
	}

	// A changed password must miss the prior plaintext, and the wrong password
	// must fail before returning a file handle.
	api.password = []byte("wrong-password\n")
	cache, err := storage.NewCache(cacheDir, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	root := New(context.Background(), api, cache, 0, true)
	fs.NewNodeFS(root, &fs.Options{})
	member := lookup(t, lookup(t, root, "private.zip"), "hello.txt")
	h, _, errno := member.Open(context.Background(), syscall.O_RDONLY)
	if h != nil {
		_ = h.(fs.FileReleaser).Release(context.Background())
	}
	if errno != syscall.EACCES {
		t.Fatalf("changed password open errno=%v, want EACCES", errno)
	}
	_ = cache.Close()

	// The content version is part of the authenticated cache identity.
	api.password = []byte("mount-test-password\n")
	api.version = "v2"
	payloadBytes = 0
	readOnce()
	if payloadBytes == 0 {
		t.Fatal("changed archive version reused old decrypted bytes")
	}
}

func TestPasswordFileDuplicatesRejected(t *testing.T) {
	api := duplicatePasswordAPI{}
	tree := &Tree{ctx: context.Background(), api: api, opts: defaults(Options{}), meta: map[string]*metaItem{}, sources: map[string]*sourceCall{}, builds: make(chan struct{}, 1), passwordKeyValid: true}
	_, err := tree.passwordForArchive(context.Background(), &archiveDescriptor{id: 1, parentID: 2, name: "x.zip", size: 5, version: "v1"})
	if !errors.Is(err, syscall.EACCES) {
		t.Fatalf("duplicate password files error = %v", err)
	}
}

func TestPasswordValidatorRejectsEmptyPassword(t *testing.T) {
	if err := ValidateZIPPassword(context.Background(), nil, 0, nil); !errors.Is(err, syscall.EACCES) {
		t.Fatalf("empty password error = %v", err)
	}
}

type duplicatePasswordAPI struct{}

func (duplicatePasswordAPI) List(context.Context, int64) ([]panapi.File, error) {
	return []panapi.File{{ID: 1, Name: "x.zip.pwd", Size: 1}, {ID: 2, Name: "x.zip.pwd", Size: 1}}, nil
}
func (duplicatePasswordAPI) DownloadURL(context.Context, int64) (string, error) { return "", nil }
func (duplicatePasswordAPI) ReadSmallFile(context.Context, int64, int64) ([]byte, error) {
	return []byte("x"), nil
}

func TestActualFUSEEncryptedZIP(t *testing.T) {
	if os.Getenv("MOUNT123_FUSE_TEST") != "1" {
		t.Skip("set MOUNT123_FUSE_TEST=1 to exercise the kernel FUSE path")
	}
	archive, err := os.ReadFile("testdata/encrypted/aes256-ae2-deflate.zip")
	if err != nil {
		t.Fatal(err)
	}
	want := append(bytes.Repeat([]byte("mount123 encrypted fixture\n"), 256), byteSliceRange()...)
	for _, tc := range []struct {
		name        string
		password    []byte
		wantSuccess bool
	}{{"correct", []byte("mount-test-password\n"), true}, {"missing", nil, false}, {"wrong", []byte("wrong"), false}} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", `"v1"`)
				http.ServeContent(w, r, "private.zip", time.Unix(1, 0), bytes.NewReader(archive))
			}))
			defer server.Close()
			api := &encryptedFakeAPI{url: server.URL, archive: archive, password: tc.password}
			cache, err := storage.NewCache(t.TempDir(), 16<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer cache.Close()
			root := New(context.Background(), api, cache, 0, true)
			mountpoint := t.TempDir()
			mounted, err := fs.Mount(mountpoint, root, &fs.Options{MountOptions: fuse.MountOptions{Options: []string{"ro"}}})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := mounted.Unmount(); err != nil {
					t.Error(err)
				}
				mounted.Wait()
			}()
			data, err := os.ReadFile(filepath.Join(mountpoint, "private.zip", "hello.txt"))
			if tc.wantSuccess {
				if err != nil || !bytes.Equal(data, want) {
					t.Fatalf("encrypted FUSE read: len=%d err=%v", len(data), err)
				}
			} else if err == nil || !os.IsPermission(err) {
				t.Fatalf("expected EACCES for %s password, got %v", tc.name, err)
			}
		})
	}
}

func byteSliceRange() []byte {
	out := make([]byte, 256)
	for i := range out {
		out[i] = byte(i)
	}
	return out
}
