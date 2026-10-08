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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/iostats"
	"github.com/binghezhouke/123/mount123/internal/mountfs"
	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
)

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
