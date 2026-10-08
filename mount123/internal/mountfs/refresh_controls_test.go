package mountfs

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/binghezhouke/123/mount123/internal/workqueue"
	goFS "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

const testRefreshName = ".mount123-refresh"

func refreshTestRoot(t *testing.T) (*Node, *refreshFixtureAPI) {
	t.Helper()
	api := &refreshFixtureAPI{files: map[int64][]panapi.File{0: {{ID: 1, Name: "before.txt"}}}, listHits: map[int64]int{}}
	root := New(context.Background(), api, nil, 0, true)
	if err := root.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	return root, api
}

func readRefreshHandle(t *testing.T, h goFS.FileHandle) string {
	t.Helper()
	result, errno := h.(goFS.FileReader).Read(context.Background(), make([]byte, 1024), 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	data, status := result.Bytes(make([]byte, 1024))
	if status != fuse.OK {
		t.Fatal(status)
	}
	return string(data)
}

func TestRefreshControlOnlyOpenRefreshes(t *testing.T) {
	root, api := refreshTestRoot(t)
	goFS.NewNodeFS(root, &goFS.Options{})
	stream, errno := root.Readdir(context.Background())
	if errno != 0 {
		t.Fatal(errno)
	}
	defer stream.Close()
	for stream.HasNext() {
		entry, errno := stream.Next()
		if errno != 0 || entry.Name == testRefreshName {
			t.Fatalf("control leaked into listing: %+v %v", entry, errno)
		}
	}
	inode, errno := root.Lookup(context.Background(), testRefreshName, &fuse.EntryOut{})
	if errno != 0 {
		t.Fatalf("lookup refresh control: %v", errno)
	}
	control := inode.Operations()
	if errno := control.(goFS.NodeGetattrer).Getattr(context.Background(), nil, &fuse.AttrOut{}); errno != 0 {
		t.Fatal(errno)
	}
	if api.listHits[0] != 1 {
		t.Fatal("lookup/stat triggered refresh")
	}
	if _, _, errno := control.(goFS.NodeOpener).Open(context.Background(), syscall.O_WRONLY); errno != syscall.EROFS {
		t.Fatalf("write errno=%v", errno)
	}
	api.files[0] = []panapi.File{{ID: 2, Name: "after.txt"}, {ID: 3, Name: "another.txt"}}
	h, flags, errno := control.(goFS.NodeOpener).Open(context.Background(), syscall.O_RDONLY)
	if errno != 0 {
		t.Fatal(errno)
	}
	defer h.(goFS.FileReleaser).Release(context.Background())
	if flags&fuse.FOPEN_DIRECT_IO == 0 {
		t.Fatal("control response can be page cached")
	}
	for i := 0; i < 2; i++ {
		if got := readRefreshHandle(t, h); got != "entries=2\n" {
			t.Fatalf("refresh response=%q", got)
		}
	}
	if api.listHits[0] != 2 {
		t.Fatalf("same handle refreshed repeatedly: %d", api.listHits[0])
	}
	lookup(t, root, "after.txt")
}

func TestActualFUSERefreshControl(t *testing.T) {
	if os.Getenv("MOUNT123_FUSE_TEST") != "1" {
		t.Skip("set MOUNT123_FUSE_TEST=1")
	}
	root, api := refreshTestRoot(t)
	mountpoint := t.TempDir()
	hour := time.Hour
	server, err := goFS.Mount(mountpoint, root, &goFS.Options{MountOptions: fuse.MountOptions{Options: []string{"ro"}}, EntryTimeout: &hour, AttrTimeout: &hour, NegativeTimeout: &hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := server.Unmount(); err != nil {
			t.Error(err)
		}
		server.Wait()
	}()
	control := filepath.Join(mountpoint, testRefreshName)
	for i := 0; i < 2; i++ {
		if _, err := os.Stat(control); err != nil {
			t.Fatal(err)
		}
		if err := filepath.WalkDir(mountpoint, func(path string, entry fs.DirEntry, err error) error {
			if entry != nil && entry.Name() == testRefreshName {
				t.Error("find discovered control")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	api.mu.Lock()
	hits := api.listHits[0]
	api.mu.Unlock()
	if hits != 1 {
		t.Fatalf("stat/find refreshed: %d", hits)
	}
	if _, err := os.Stat(filepath.Join(mountpoint, "new.txt")); !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("negative lookup=%v", err)
	}
	for count := 1; count <= 2; count++ {
		api.mu.Lock()
		api.files[0] = []panapi.File{{ID: 2, Name: "new.txt"}}
		if count == 2 {
			api.files[0] = append(api.files[0], panapi.File{ID: 3, Name: "second.txt"})
		}
		api.mu.Unlock()
		data, err := exec.Command("cat", control).Output()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "entries=") {
			t.Fatalf("response=%q", data)
		}
		if _, err := os.Stat(filepath.Join(mountpoint, "new.txt")); err != nil {
			t.Fatalf("kernel negative cache not invalidated: %v", err)
		}
		if _, err := os.Stat(filepath.Join(mountpoint, "before.txt")); !errors.Is(err, syscall.ENOENT) {
			t.Fatalf("old entry remained: %v", err)
		}
	}
	if err := os.WriteFile(control, []byte("refresh"), 0600); !errors.Is(err, syscall.EROFS) {
		t.Fatalf("write=%v", err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.listHits[0] != 3 {
		t.Fatalf("repeated cat did not refresh exactly once each: %d", api.listHits[0])
	}
}

type missingPasswordAPI struct {
	*refreshFixtureAPI
	url string
}

func (*missingPasswordAPI) CacheIdentity() string                                { return "missing-password-test" }
func (a *missingPasswordAPI) DownloadURL(context.Context, int64) (string, error) { return a.url, nil }
func (*missingPasswordAPI) ReadSmallFile(context.Context, int64, int64) ([]byte, error) {
	return []byte("mount-test-password"), nil
}

func missingPasswordFixture(t *testing.T) (*missingPasswordAPI, *storage.Cache) {
	t.Helper()
	data, err := os.ReadFile("testdata/encrypted/aes256-ae2-deflate.zip")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		http.ServeContent(w, r, "private.zip", time.Unix(1, 0), bytes.NewReader(data))
	}))
	t.Cleanup(server.Close)
	cache, err := storage.NewCache(t.TempDir(), 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	api := &missingPasswordAPI{refreshFixtureAPI: &refreshFixtureAPI{files: map[int64][]panapi.File{0: {{ID: 77, Name: "private.zip", Size: int64(len(data)), Version: "v1"}}}, listHits: map[int64]int{}}, url: server.URL}
	return api, cache
}

func TestMissingPasswordRecoversPersistedDirectoryOnOpen(t *testing.T) {
	api, cache := missingPasswordFixture(t)
	root := New(context.Background(), api, cache, 0, true)
	if err := root.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A new mount restores the old disk snapshot after the web app adds .pwd.
	api.files[0] = append(api.files[0], panapi.File{ID: 78, Name: "private.zip.pwd", Size: 19, Version: "p1"})
	root = New(context.Background(), api, cache, 0, true)
	goFS.NewNodeFS(root, &goFS.Options{})
	member := lookup(t, lookup(t, root, "private.zip"), "hello.txt")
	if api.listHits[0] != 1 {
		t.Fatal("index/lookup refreshed the directory")
	}
	if h, _, errno := member.Open(workqueue.Background(context.Background()), syscall.O_RDONLY); errno != syscall.EACCES {
		if h != nil {
			_ = h.(goFS.FileReleaser).Release(context.Background())
		}
		t.Fatalf("background open=%v", errno)
	}
	if api.listHits[0] != 1 {
		t.Fatal("prefetch triggered refresh")
	}
	want := append(bytes.Repeat([]byte("mount123 encrypted fixture\n"), 256), byteSliceRange()...)
	if got := readNode(t, member, 0, len(want)+1); !bytes.Equal(got, want) {
		t.Fatal("auto-recovered content mismatch")
	}
	if api.listHits[0] != 2 {
		t.Fatalf("want exactly one recovery refresh: %d", api.listHits[0])
	}
}

func TestMissingPasswordConcurrentOpensAndCooldown(t *testing.T) {
	api, cache := missingPasswordFixture(t)
	root := New(context.Background(), api, cache, 0, true)
	goFS.NewNodeFS(root, &goFS.Options{})
	member := lookup(t, lookup(t, root, "private.zip"), "hello.txt")
	var group sync.WaitGroup
	for i := 0; i < 20; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			h, _, errno := member.Open(context.Background(), syscall.O_RDONLY)
			if h != nil {
				_ = h.(goFS.FileReleaser).Release(context.Background())
			}
			if errno != syscall.EACCES {
				t.Errorf("missing password open=%v", errno)
			}
		}()
	}
	group.Wait()
	api.mu.Lock()
	hits := api.listHits[0]
	api.files[0] = append(api.files[0], panapi.File{ID: 78, Name: "private.zip.pwd", Size: 19, Version: "p1"})
	api.mu.Unlock()
	if hits != 2 {
		t.Fatalf("concurrent/repeated failures refreshed %d times, want initial + one", hits)
	}
	if _, err := root.RefreshDirectory(context.Background(), "."); err != nil {
		t.Fatal(err)
	}
	if len(readNode(t, member, 0, 32)) != 32 {
		t.Fatal("manual refresh could not bypass recovery cooldown")
	}
}

func TestRefreshControlPreservesCloudNameAndArchiveContents(t *testing.T) {
	root, api := refreshTestRoot(t)
	api.files[0] = append(api.files[0], panapi.File{ID: 42, Name: testRefreshName})
	if _, err := root.RefreshDirectory(context.Background(), "."); err != nil {
		t.Fatal(err)
	}
	goFS.NewNodeFS(root, &goFS.Options{})
	entries, err := root.list(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if entries[testRefreshName] != nil {
		t.Fatal("real file hides control name")
	}
	found := false
	for name, entry := range entries {
		if entry.cloud.ID == 42 {
			found = true
			if lookup(t, root, name).item.cloud.ID != 42 {
				t.Fatal("alias lost real file identity")
			}
		}
	}
	if !found {
		t.Fatal("real control-named cloud file lost")
	}

	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	file, err := writer.Create(testRefreshName)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.Write([]byte("archive content"))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	archiveRoot := fixtureArchive(t, archive.Bytes())
	goFS.NewNodeFS(archiveRoot, &goFS.Options{})
	member := lookup(t, lookup(t, archiveRoot, "photos.zip"), testRefreshName)
	if got := string(readNode(t, member, 0, 100)); got != "archive content" {
		t.Fatalf("archive control-named member=%q", got)
	}
}

func TestRefreshControlCoalescesWithCLIAndWaiterCanCancel(t *testing.T) {
	root, api := refreshTestRoot(t)
	goFS.NewNodeFS(root, &goFS.Options{})
	inode, errno := root.Lookup(context.Background(), testRefreshName, &fuse.EntryOut{})
	if errno != 0 {
		t.Fatal(errno)
	}
	control := inode.Operations().(goFS.NodeOpener)
	started, release := make(chan struct{}, 10), make(chan struct{})
	api.started, api.block = started, release
	done := make(chan error, 1)
	go func() { _, err := root.RefreshDirectory(context.Background(), "."); done <- err }()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, _, errno := control.Open(ctx, syscall.O_RDONLY); errno != syscall.ETIMEDOUT {
		t.Fatalf("waiter cancellation=%v", errno)
	}
	var group sync.WaitGroup
	var entered atomic.Int32
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			entered.Add(1)
			h, _, errno := control.Open(context.Background(), syscall.O_RDONLY)
			if errno != 0 {
				t.Errorf("coalesced control open=%v", errno)
				return
			}
			_ = h.(goFS.FileReleaser).Release(context.Background())
		}()
	}
	for entered.Load() != 8 {
		time.Sleep(time.Millisecond)
	}
	// All callers must enter the open request while the API remains blocked.
	time.Sleep(20 * time.Millisecond)
	close(release)
	group.Wait()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.listHits[0] != 2 {
		t.Fatalf("parallel explicit refreshes made %d list calls", api.listHits[0])
	}
}

func TestMissingPasswordRefreshFailureAndCancellation(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprint("canceled=", canceled), func(t *testing.T) {
			api, cache := missingPasswordFixture(t)
			root := New(context.Background(), api, cache, 0, true)
			goFS.NewNodeFS(root, &goFS.Options{})
			member := lookup(t, lookup(t, root, "private.zip"), "hello.txt")
			if canceled {
				started := make(chan struct{}, 1)
				api.block, api.started = make(chan struct{}), started
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan syscall.Errno, 1)
				go func() { _, _, errno := member.Open(ctx, syscall.O_RDONLY); done <- errno }()
				<-started
				cancel()
				if errno := <-done; errno != syscall.EINTR {
					t.Fatalf("canceled recovery=%v", errno)
				}
				api.mu.Lock()
				api.block = nil
				api.mu.Unlock()
			} else {
				api.err = syscall.EIO
				for i := 0; i < 3; i++ {
					if _, _, errno := member.Open(context.Background(), syscall.O_RDONLY); errno != syscall.EIO {
						t.Fatalf("failed refresh=%v", errno)
					}
				}
				if api.listHits[0] != 2 {
					t.Fatal("failed automatic refresh ignored cooldown")
				}
				if _, err := root.list(context.Background()); err != nil {
					t.Fatal("failed refresh removed old listing")
				}
				api.err = nil
			}
			api.mu.Lock()
			api.files[0] = append(api.files[0], panapi.File{ID: 78, Name: "private.zip.pwd", Size: 19, Version: "p1"})
			api.mu.Unlock()
			if !canceled {
				if _, err := root.RefreshDirectory(context.Background(), "."); err != nil {
					t.Fatal(err)
				}
			}
			if len(readNode(t, member, 0, 32)) != 32 {
				t.Fatal("refresh did not recover after failure/cancellation")
			}
		})
	}
}

func TestActualFUSEMissingPasswordRefresh(t *testing.T) {
	if os.Getenv("MOUNT123_FUSE_TEST") != "1" {
		t.Skip("set MOUNT123_FUSE_TEST=1")
	}
	api, cache := missingPasswordFixture(t)
	root := New(context.Background(), api, cache, 0, true)
	if err := root.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	mountpoint := t.TempDir()
	hour := time.Hour
	server, err := goFS.Mount(mountpoint, root, &goFS.Options{MountOptions: fuse.MountOptions{Options: []string{"ro"}}, NegativeTimeout: &hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := server.Unmount(); err != nil {
			t.Error(err)
		}
		server.Wait()
	}()
	passwordPath := filepath.Join(mountpoint, "private.zip.pwd")
	if _, err := os.Stat(passwordPath); !errors.Is(err, syscall.ENOENT) {
		t.Fatal(err)
	}
	api.mu.Lock()
	api.files[0] = append(api.files[0], panapi.File{ID: 78, Name: "private.zip.pwd", Size: 19, Version: "p1"})
	api.mu.Unlock()
	data, err := os.ReadFile(filepath.Join(mountpoint, "private.zip", "hello.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want := append(bytes.Repeat([]byte("mount123 encrypted fixture\n"), 256), byteSliceRange()...)
	if !bytes.Equal(data, want) {
		t.Fatal("kernel mounted decrypted bytes mismatch")
	}
	if _, err := os.Stat(passwordPath); err != nil {
		t.Fatalf("recovery did not notify kernel of sidecar: %v", err)
	}
}
