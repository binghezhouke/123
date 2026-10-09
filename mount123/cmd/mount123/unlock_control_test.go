//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/mountfs"
	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type mountedUnlockAPI struct {
	mu                 sync.Mutex
	files              []panapi.File
	password           []byte
	url                string
	saves              int
	started, cancelled chan struct{}
}

func (a *mountedUnlockAPI) List(context.Context, int64) ([]panapi.File, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]panapi.File(nil), a.files...), nil
}
func (a *mountedUnlockAPI) DownloadURL(context.Context, int64) (string, error) { return a.url, nil }
func (a *mountedUnlockAPI) ReadSmallFile(context.Context, int64, int64) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]byte(nil), a.password...), nil
}
func (a *mountedUnlockAPI) SaveArchivePassword(ctx context.Context, f panapi.File, pw []byte, overwrite bool) (panapi.File, bool, error) {
	if a.started != nil {
		close(a.started)
		<-ctx.Done()
		close(a.cancelled)
		return panapi.File{}, false, ctx.Err()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	name, err := panapi.PasswordSidecarName(f)
	if err != nil {
		return panapi.File{}, false, err
	}
	for _, old := range a.files {
		if old.Name == name && !overwrite {
			return old, true, nil
		}
	}
	a.password = append([]byte(nil), pw...)
	a.saves++
	sidecar := panapi.File{ID: 1000 + f.ID, ParentID: f.ParentID, Name: name, Size: int64(len(pw)), Version: "password1"}
	a.files = append(a.files, sidecar)
	return sidecar, false, nil
}

func mountedUnlockControl(t *testing.T, api *mountedUnlockAPI, actualFUSE bool) (string, string, *mountfs.Node) {
	t.Helper()
	cache, err := storage.NewCache(t.TempDir(), 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	root := mountfs.New(ctx, api, cache, 0, true)
	mountpoint := t.TempDir()
	if actualFUSE {
		hour := time.Hour
		server, err := fs.Mount(mountpoint, root, &fs.Options{MountOptions: fuse.MountOptions{Options: []string{"ro"}}, NegativeTimeout: &hour})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := server.Unmount(); err != nil {
				t.Error(err)
			}
			server.Wait()
		})
	} else {
		fs.NewNodeFS(root, &fs.Options{})
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "control.sock")
	control, err := startControlServer(socket, root, mountpoint)
	if err != nil {
		t.Fatal(err)
	}
	go control.serve(ctx)
	t.Cleanup(control.close)
	return socket, mountpoint, root
}

func TestMountedUnlockCLIUsesRunningMountAndSkipsExisting(t *testing.T) {
	api := &mountedUnlockAPI{files: []panapi.File{{ID: 1, Name: "a.zip", Version: "v1"}, {ID: 2, Name: "b.rar", Version: "v1"}, {ID: 1002, Name: "b.rar.pwd", Size: 3, Version: "p1"}, {ID: 3, Name: "ordinary.jpg", Version: "v1"}}}
	socket, mountpoint, _ := mountedUnlockControl(t, api, false)
	var output bytes.Buffer
	err := runUnlockWith(context.Background(), []string{"-all", "-skip-validation", "-control-socket", socket, "-password-stdin", mountpoint}, strings.NewReader("private-test-password\n"), io.Discard, &output, false, nil, unlockDeps{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "total=2 saved=1 skipped=1 failed=0 refreshed=true") || strings.Contains(output.String(), "private-test-password") {
		t.Fatalf("unexpected CLI result: %s", output.String())
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.saves != 1 || string(api.password) != "private-test-password" {
		t.Fatal("running mount did not save password")
	}
}

func TestMountedUnlockRejectsOutsideMountAndCancelsDisconnectedClient(t *testing.T) {
	api := &mountedUnlockAPI{files: []panapi.File{{ID: 1, Name: "a.zip", Version: "v1"}}, started: make(chan struct{}), cancelled: make(chan struct{})}
	socket, mountpoint, _ := mountedUnlockControl(t, api, false)
	_, err := requestMountedUnlock(context.Background(), socket, filepath.Join(t.TempDir(), "a.zip"), []byte("secret"), mountfs.UnlockOptions{SkipValidation: true}, time.Minute, nil)
	if err == nil || !strings.Contains(err.Error(), "must belong") {
		t.Fatalf("outside mount accepted: %v", err)
	}
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(conn).Encode(controlRequest{Command: "unlock", Path: filepath.Join(mountpoint, "a.zip"), Password: []byte("secret"), Unlock: mountfs.UnlockOptions{SkipValidation: true}}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-api.started:
	case <-time.After(time.Second):
		t.Fatal("upload did not start")
	}
	_ = conn.Close()
	select {
	case <-api.cancelled:
	case <-time.After(time.Second):
		t.Fatal("disconnect did not cancel upload")
	}
}

func TestActualFUSECLIUnlocksDetectedEncrypted7zAndInvalidatesNegativeCache(t *testing.T) {
	if os.Getenv("MOUNT123_FUSE_TEST") != "1" {
		t.Skip("set MOUNT123_FUSE_TEST=1")
	}
	data, err := os.ReadFile("../../internal/mountfs/testdata/encrypted/headers.7z")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"fixture"`)
		http.ServeContent(w, r, "archive", time.Unix(1, 0), bytes.NewReader(data))
	}))
	defer server.Close()
	api := &mountedUnlockAPI{files: []panapi.File{{ID: 77, Name: "NO.001", Size: int64(len(data)), Version: "v1"}}, url: server.URL}
	socket, mountpoint, _ := mountedUnlockControl(t, api, true)
	if _, err := os.ReadFile(filepath.Join(mountpoint, ".mount123-probe")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, err := os.ReadFile(filepath.Join(mountpoint, ".mount123-probe-status"))
		if err != nil {
			t.Fatal(err)
		}
		var status struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal(raw, &status); err != nil {
			t.Fatal(err)
		}
		if status.State != "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("probe did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	alias := filepath.Join(mountpoint, "NO.001.7z")
	if _, err := os.ReadDir(alias); !errors.Is(err, syscall.EACCES) {
		t.Fatalf("encrypted directory before unlock: %v", err)
	}
	if _, err := os.Stat(filepath.Join(mountpoint, "NO.001.pwd")); !errors.Is(err, syscall.ENOENT) {
		t.Fatal("expected missing sidecar")
	}
	var output bytes.Buffer
	args := []string{"-control-socket", socket, "-password-stdin", alias}
	if err := runUnlockWith(context.Background(), args, strings.NewReader("wrong\n"), io.Discard, &output, false, nil, unlockDeps{}); err == nil {
		t.Fatal("wrong password accepted")
	}
	if err := runUnlockWith(context.Background(), args, strings.NewReader("mount-test-password\n"), io.Discard, &output, false, nil, unlockDeps{}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(alias, "folder", "hello.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, bytes.Repeat([]byte("mount 7z fixture contents\n"), 200)) {
		t.Fatal("decrypted contents mismatch")
	}
	if _, err := os.Stat(filepath.Join(mountpoint, "NO.001.pwd")); err != nil {
		t.Fatalf("sidecar negative cache was not invalidated: %v", err)
	}
	if err := os.WriteFile(filepath.Join(mountpoint, "forbidden"), []byte("test"), 0600); !errors.Is(err, syscall.EROFS) {
		t.Fatalf("mount no longer read-only: %v", err)
	}
}
