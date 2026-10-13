package mountfs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
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
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// Exercise the real CLI's fresh scans against kernel FUSE snapshots. The local
// entries fixture implements the completed listing contract, not webserve.
func TestActualFUSELiveAcceptanceProgressiveRARConverges(t *testing.T) {
	if os.Getenv("MOUNT123_FUSE_TEST") != "1" {
		t.Skip("set MOUNT123_FUSE_TEST=1 to exercise the kernel FUSE path")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python 3 is required for the acceptance CLI")
	}
	// Allow the same kernel fixture to compare baseline and candidate scripts
	// without modifying either implementation.
	script := os.Getenv("MOUNT123_ACCEPTANCE_TEST_SCRIPT")
	if script == "" {
		script = filepath.Join("..", "..", "tools", "live_acceptance.py")
	}
	script, err = filepath.Abs(script)
	if err != nil {
		t.Fatal(err)
	}
	const members = 128
	data := rarFixture(members, 256<<10)
	release, blocked := make(chan struct{}), make(chan struct{})
	var releaseOnce, blockedOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var start, end int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err == nil && start >= 1<<20 {
			blockedOnce.Do(func() { close(blocked) })
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("ETag", `"synthetic-progressive"`)
		http.ServeContent(w, r, "synthetic.rar", time.Unix(1, 0), io.NewSectionReader(data, 0, data.size))
	}))
	cache, err := storage.NewCache(t.TempDir(), 64<<20)
	if err != nil {
		source.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	// Register the mount directory before its cleanup so unmount runs before
	// TempDir removes this path. It must never traverse a still-mounted tree.
	point := t.TempDir()
	var mounted *fuse.Server
	var entries *httptest.Server
	t.Cleanup(func() {
		// Release the source before cancellation/unmount, including fatal paths.
		unblock()
		cancel()
		if mounted != nil {
			if err := mounted.Unmount(); err != nil {
				t.Errorf("temporary FUSE unmount failed; mount may remain (not waiting or forcing unmount): %v", err)
			} else {
				mounted.Wait()
				t.Log("temporary FUSE server stopped after unmount")
			}
		}
		if entries != nil {
			entries.Close()
		}
		source.Close()
		_ = cache.Close()
	})
	api := &archiveAPI{url: source.URL, files: []panapi.File{{ID: 77, Name: "synthetic.rar", Size: data.size, Version: "synthetic-v1"}}}
	root := NewWithOptions(ctx, api, cache, 0, true, Options{DisableReadAhead: true})
	mounted, err = fs.Mount(point, root, &fs.Options{MountOptions: fuse.MountOptions{Options: []string{"ro"}}})
	if err != nil {
		t.Fatal(err)
	}
	var entryRequests atomic.Int64
	var partialMembers atomic.Int64
	entries = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/archive/77/entries" || r.URL.Query().Get("path") != "" || r.URL.Query().Get("cursor") != "" {
			http.Error(w, "unexpected synthetic request", http.StatusBadRequest)
			return
		}
		entryRequests.Add(1)
		select {
		case <-blocked:
		case <-r.Context().Done():
			return
		case <-time.After(3 * time.Second):
			http.Error(w, "synthetic scan did not block", http.StatusInternalServerError)
			return
		}
		status, err := root.StatusArchiveIndex(r.Context(), "synthetic.rar")
		if err != nil || status.State != "scanning" || status.Members == 0 || status.Members >= members {
			http.Error(w, "synthetic first scan was not partial", http.StatusInternalServerError)
			return
		}
		partialMembers.Store(int64(status.Members))
		unblock()
		wait, stop := context.WithTimeout(r.Context(), 5*time.Second)
		defer stop()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			status, err = root.StatusArchiveIndex(wait, "synthetic.rar")
			if err != nil {
				http.Error(w, "synthetic index failed", http.StatusInternalServerError)
				return
			}
			if status.State == "complete" {
				if status.Members != members {
					http.Error(w, "synthetic index count mismatch", http.StatusInternalServerError)
					return
				}
				break
			}
			select {
			case <-ticker.C:
			case <-wait.Done():
				http.Error(w, "synthetic index wait expired", http.StatusServiceUnavailable)
				return
			}
		}
		items := make([]map[string]string, members)
		for i := range items {
			items[i] = map[string]string{"name": fmt.Sprintf("%04d.jpg", i)}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "total": members, "complete": true, "next_cursor": ""})
	}))
	artifacts := t.TempDir()
	manifestPath, reportPath := filepath.Join(artifacts, "manifest.json"), filepath.Join(artifacts, "report.json")
	manifest := map[string]any{"version": 1, "cases": []any{map[string]any{
		"name": "synthetic_progressive_rar", "kind": "archive", "order": "outer-first", "file_id": 77,
		"mount_path": "synthetic.rar", "directories": []any{map[string]any{"path": "", "archive_depth": 0, "count": members}}, "reads": []any{},
	}}}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	cliContext, stopCLI := context.WithTimeout(context.Background(), 22*time.Second)
	defer stopCLI()
	command := exec.CommandContext(cliContext, python, script, "--live", "--manifest", manifestPath,
		"--mount", point, "--base-url", entries.URL, "--report", reportPath,
		"--timeout", "8", "--max-seconds", "18", "--max-pages", "1", "--max-entries", "128", "--max-operations", "12", "--repeats", "1")
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "MOUNT123_ACCEPTANCE_COOKIE=") && !strings.HasPrefix(value, "MOUNT123_ACCEPTANCE_AUTHORIZATION=") {
			command.Env = append(command.Env, value)
		}
	}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		// Let Python reclaim its isolated HTTP/FUSE worker before the fallback
		// kill. Source release also makes temporary FUSE I/O return promptly.
		unblock()
		cancel()
		return syscall.Kill(-command.Process.Pid, syscall.SIGINT)
	}
	command.WaitDelay = 3 * time.Second
	output, cliErr := command.CombinedOutput()
	reportBytes, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("acceptance report missing: %v (CLI: %v)", err, cliErr)
	}
	var report struct {
		Status string `json:"status"`
		Cases  []struct {
			Status string `json:"status"`
			Error  struct {
				Code string `json:"code"`
			} `json:"error"`
			Steps []struct {
				Count       int    `json:"count"`
				NamesSHA256 string `json:"names_sha256"`
				FirstFUSE   struct {
					Status string `json:"status"`
					Count  int    `json:"count"`
				} `json:"first_fuse"`
				FUSEAttemptCount int `json:"fuse_attempt_count"`
				FUSEAttempts     []struct {
					Status string `json:"status"`
					Count  int    `json:"count"`
				} `json:"fuse_attempts"`
			} `json:"steps"`
		} `json:"cases"`
		Attempt struct {
			Operations int `json:"operations"`
		} `json:"attempt"`
	}
	if err := json.Unmarshal(reportBytes, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Cases) != 1 {
		t.Fatalf("acceptance cases=%d", len(report.Cases))
	}
	if cliErr != nil || report.Status != "passed" || report.Cases[0].Status != "passed" {
		t.Fatalf("acceptance did not converge: error=%s CLI=%v output=%s", report.Cases[0].Error.Code, cliErr, output)
	}
	if entryRequests.Load() != 1 || partialMembers.Load() <= 0 || partialMembers.Load() >= members {
		t.Fatalf("synthetic trigger: HTTP=%d partial members=%d", entryRequests.Load(), partialMembers.Load())
	}
	if len(report.Cases[0].Steps) != 1 {
		t.Fatalf("acceptance directory steps=%d", len(report.Cases[0].Steps))
	}
	step := report.Cases[0].Steps[0]
	if step.FirstFUSE.Status != "partial" || step.FirstFUSE.Count <= 0 || step.FirstFUSE.Count >= members {
		t.Fatalf("first kernel snapshot evidence: status=%s count=%d", step.FirstFUSE.Status, step.FirstFUSE.Count)
	}
	if step.Count != members || step.FUSEAttemptCount < 2 || step.FUSEAttemptCount > 9 || report.Attempt.Operations != step.FUSEAttemptCount+1 {
		t.Fatalf("convergence evidence: count=%d scans=%d operations=%d", step.Count, step.FUSEAttemptCount, report.Attempt.Operations)
	}
	if len(step.FUSEAttempts) != step.FUSEAttemptCount || step.FUSEAttempts[0].Status != "partial" || step.FUSEAttempts[0].Count != step.FirstFUSE.Count {
		t.Fatal("scan attempts omit the first partial snapshot")
	}
	last := step.FUSEAttempts[len(step.FUSEAttempts)-1]
	if last.Status != "complete" || last.Count != members {
		t.Fatalf("final fresh kernel scan: status=%s count=%d", last.Status, last.Count)
	}
	quotedNames := make([]string, members)
	for i := range quotedNames {
		quotedNames[i] = fmt.Sprintf("%q", fmt.Sprintf("%04d.jpg", i))
	}
	// Python's canonical list hash includes a space after each comma.
	hash := sha256.Sum256([]byte("[" + strings.Join(quotedNames, ", ") + "]"))
	if step.NamesSHA256 != hex.EncodeToString(hash[:]) {
		t.Fatal("completed kernel/Web name set digest differs from the fixture")
	}
	t.Logf("first_fuse=partial count=%d final=%d scans=%d HTTP=%d", step.FirstFUSE.Count, step.Count, step.FUSEAttemptCount, entryRequests.Load())
}
