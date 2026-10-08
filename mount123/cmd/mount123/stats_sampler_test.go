//go:build linux

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/iostats"
	"github.com/binghezhouke/123/mount123/internal/mountfs"
	"github.com/binghezhouke/123/mount123/internal/storage"
)

func TestStatsSamplerWritesStartupIntervalsFinalAndPrivateJSONL(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, statsFileName)
	var samples atomic.Uint64
	var logs bytes.Buffer
	sampler := startStatsSampler(context.Background(), path, 15*time.Millisecond, func() iostats.Snapshot {
		samples.Add(1)
		return iostats.New().Snapshot()
	}, func(format string, args ...any) { _, _ = fmt.Fprintf(&logs, format+"\n", args...) })
	time.Sleep(45 * time.Millisecond)
	sampler.Close()
	if got := samples.Load(); got < 4 {
		t.Fatalf("snapshot calls = %d, want startup, intervals and final sample", got)
	}
	if logs.Len() != 0 {
		t.Fatalf("unexpected sampler log: %s", logs.String())
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("statistics file mode = %04o, want 0600", info.Mode().Perm())
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	var records []statsRecord
	for scanner.Scan() {
		var record statsRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("invalid JSONL record: %v", err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(records) < 4 {
		t.Fatalf("JSONL records = %d, want at least 4", len(records))
	}
	if records[0].Interval != nil {
		t.Fatal("startup record interval must be null")
	}
	if records[1].Interval == nil || records[len(records)-1].Interval == nil {
		t.Fatal("subsequent and final records must include interval analysis")
	}
	if strings.Contains(string(mustReadFile(t, path)), dir) {
		t.Fatal("statistics record unexpectedly contains local path")
	}
}

func TestStatsSamplerCanBeDisabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), statsFileName)
	called := atomic.Bool{}
	if sampler := startStatsSampler(context.Background(), path, 0, func() iostats.Snapshot {
		called.Store(true)
		return iostats.New().Snapshot()
	}, nil); sampler != nil {
		t.Fatal("zero interval returned an active sampler")
	}
	if called.Load() {
		t.Fatal("disabled sampler collected a snapshot")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("disabled sampler created a file: %v", err)
	}
}

func TestStatsRotationKeepsOnePrivatePreviousFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, statsFileName)
	first := bytes.Repeat([]byte("a"), 5<<20)
	second := bytes.Repeat([]byte("b"), 5<<20)
	third := bytes.Repeat([]byte("c"), 5<<20)
	for _, line := range [][]byte{first, second, third} {
		if err := appendStatsLine(path, line, false); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{path, path + ".1"} {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatalf("%s mode = %04o, want 0600", filepath.Base(name), info.Mode().Perm())
		}
		if info.Size() != int64(len(third)) {
			t.Fatalf("%s size = %d, want %d", filepath.Base(name), info.Size(), len(third))
		}
	}
	current, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(current, third) {
		t.Fatalf("current file is not newest record: err=%v", err)
	}
	previous, err := os.ReadFile(path + ".1")
	if err != nil || !bytes.Equal(previous, second) {
		t.Fatalf(".1 does not retain the immediately previous file: err=%v", err)
	}
}

func TestIOStatsControlSamplingCountAndCancellation(t *testing.T) {
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
	if err := runControlCommand(context.Background(), []string{"io-stats", "-control-socket", socketPath, "-interval", "1ms", "-count", "2"}, io.Discard, &output); err != nil {
		t.Fatal(err)
	}
	if lines := bytes.Count(output.Bytes(), []byte("\n")); lines != 2 {
		t.Fatalf("sampled io-stats lines = %d, want 2", lines)
	}
	scanner := bufio.NewScanner(bytes.NewReader(output.Bytes()))
	var first, second statsRecord
	if !scanner.Scan() || json.Unmarshal(scanner.Bytes(), &first) != nil || !scanner.Scan() || json.Unmarshal(scanner.Bytes(), &second) != nil {
		t.Fatalf("could not decode sampled io-stats output: %s", output.String())
	}
	if first.Interval != nil || second.Interval == nil {
		t.Fatal("interval records must have null first interval and measured second interval")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Millisecond)
	defer cancel()
	output.Reset()
	err = runControlCommand(ctx, []string{"io-stats", "-control-socket", socketPath, "-interval", "5ms"}, io.Discard, &output)
	var exitErr *commandExitError
	if !errors.As(err, &exitErr) || exitErr.code != controlExitCanceled {
		t.Fatalf("continuous io-stats cancellation error = %v, want exit code %d", err, controlExitCanceled)
	}
	if lines := bytes.Count(output.Bytes(), []byte("\n")); lines < 1 {
		t.Fatalf("continuous io-stats emitted no samples before cancellation: %s", output.String())
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
