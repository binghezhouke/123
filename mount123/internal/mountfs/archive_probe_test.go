package mountfs

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
	goFS "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func probeZIP(t *testing.T) []byte {
	t.Helper()
	var data bytes.Buffer
	w := zip.NewWriter(&data)
	f, err := w.Create("hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("probe fixture")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func TestProbeRecognitionChecksStructure(t *testing.T) {
	plain, err := os.ReadFile("testdata/encrypted/plain.7z")
	if err != nil {
		t.Fatal(err)
	}
	bad := append([]byte(nil), plain...)
	bad[8] ^= 1
	truncated := append([]byte(nil), plain...)
	binary.LittleEndian.PutUint64(truncated[12:20], uint64(len(plain)))
	binary.LittleEndian.PutUint32(truncated[8:12], crc32.ChecksumIEEE(truncated[12:32]))
	badNext := append([]byte(nil), plain...)
	badNext[len(badNext)-1] ^= 1
	rarVolume := storedRARImages([]byte("probe fixture"), 1)
	rarVolume[10] |= 1
	tests := []struct {
		name        string
		data        []byte
		kind, state string
	}{
		{"7z", plain, ".7z", "detected"},
		{"7z-bad-start-crc", bad, "", "invalid"},
		{"7z-bad-next-crc", badNext, "", "invalid"},
		{"7z-split-or-truncated", truncated, "", "incomplete"},
		{"7z-signature-only", []byte("7z\xbc\xaf\x27\x1c\x00\x03"), "", "invalid"},
		{"zip", probeZIP(t), ".zip", "detected"},
		{"fake-zip", []byte("PK\x03\x04not a real zip archive"), "", "unknown"},
		{"rar", storedRARImages([]byte("probe fixture"), 1), ".rar", "detected"},
		{"rar-volume", rarVolume, "", "incomplete"},
		{"ordinary", []byte("an ordinary data file"), "", "not_archive"},
	}
	for _, fixture := range []string{"headers.7z", "password.7z"} {
		data, err := os.ReadFile("testdata/encrypted/" + fixture)
		if err != nil {
			t.Fatal(err)
		}
		tests = append(tests, struct {
			name        string
			data        []byte
			kind, state string
		}{fixture, data, ".7z", "detected"})
	}
	for _, fixture := range []string{"rar5-crc.rar", "rar5-hpsw.rar"} {
		data, err := os.ReadFile(filepath.Join("../../../tests/fixtures/rar", fixture))
		if err != nil {
			t.Fatal(err)
		}
		tests = append(tests, struct {
			name        string
			data        []byte
			kind, state string
		}{fixture, data, ".rar", "detected"})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind, state := detectArchiveFormat(bytes.NewReader(tt.data), int64(len(tt.data)))
			if kind != tt.kind || state != tt.state {
				t.Fatalf("kind=%s state=%s; want %s/%s", kind, state, tt.kind, tt.state)
			}
		})
	}
}

type probeAPI struct {
	*archiveAPI
	resolves atomic.Int64
}

func (*probeAPI) CacheIdentity() string { return "probe-fixture-account" }
func (a *probeAPI) DownloadURL(ctx context.Context, id int64) (string, error) {
	a.resolves.Add(1)
	return a.archiveAPI.DownloadURL(ctx, id)
}

func probeFixture(t *testing.T, name string, data, password []byte) (*Node, *probeAPI, *storage.Cache) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"fixture"`)
		http.ServeContent(w, r, "source", time.Unix(1, 0), bytes.NewReader(data))
	}))
	t.Cleanup(server.Close)
	cache, err := storage.NewCache(t.TempDir(), 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	api := &probeAPI{archiveAPI: &archiveAPI{url: server.URL, password: password, files: []panapi.File{{ID: 77, Name: name, Size: int64(len(data)), Version: "v1"}}}}
	if len(password) > 0 {
		api.files = append(api.files, panapi.File{ID: 88, Name: name + ".pwd", Size: int64(len(password)), Version: "p1"})
	}
	ctx, cancel := context.WithCancel(context.Background())
	root := New(ctx, api, cache, 0, true)
	goFS.NewNodeFS(root, &goFS.Options{})
	t.Cleanup(func() { cancel(); waitProbeStopped(t, root) })
	return root, api, cache
}

func waitProbeStopped(t *testing.T, root *Node) ArchiveProbeStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status := root.probeStatus()
		if status.State != "running" {
			return status
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("probe did not finish")
	return ArchiveProbeStatus{}
}

func readProbeControl(t *testing.T, root *Node, name string) ArchiveProbeStatus {
	t.Helper()
	inode, errno := root.Lookup(context.Background(), name, &fuse.EntryOut{})
	if errno != 0 {
		t.Fatal(errno)
	}
	h, flags, errno := inode.Operations().(goFS.NodeOpener).Open(context.Background(), syscall.O_RDONLY)
	if errno != 0 {
		t.Fatal(errno)
	}
	defer h.(goFS.FileReleaser).Release(context.Background())
	if flags&fuse.FOPEN_DIRECT_IO == 0 {
		t.Fatal("control response is cacheable")
	}
	var result ArchiveProbeStatus
	if err := json.Unmarshal([]byte(readRefreshHandle(t, h)), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestProbeControlExplicitReadAndAlias(t *testing.T) {
	data := probeZIP(t)
	root, api, _ := probeFixture(t, "NO.001", data, nil)
	entries, err := root.list(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries["NO.001"].directory {
		t.Fatal("listing inferred a format from a number")
	}
	for _, name := range []string{probeControlName, probeStatusControlName} {
		inode, errno := root.Lookup(context.Background(), name, &fuse.EntryOut{})
		if errno != 0 {
			t.Fatal(errno)
		}
		control := inode.Operations()
		if errno := control.(goFS.NodeGetattrer).Getattr(context.Background(), nil, &fuse.AttrOut{}); errno != 0 {
			t.Fatal(errno)
		}
		if _, _, errno := control.(goFS.NodeOpener).Open(context.Background(), syscall.O_WRONLY); errno != syscall.EROFS {
			t.Fatal(errno)
		}
	}
	if readProbeControl(t, root, probeStatusControlName).State != "idle" || api.resolves.Load() != 0 {
		t.Fatal("passive lookup/status probed files")
	}
	readProbeControl(t, root, probeControlName)
	status := waitProbeStopped(t, root)
	if status.State != "complete" || status.Detected != 1 || status.Failed != 0 {
		t.Fatalf("status=%+v", status)
	}
	entries, err = root.list(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries["NO.001"].directory || !entries["NO.001.zip"].directory {
		t.Fatal("raw file and directory alias did not coexist")
	}
	alias := lookup(t, root, "NO.001.zip")
	if got := readNode(t, lookup(t, alias, "hello.txt"), 0, 100); !bytes.Equal(got, []byte("probe fixture")) {
		t.Fatalf("member=%q", got)
	}
	if got := readNode(t, lookup(t, root, "NO.001"), 0, len(data)+1); !bytes.Equal(got, data) {
		t.Fatal("raw bytes changed")
	}
	before := api.resolves.Load()
	readProbeControl(t, root, probeControlName)
	status = waitProbeStopped(t, root)
	if status.Cached != 1 || api.resolves.Load() != before {
		t.Fatalf("second scan did not reuse detection: %+v", status)
	}
}

func TestProbeEncrypted7zOriginalPasswordAndRestart(t *testing.T) {
	data, err := os.ReadFile("testdata/encrypted/headers.7z")
	if err != nil {
		t.Fatal(err)
	}
	root, api, cache := probeFixture(t, "NO.001", data, []byte("mount-test-password"))
	readProbeControl(t, root, probeControlName)
	if status := waitProbeStopped(t, root); status.Detected != 1 || status.Persistence != "disk" {
		t.Fatalf("status=%+v", status)
	}
	alias := lookup(t, root, "NO.001.7z")
	entries, err := alias.list(context.Background())
	if err != nil || len(entries) == 0 {
		t.Fatalf("encrypted index: %v", err)
	}
	var checkMembers func(*Node, map[string]*entry)
	var read int
	checkMembers = func(parent *Node, children map[string]*entry) {
		for name, item := range children {
			child := lookup(t, parent, name)
			if item.directory {
				nested, err := child.list(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				checkMembers(child, nested)
			} else {
				if got := readNode(t, child, 0, int(item.member.size)+1); uint64(len(got)) != item.member.size {
					t.Fatal("short member")
				}
				read++
			}
		}
	}
	checkMembers(alias, entries)
	if read == 0 {
		t.Fatal("no decrypted members read")
	}
	before := api.resolves.Load()
	restarted := New(context.Background(), api, cache, 0, true)
	goFS.NewNodeFS(restarted, &goFS.Options{})
	lookup(t, restarted, "NO.001.7z")
	if api.resolves.Load() != before {
		t.Fatal("restart listing performed a source probe")
	}
	api.files[0].Version = "v2"
	if _, err := restarted.RefreshDirectory(context.Background(), "."); err != nil {
		t.Fatal(err)
	}
	if _, errno := restarted.Lookup(context.Background(), "NO.001.7z", &fuse.EntryOut{}); errno != syscall.ENOENT {
		t.Fatalf("old detection survived content change: %v", errno)
	}
}

func TestProbeAliasAndControlNameCollisions(t *testing.T) {
	root, api, _ := probeFixture(t, "NO.001", probeZIP(t), nil)
	api.files = append(api.files, panapi.File{ID: 90, Name: "NO.001.zip", Size: 40, Version: "v1"}, panapi.File{ID: 91, Name: probeControlName, Size: 4}, panapi.File{ID: 92, Name: probeStatusControlName, Size: 4})
	readProbeControl(t, root, probeControlName)
	waitProbeStopped(t, root)
	entries, err := root.list(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for name, item := range entries {
		if isDirectoryControlName(name) {
			t.Fatalf("control enumerated: %s", name)
		}
		if name == "NO.001.zip" && item.cloud.ID != 90 {
			t.Fatal("alias hid cloud item")
		}
	}
	for _, name := range []string{"NO.001 [id=77].zip", ".mount123-probe [id=91]", ".mount123-probe-status [id=92]"} {
		if entries[name] == nil {
			t.Fatalf("missing collision alias %q; entries=%v", name, entries)
		}
	}
}

func TestActualFUSEArchiveProbe(t *testing.T) {
	if os.Getenv("MOUNT123_FUSE_TEST") != "1" {
		t.Skip("set MOUNT123_FUSE_TEST=1")
	}
	root, api, _ := probeFixture(t, "NO.001", probeZIP(t), nil)
	point := t.TempDir()
	hour := time.Hour
	server, err := goFS.Mount(point, root, &goFS.Options{MountOptions: fuse.MountOptions{Options: []string{"ro"}}, EntryTimeout: &hour, AttrTimeout: &hour, NegativeTimeout: &hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := server.Unmount(); err != nil {
			t.Error(err)
		}
		server.Wait()
	}()
	for _, name := range []string{probeControlName, probeStatusControlName} {
		if _, err := os.Stat(filepath.Join(point, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := filepath.WalkDir(point, func(path string, e os.DirEntry, err error) error {
		if err == nil && isDirectoryControlName(e.Name()) {
			t.Error("find discovered a control")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if api.resolves.Load() != 0 {
		t.Fatal("stat/find triggered source I/O")
	}
	if _, err := os.Stat(filepath.Join(point, "NO.001.zip")); !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("pre-scan alias=%v", err)
	}
	f, err := os.Open(filepath.Join(point, probeControlName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(f); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if status := waitProbeStopped(t, root); status.Detected != 1 {
		t.Fatalf("status=%+v", status)
	}
	if data, err := os.ReadFile(filepath.Join(point, probeStatusControlName)); err != nil || !strings.Contains(string(data), `"state":"complete"`) {
		t.Fatalf("status=%s %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(point, "NO.001.zip", "hello.txt")); err != nil || string(data) != "probe fixture" {
		t.Fatalf("alias blocked by negative dentry: %q %v", data, err)
	}
	if err := os.WriteFile(filepath.Join(point, probeControlName), []byte("start"), 0600); !errors.Is(err, syscall.EROFS) {
		t.Fatalf("write=%v", err)
	}
	api.files[0].Version = "v2"
	if _, err := root.RefreshDirectory(context.Background(), "."); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(point, "NO.001.zip")); !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("kernel retained obsolete alias: %v", err)
	}
}

func TestProbeCancellationAndCoalescing(t *testing.T) {
	data := probeZIP(t)
	root, api, _ := probeFixture(t, "NO.001", data, nil)
	ctx, cancel := context.WithCancel(root.tree.ctx)
	root.tree.ctx = ctx
	defer func() { cancel(); waitProbeStopped(t, root) }()
	entered := make(chan struct{}, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "bytes=0-0" {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-r.Context().Done()
			return
		}
		w.Header().Set("ETag", `"fixture"`)
		http.ServeContent(w, r, "source", time.Unix(1, 0), bytes.NewReader(data))
	}))
	t.Cleanup(server.Close)
	api.url = server.URL
	readProbeControl(t, root, probeControlName)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("probe did not reach header read")
	}
	for i := 0; i < 5; i++ {
		if status := readProbeControl(t, root, probeControlName); status.State != "running" || status.Checked != 0 {
			t.Fatalf("duplicate probe was not merged: %+v", status)
		}
		if status := readProbeControl(t, root, probeStatusControlName); status.State != "running" {
			t.Fatal(status)
		}
	}
	if api.resolves.Load() != 1 {
		t.Fatalf("duplicate source resolution: %d", api.resolves.Load())
	}
	cancel()
	if status := waitProbeStopped(t, root); status.State != "cancelled" {
		t.Fatalf("status=%+v", status)
	}
	if len(root.tree.probeGate) != 0 {
		t.Fatal("cancelled scan retained a worker slot")
	}
}
