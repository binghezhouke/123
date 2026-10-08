package mountfs

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
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/faults"
	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
)

type doctorFixtureAPI struct {
	mu       sync.Mutex
	files    map[int64][]panapi.File
	urls     map[int64]string
	listErr  error
	urlErr   error
	password []byte
}

func (a *doctorFixtureAPI) List(_ context.Context, parent int64) ([]panapi.File, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.listErr != nil {
		return nil, a.listErr
	}
	return append([]panapi.File(nil), a.files[parent]...), nil
}
func (a *doctorFixtureAPI) DownloadURL(_ context.Context, id int64) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.urlErr != nil {
		return "", a.urlErr
	}
	return a.urls[id], nil
}
func (a *doctorFixtureAPI) ReadSmallFile(context.Context, int64, int64) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]byte(nil), a.password...), nil
}

func TestDiagnosePathDirectoryFileAndSafeFailures(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 2<<20)
	var mu sync.Mutex
	var maxServed, requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		w.Header().Set("ETag", `"doctor-v1"`)
		cw := &countingResponseWriter{ResponseWriter: w}
		http.ServeContent(cw, r, "plain.bin", time.Unix(1, 0), bytes.NewReader(data))
		mu.Lock()
		if cw.written > maxServed {
			maxServed = cw.written
		}
		mu.Unlock()
	}))
	defer server.Close()
	api := &doctorFixtureAPI{
		files: map[int64][]panapi.File{
			0:  {{ID: 10, ParentID: 0, Name: "folder", IsDir: true}, {ID: 20, ParentID: 0, Name: "plain.bin", Size: int64(len(data)), Version: "v1"}},
			10: {{ID: 11, ParentID: 10, Name: "child.txt", Size: 1, Version: "v1"}},
		},
		urls: map[int64]string{20: server.URL},
	}
	root := doctorRoot(t, api)

	dir, err := root.DiagnosePath(context.Background(), "folder", false)
	if err != nil || dir.State != "ok" || dir.Stage != "directory_metadata" {
		t.Fatalf("directory diagnosis=%+v err=%v", dir, err)
	}
	file, err := root.DiagnosePath(context.Background(), "plain.bin", false)
	if err != nil || file.State != "ok" || file.Stage != "sample_read" || file.FileID == nil || *file.FileID != 20 {
		t.Fatalf("file diagnosis=%+v err=%v", file, err)
	}
	mu.Lock()
	gotRequests, largest := requests, maxServed
	mu.Unlock()
	if gotRequests < 2 || largest > 1 {
		t.Fatalf("doctor should only request 1-byte ranges; requests=%d largest-body=%d", gotRequests, largest)
	}

	missing, err := root.DiagnosePath(context.Background(), "not-here", false)
	if err != nil || missing.State != "failed" || missing.Reason != "path_not_found" {
		t.Fatalf("missing diagnosis=%+v err=%v", missing, err)
	}
	unsafe, err := root.DiagnosePath(context.Background(), "../outside", false)
	if err != nil || unsafe.Reason != "invalid_path" {
		t.Fatalf("unsafe diagnosis=%+v err=%v", unsafe, err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := root.DiagnosePath(canceled, "plain.bin", false); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled diagnosis err=%v", err)
	}
}

func TestDiagnosePathClassifiesRemoteFailuresWithoutSecrets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "secret response body", http.StatusForbidden)
	}))
	defer server.Close()
	api := &doctorFixtureAPI{
		files: map[int64][]panapi.File{0: {{ID: 5, Name: "private.txt", Size: 3, Version: "v1"}}},
		urls:  map[int64]string{5: server.URL + "/token=secret"},
	}
	root := doctorRoot(t, api)
	report, err := root.DiagnosePath(context.Background(), "private.txt", false)
	if err != nil || report.State != "failed" || report.Stage != "source_probe" || report.Reason != "unauthorized" {
		t.Fatalf("diagnosis=%+v err=%v", report, err)
	}
	encoded, _ := json.Marshal(report)
	if strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), server.URL) {
		t.Fatalf("diagnosis leaked remote details: %s", encoded)
	}

	api.listErr = &faults.Error{Kind: faults.Throttled, Message: "bearer secret https://example.invalid/token", Retryable: true}
	root = doctorRoot(t, api) // use a cold directory snapshot to exercise List failure.
	report, err = root.DiagnosePath(context.Background(), "private.txt", false)
	if err != nil || report.Reason != "rate_limited" {
		t.Fatalf("metadata error classification=%+v err=%v", report, err)
	}
	encoded, _ = json.Marshal(report)
	if strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "example.invalid") {
		t.Fatalf("metadata diagnosis leaked upstream error: %s", encoded)
	}
}

func TestDiagnoseArchiveRetryAfterTransientIndexFailure(t *testing.T) {
	var archive bytes.Buffer
	w := zip.NewWriter(&archive)
	for name, value := range map[string]string{"hello.txt": "payload", "nested.zip": "not an archive"} {
		member, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(member, value)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	data := append([]byte(nil), archive.Bytes()...)
	server := serveDoctorData(t, data)
	api := &doctorFixtureAPI{
		files: map[int64][]panapi.File{0: {{ID: 7, Name: "archive.zip", Size: int64(len(data)), Version: "v1"}}},
		urls:  map[int64]string{7: server.URL},
	}
	root := doctorRootWithOptions(t, api, Options{MaxZIPEntries: 1})
	report, err := root.DiagnosePath(context.Background(), "archive.zip", false)
	if err != nil {
		t.Fatal(err)
	}
	report = waitDiagnosis(t, root, "archive.zip", report)
	if report.State != "failed" || report.ArchiveStatus == nil || report.ArchiveStatus.FailureReason != "unclassified_error" {
		t.Fatalf("first archive diagnosis=%+v archive=%+v", report, report.ArchiveStatus)
	}
	root.tree.opts.MaxZIPEntries = 10 // address the bounded-index limit before explicit retry.

	report, err = root.DiagnosePath(context.Background(), "archive.zip/hello.txt", true)
	if err != nil {
		t.Fatal(err)
	}
	if !report.MetadataOnly || report.Stage != "archive_member_metadata" || report.State == "failed" {
		t.Fatalf("member diagnosis should retry index without reading member content: %+v", report)
	}
	report = waitDiagnosis(t, root, "archive.zip", report)
	if report.State != "ok" || report.ArchiveStatus == nil || report.ArchiveStatus.State != "complete" {
		t.Fatalf("retried archive diagnosis=%+v", report)
	}
	missing, err := root.DiagnosePath(context.Background(), "archive.zip/missing.txt", false)
	if err != nil || missing.State != "failed" || missing.Reason != "path_not_found" {
		t.Fatalf("missing archive member diagnosis=%+v err=%v", missing, err)
	}
	nested, err := root.DiagnosePath(context.Background(), "archive.zip/nested.zip/file.txt", false)
	if err != nil || nested.State != "unchecked" || nested.Reason != "nested_archive_content_not_read" {
		t.Fatalf("nested archive diagnosis=%+v err=%v", nested, err)
	}
}

type countingResponseWriter struct {
	http.ResponseWriter
	written int
}

func (w *countingResponseWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.written += n
	return n, err
}

func TestDiagnoseUnsupportedAndPasswordFailureAreStructured(t *testing.T) {
	unsupported := failedArchiveStatus(12, syscall.EOPNOTSUPP)
	if unsupported.FailureReason != "unsupported_format" || unsupported.RecommendedAction == "" {
		t.Fatalf("unsupported status=%+v", unsupported)
	}
	password := failedArchiveStatus(12, syscall.EACCES)
	if password.FailureReason != "password_required_or_invalid" {
		t.Fatalf("password status=%+v", password)
	}
	unknown := failedArchiveStatus(12, fmt.Errorf("opaque upstream secret"))
	if unknown.FailureKind != "unknown" || unknown.FailureReason != "unclassified_error" {
		t.Fatalf("unknown error was guessed: %+v", unknown)
	}
}

func doctorRoot(t *testing.T, api *doctorFixtureAPI) *Node {
	return doctorRootWithOptions(t, api, Options{DisableReadAhead: true})
}

func doctorRootWithOptions(t *testing.T, api *doctorFixtureAPI, options Options) *Node {
	t.Helper()
	cache, err := storage.NewCache(t.TempDir(), 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	options.DisableReadAhead = true
	return NewWithOptions(context.Background(), api, cache, 0, true, options)
}

func serveDoctorData(t *testing.T, data []byte) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"doctor-data-v1"`)
		http.ServeContent(w, r, "data", time.Unix(1, 0), bytes.NewReader(data))
	}))
	t.Cleanup(server.Close)
	return server
}

func waitDiagnosis(t *testing.T, root *Node, path string, latest PathDiagnosis) PathDiagnosis {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		report, err := root.DiagnosePath(context.Background(), path, false)
		if err != nil {
			t.Fatal(err)
		}
		latest = report
		if report.ArchiveStatus != nil && (report.ArchiveStatus.State == "complete" || report.ArchiveStatus.State == "failed") {
			return report
		}
		time.Sleep(time.Millisecond)
	}
	return latest
}
