//go:build linux

package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/iostats"
	"github.com/binghezhouke/123/mount123/internal/mountfs"
	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func TestRequestControlStatusAllowsDeepPathResolutionBeyondFiveSeconds(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "control.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socketPath, 0600); err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var request controlRequest
		if err := json.NewDecoder(conn).Decode(&request); err != nil {
			return
		}
		time.Sleep(5200 * time.Millisecond)
		_ = json.NewEncoder(conn).Encode(controlResponse{Status: &mountfs.ArchiveIndexStatus{
			State: "scanning", Members: 3, ScanOffset: 12, ArchiveSize: 34,
		}})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	status, err := requestControlStatus(ctx, socketPath, "deep/archive.7z")
	if err != nil {
		t.Fatalf("status request timed out before the server response: %v", err)
	}
	if status.State != "scanning" || status.Members != 3 {
		t.Fatalf("unexpected status: %#v", status)
	}
}

type controlTestAPI struct {
	url   string
	size  int64
	files []panapi.File
}

func TestIOStatsControlCommandReturnsAggregateJSONWithoutPath(t *testing.T) {
	cache, err := storage.NewCache(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	root := mountfs.New(context.Background(), &controlTestAPI{}, cache, 0, true)
	socketDir := t.TempDir()
	if err := os.Chmod(socketDir, 0700); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(socketDir, "control.sock")
	control, err := startControlServer(socketPath, root)
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, stopServe := context.WithCancel(context.Background())
	go control.serve(serveCtx)
	defer func() { stopServe(); control.close() }()

	var output bytes.Buffer
	if err := runControlCommand(context.Background(), []string{"io-stats", "-control-socket", socketPath}, io.Discard, &output); err != nil {
		t.Fatal(err)
	}
	var snapshot iostats.Snapshot
	if err := json.Unmarshal(output.Bytes(), &snapshot); err != nil {
		t.Fatalf("io-stats did not return JSON: %v (%s)", err, output.String())
	}
	if snapshot.ForegroundReadLatency.Status != "unknown" || snapshot.DirectoryLookup.Status != "unknown" {
		t.Fatalf("unobserved metrics are not explicit: %#v", snapshot)
	}
	if bytes.Contains(output.Bytes(), []byte("signature")) || bytes.Contains(output.Bytes(), []byte("password")) {
		t.Fatalf("stats response unexpectedly exposes sensitive field names: %s", output.String())
	}
}

func (a *controlTestAPI) List(context.Context, int64) ([]panapi.File, error) {
	return append([]panapi.File(nil), a.files...), nil
}
func (a *controlTestAPI) DownloadURL(context.Context, int64) (string, error) { return a.url, nil }

type controlRefreshAPI struct {
	mu    sync.Mutex
	files map[int64][]panapi.File
}

type controlDoctorAPI struct {
	files  []panapi.File
	url    string
	urlErr error
}

func (a *controlDoctorAPI) List(context.Context, int64) ([]panapi.File, error) {
	return append([]panapi.File(nil), a.files...), nil
}
func (a *controlDoctorAPI) DownloadURL(context.Context, int64) (string, error) {
	return a.url, a.urlErr
}

func TestDoctorCLIUsesSocketAndEmitsSafeStructuredReport(t *testing.T) {
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"doctor-cli-v1"`)
		http.ServeContent(w, r, "file", time.Unix(1, 0), bytes.NewReader([]byte("content")))
	}))
	defer httpServer.Close()
	cache, err := storage.NewCache(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	root := mountfs.New(context.Background(), &controlDoctorAPI{files: []panapi.File{{ID: 44, Name: "plain.txt", Size: 7, Version: "v1"}}, url: httpServer.URL}, cache, 0, true)
	socketDir := t.TempDir()
	if err := os.Chmod(socketDir, 0700); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(socketDir, "control.sock")
	mountpoint := t.TempDir()
	control, err := startControlServer(socketPath, root, mountpoint)
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, stopServe := context.WithCancel(context.Background())
	go control.serve(serveCtx)
	defer func() { stopServe(); control.close() }()

	var output bytes.Buffer
	path := filepath.Join(mountpoint, "plain.txt")
	if err := runControlCommand(context.Background(), []string{"doctor", "-control-socket", socketPath, "-timeout", "5s", path}, io.Discard, &output); err != nil {
		t.Fatal(err)
	}
	var report mountfs.PathDiagnosis
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatalf("doctor output is not JSON: %v (%s)", err, output.String())
	}
	if report.Path != "plain.txt" || report.State != "ok" || report.Stage != "sample_read" || report.FileID == nil || *report.FileID != 44 {
		t.Fatalf("doctor report=%+v", report)
	}

	output.Reset()
	missingErr := runControlCommand(context.Background(), []string{"doctor", "-control-socket", socketPath, "missing"}, io.Discard, &output)
	var exitErr *commandExitError
	if !errors.As(missingErr, &exitErr) || exitErr.code != controlExitFailed {
		t.Fatalf("missing-path exit=%v", missingErr)
	}
	if err := json.Unmarshal(output.Bytes(), &report); err != nil || report.Reason != "path_not_found" {
		t.Fatalf("missing-path report=%+v err=%v output=%q", report, err, output.String())
	}
}

func TestDoctorCLIRedactsSourceErrors(t *testing.T) {
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "secret remote body", http.StatusServiceUnavailable)
	}))
	defer httpServer.Close()
	cache, err := storage.NewCache(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	api := &controlDoctorAPI{
		files: []panapi.File{{ID: 45, Name: "private.bin", Size: 1, Version: "v1"}},
		url:   httpServer.URL + "/token=hidden",
	}
	root := mountfs.New(context.Background(), api, cache, 0, true)
	socketDir := t.TempDir()
	if err := os.Chmod(socketDir, 0700); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(socketDir, "control.sock")
	control, err := startControlServer(socketPath, root)
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, stopServe := context.WithCancel(context.Background())
	go control.serve(serveCtx)
	defer func() { stopServe(); control.close() }()
	var output bytes.Buffer
	err = runControlCommand(context.Background(), []string{"doctor", "-control-socket", socketPath, "private.bin"}, io.Discard, &output)
	if err == nil {
		t.Fatal("doctor returned success for failed source probe")
	}
	var report mountfs.PathDiagnosis
	if decodeErr := json.Unmarshal(output.Bytes(), &report); decodeErr != nil || report.Reason != "remote_unavailable" {
		t.Fatalf("report=%+v decode=%v output=%q", report, decodeErr, output.String())
	}
	if bytes.Contains(output.Bytes(), []byte(httpServer.URL)) || bytes.Contains(output.Bytes(), []byte("hidden")) || bytes.Contains(output.Bytes(), []byte("secret remote body")) {
		t.Fatalf("doctor output leaked remote details: %s", output.String())
	}
}

func TestControlTimeoutBoundsKeepWaitIndexDefaultAndDoctorLimit(t *testing.T) {
	missingSocket := filepath.Join(t.TempDir(), "no-control.sock")
	for _, args := range [][]string{
		{"wait-index", "-control-socket", missingSocket, "archive.zip"},
		{"wait-index", "-control-socket", missingSocket, "-timeout", "1m", "archive.zip"},
	} {
		err := runControlCommand(context.Background(), args, io.Discard, io.Discard)
		if err == nil || err.Error() == "expected one mounted archive path; flags must precede the path" {
			t.Fatalf("wait-index args %v rejected before socket access: %v", args, err)
		}
	}
	for _, tc := range []struct {
		name       string
		timeout    string
		wantReject bool
	}{{"25s accepted", "25s", false}, {"over 25s rejected", "25s1ns", true}, {"zero rejected", "0s", true}} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"doctor", "-control-socket", missingSocket, "-timeout", tc.timeout, "archive.zip"}
			err := runControlCommand(context.Background(), args, io.Discard, io.Discard)
			parameterError := err != nil && err.Error() == "expected one mounted archive path; flags must precede the path"
			if parameterError != tc.wantReject {
				t.Fatalf("doctor timeout %s rejection=%v err=%v", tc.timeout, parameterError, err)
			}
		})
	}
}

func (a *controlRefreshAPI) List(_ context.Context, parent int64) ([]panapi.File, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]panapi.File(nil), a.files[parent]...), nil
}
func (*controlRefreshAPI) DownloadURL(context.Context, int64) (string, error) { return "", nil }

func TestActualFUSEManualRefreshControl(t *testing.T) {
	if os.Getenv("MOUNT123_FUSE_TEST") != "1" {
		t.Skip("set MOUNT123_FUSE_TEST=1 to test manual refresh over FUSE and control socket")
	}
	api := &controlRefreshAPI{files: map[int64][]panapi.File{
		0:  {{ID: 10, ParentID: 0, Name: "folder", IsDir: true}},
		10: {{ID: 11, ParentID: 10, Name: "before.txt", Size: 1, Version: "v1"}},
	}}
	root := mountfs.New(context.Background(), api, nil, 0, true)
	mountpoint := t.TempDir()
	server, err := fs.Mount(mountpoint, root, &fs.Options{MountOptions: fuse.MountOptions{Options: []string{"ro"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := server.Unmount(); err != nil {
			t.Error(err)
		}
		server.Wait()
	}()
	folderPath := filepath.Join(mountpoint, "folder")
	entries, err := os.ReadDir(folderPath)
	if err != nil || len(entries) != 1 || entries[0].Name() != "before.txt" {
		t.Fatalf("initial mounted listing=%v err=%v", entries, err)
	}
	api.mu.Lock()
	api.files[10] = []panapi.File{{ID: 12, ParentID: 10, Name: "after.txt", Size: 1, Version: "v2"}}
	api.mu.Unlock()
	socketDir := t.TempDir()
	if err := os.Chmod(socketDir, 0700); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(socketDir, "control.sock")
	control, err := startControlServer(socketPath, root, mountpoint)
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, stopServe := context.WithCancel(context.Background())
	go control.serve(serveCtx)
	defer func() { stopServe(); control.close() }()
	var output bytes.Buffer
	if err := runControlCommand(context.Background(), []string{"refresh", "-control-socket", socketPath, folderPath}, io.Discard, &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "entries=1\n" {
		t.Fatalf("refresh output=%q", output.String())
	}
	entries, err = os.ReadDir(folderPath)
	if err != nil || len(entries) != 1 || entries[0].Name() != "after.txt" {
		t.Fatalf("refreshed mounted listing=%v err=%v", entries, err)
	}
	if _, err := os.Stat(filepath.Join(folderPath, "before.txt")); !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("removed entry remained visible: %v", err)
	}
}

func TestControlSocketStatusWaitTimeoutAndCompletion(t *testing.T) {
	var data bytes.Buffer
	w := zip.NewWriter(&data)
	for i := 0; i < 1000; i++ {
		name := fmt.Sprintf("folder/file-%04d.txt", i)
		entry, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(entry, "data")
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	blocked := make(chan struct{})
	var once, releaseOnce sync.Once
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "bytes=0-0" && r.Method != http.MethodHead {
			once.Do(func() { close(blocked) })
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("ETag", `"control-test"`)
		http.ServeContent(w, r, "archive.zip", time.Unix(1, 0), bytes.NewReader(data.Bytes()))
	}))
	defer httpServer.Close()
	defer releaseOnce.Do(func() { close(release) })
	api := &controlTestAPI{url: httpServer.URL, size: int64(data.Len()), files: []panapi.File{{ID: 1, Name: "archive.zip", Size: int64(data.Len()), Version: "v1"}}}
	cache, err := storage.NewCache(t.TempDir(), 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	mountCtx, stopMount := context.WithCancel(context.Background())
	defer stopMount()
	root := mountfs.New(mountCtx, api, cache, 0, true)
	if err := root.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(t.TempDir(), "control.sock")
	if err := os.Chmod(filepath.Dir(socketPath), 0700); err != nil {
		t.Fatal(err)
	}
	mountpoint := t.TempDir()
	control, err := startControlServer(socketPath, root, mountpoint)
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, stopServe := context.WithCancel(context.Background())
	go control.serve(serveCtx)
	defer func() { stopServe(); control.close() }()
	info, err := os.Lstat(socketPath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("control socket mode: info=%v err=%v", info, err)
	}
	status, err := requestControlStatus(context.Background(), socketPath, "archive.zip")
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "queued" && status.State != "scanning" {
		t.Fatalf("initial state = %q, want queued or scanning", status.State)
	}
	status, err = requestControlStatus(context.Background(), socketPath, filepath.Join(mountpoint, "archive.zip"))
	if err != nil || (status.State != "queued" && status.State != "scanning") {
		t.Fatalf("absolute mounted path status = %#v, err=%v", status, err)
	}
	select {
	case <-blocked:
	case <-time.After(time.Second):
		t.Fatal("status did not start the ZIP index read")
	}
	var output bytes.Buffer
	err = runControlCommand(context.Background(), []string{"wait-index", "-control-socket", socketPath, "-timeout", "40ms", "archive.zip"}, io.Discard, &output)
	var exitErr *commandExitError
	if !errors.As(err, &exitErr) || exitErr.code != controlExitTimeout {
		t.Fatalf("wait-index while blocked = %v, want exit %d", err, controlExitTimeout)
	}
	releaseOnce.Do(func() { close(release) })
	output.Reset()
	err = runControlCommand(context.Background(), []string{"wait-index", "-control-socket", socketPath, "-timeout", "5s", "archive.zip"}, io.Discard, &output)
	if err != nil {
		t.Fatalf("wait-index after release: %v", err)
	}
	if !bytes.Contains(output.Bytes(), []byte("state=complete members=1000")) {
		t.Fatalf("wait-index output = %q", output.String())
	}
}

func TestControlSocketRejectsUnownedOrNonSocketPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.sock")
	if err := os.Chmod(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	root := mountfs.New(context.Background(), &controlTestAPI{}, nil, 0, true)
	if _, err := startControlServer(path, root); err == nil {
		t.Fatal("startControlServer accepted a regular file")
	}
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != "sentinel" {
		t.Fatalf("existing path changed: contents=%q err=%v", contents, err)
	}
}

func TestWaitIndexFailedArchiveExitCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"bad-archive"`)
		http.ServeContent(w, r, "archive.zip", time.Unix(1, 0), bytes.NewReader([]byte("not a ZIP archive")))
	}))
	defer server.Close()
	api := &controlTestAPI{url: server.URL, size: int64(len("not a ZIP archive")), files: []panapi.File{{ID: 1, Name: "archive.zip", Size: int64(len("not a ZIP archive")), Version: "v1"}}}
	cache, err := storage.NewCache(t.TempDir(), 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	root := mountfs.New(context.Background(), api, cache, 0, true)
	if err := root.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	socketDir := t.TempDir()
	if err := os.Chmod(socketDir, 0700); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(socketDir, "control.sock")
	control, err := startControlServer(socketPath, root)
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, stopServe := context.WithCancel(context.Background())
	go control.serve(serveCtx)
	defer func() { stopServe(); control.close() }()
	err = runControlCommand(context.Background(), []string{"wait-index", "-control-socket", socketPath, "-timeout", "5s", "archive.zip"}, io.Discard, io.Discard)
	var exitErr *commandExitError
	if !errors.As(err, &exitErr) || exitErr.code != controlExitFailed {
		t.Fatalf("wait-index on invalid ZIP = %v, want exit %d", err, controlExitFailed)
	}
}
