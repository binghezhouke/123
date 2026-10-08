//go:build linux

package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/mountfs"
	"github.com/binghezhouke/123/mount123/internal/panapi"
)

type unlockTestAPI struct {
	archive   panapi.File
	url       string
	saved     []byte
	saveCalls int
}

func (a *unlockTestAPI) Detail(context.Context, int64) (panapi.File, error) { return a.archive, nil }
func (a *unlockTestAPI) DownloadURL(context.Context, int64) (string, error) { return a.url, nil }
func (a *unlockTestAPI) SaveZIPPassword(_ context.Context, _ panapi.File, password []byte) (panapi.File, error) {
	a.saveCalls++
	a.saved = append([]byte(nil), password...)
	return panapi.File{ID: 9, Name: a.archive.Name + ".pwd"}, nil
}

func encryptedFixtureAPI(t *testing.T, id int64, name string) *unlockTestAPI {
	t.Helper()
	archiveBytes, err := os.ReadFile(filepath.Join("..", "..", "internal", "mountfs", "testdata", "encrypted", "aes256-deflate.zip"))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"encrypted-fixture-v1"`)
		http.ServeContent(w, r, "encrypted.zip", time.Unix(1, 0), bytes.NewReader(archiveBytes))
	}))
	t.Cleanup(server.Close)
	return &unlockTestAPI{archive: panapi.File{ID: id, ParentID: 4, Name: name, Size: int64(len(archiveBytes)), Version: "v1"}, url: server.URL}
}

func TestUnlockValidatesBeforeSavingSidecar(t *testing.T) {
	api := encryptedFixtureAPI(t, 42, "aes256-deflate.zip")
	config := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(config, []byte(`{"CLIENT_ID":"id","CLIENT_SECRET":"secret"}`), 0600); err != nil {
		t.Fatal(err)
	}
	var gotConfig panapi.Config
	deps := unlockDeps{
		newAPI: func(cfg panapi.Config) (unlockAPI, error) {
			gotConfig = cfg
			return api, nil
		},
		validate: mountfs.ValidateZIPPassword,
	}
	cacheRoot := filepath.Join(t.TempDir(), "private-cache")
	var stdout strings.Builder
	err := runUnlockWith(context.Background(), []string{"-file-id", "42", "-config", config, "-cache-dir", cacheRoot, "-password-stdin"},
		strings.NewReader("mount-test-password\n"), io.Discard, &stdout, false, nil, deps)
	if err != nil {
		t.Fatal(err)
	}
	if api.saveCalls != 1 || string(api.saved) != "mount-test-password" {
		t.Fatalf("sidecar save calls=%d bytes=%q", api.saveCalls, api.saved)
	}
	if !strings.Contains(stdout.String(), "aes256-deflate.zip") {
		t.Fatalf("missing success result: %q", stdout.String())
	}
	if gotConfig.TokenCache != filepath.Join(cacheRoot, "token.json") || gotConfig.ClientID != "id" {
		t.Fatalf("unexpected API config: %#v", gotConfig)
	}
	entries, err := os.ReadDir(cacheRoot)
	if err != nil || len(entries) != 0 {
		t.Fatalf("temporary cache was not cleaned up: entries=%v err=%v", entries, err)
	}
}

func TestUnlockDoesNotSaveWhenPasswordValidationFails(t *testing.T) {
	api := encryptedFixtureAPI(t, 7, "bad.zip")
	config := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(config, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	deps := unlockDeps{
		newAPI:   func(panapi.Config) (unlockAPI, error) { return api, nil },
		validate: mountfs.ValidateZIPPassword,
	}
	err := runUnlockWith(context.Background(), []string{"-file-id", "7", "-config", config, "-cache-dir", filepath.Join(t.TempDir(), "cache"), "-password-stdin"},
		strings.NewReader("wrong-password\n"), io.Discard, io.Discard, false, nil, deps)
	if err == nil || !strings.Contains(err.Error(), "no sidecar was uploaded") {
		t.Fatalf("expected validation failure, got %v", err)
	}
	if api.saveCalls != 0 {
		t.Fatalf("uploaded despite failed validation: %d calls", api.saveCalls)
	}
}

func TestReadUnlockPasswordPreservesExactInputExceptOneTerminalNewline(t *testing.T) {
	got, err := readUnlockPassword(context.Background(), true, strings.NewReader("  pass  \r\n"), false, io.Discard, nil)
	if err != nil || string(got) != "  pass  " {
		t.Fatalf("read password=%q err=%v", got, err)
	}
	got, err = readUnlockPassword(context.Background(), true, strings.NewReader("one\ntwo\n"), false, io.Discard, nil)
	if err != nil || string(got) != "one\ntwo" {
		t.Fatalf("read multiline password=%q err=%v", got, err)
	}
}

func TestReadUnlockPasswordBoundsAndRequiresTTY(t *testing.T) {
	if _, err := readUnlockPassword(context.Background(), false, strings.NewReader(""), false, io.Discard, nil); err == nil {
		t.Fatal("expected non-terminal prompt rejection")
	}
	if _, err := readUnlockPassword(context.Background(), true, strings.NewReader(strings.Repeat("x", maxUnlockPasswordBytes+1)), false, io.Discard, nil); err == nil {
		t.Fatal("expected oversized password rejection")
	}
	if _, err := readUnlockPassword(context.Background(), true, strings.NewReader(string([]byte{0xff})), false, io.Discard, nil); err == nil {
		t.Fatal("expected invalid UTF-8 rejection")
	}
}

func TestCanceledHiddenPasswordReadRestoresTerminal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	release := make(chan struct{})
	restored := false
	result := make(chan error, 1)
	go func() {
		_, err := awaitTerminalPassword(ctx, func() ([]byte, error) {
			close(started)
			<-release
			return nil, nil
		}, func() error {
			restored = true
			return nil
		})
		result <- err
	}()
	<-started
	cancel()
	if err := <-result; err != context.Canceled {
		t.Fatalf("canceled read error=%v", err)
	}
	if !restored {
		t.Fatal("terminal state was not restored on cancellation")
	}
	close(release)
}

type unlockBlockingReader struct {
	started chan struct{}
	release chan struct{}
}

func (r unlockBlockingReader) Read(p []byte) (int, error) {
	select {
	case <-r.started:
	default:
		close(r.started)
	}
	<-r.release
	return copy(p, "password"), io.EOF
}

func TestCanceledPasswordStdinReadReturnsPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader := unlockBlockingReader{started: make(chan struct{}), release: make(chan struct{})}
	result := make(chan error, 1)
	go func() {
		_, err := readPasswordStdin(ctx, reader)
		result <- err
	}()
	<-reader.started
	cancel()
	if err := <-result; err != context.Canceled {
		t.Fatalf("canceled stdin read error=%v", err)
	}
	close(reader.release)
}
