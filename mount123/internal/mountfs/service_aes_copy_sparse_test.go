package mountfs

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/faults"
	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
)

const serviceAESCopyPassword = "synthetic-aes-copy-password"

// Both inner formats index their large stored payload at the tail. The external
// archive tool independently produces the AES+Copy graph and CRC metadata.
func serviceAESCopyFixture(t *testing.T, format string) ([]byte, []byte) {
	t.Helper()
	return serviceAESCopyGenerateFixture(t, format, true, false)
}

func serviceAESCopyGenerateFixture(t *testing.T, format string, encryptedHeaders, corruptInnerCRC bool) ([]byte, []byte) {
	t.Helper()
	tool, err := exec.LookPath("7z")
	if err != nil {
		t.Skip("7z fixture generator unavailable")
	}
	payload := make([]byte, (8<<20)+13)
	for i := range payload {
		payload[i] = byte(i*29 + i/251)
	}
	dir := t.TempDir()
	innerName := "inner." + format
	var inner []byte
	switch format {
	case "zip":
		var encoded bytes.Buffer
		zw := zip.NewWriter(&encoded)
		w, err := zw.CreateHeader(&zip.FileHeader{Name: "payload.bin", Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = w.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err = zw.Close(); err != nil {
			t.Fatal(err)
		}
		inner = encoded.Bytes()
		if corruptInnerCRC {
			zr, err := zip.NewReader(bytes.NewReader(inner), int64(len(inner)))
			if err != nil {
				t.Fatal(err)
			}
			offset, err := zr.File[0].DataOffset()
			if err != nil {
				t.Fatal(err)
			}
			inner[offset+int64(len(payload))-1] ^= 1
		}
	case "7z":
		if err = os.WriteFile(filepath.Join(dir, "payload.bin"), payload, 0600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(tool, "a", "-t7z", "-m0=Copy", "-ms=off", "-mhc=off", "-mtm=off", "-mta=off", "-mtc=off", innerName, "payload.bin")
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("generate synthetic inner7z: %v: %s", err, out)
		}
		inner, err = os.ReadFile(filepath.Join(dir, innerName))
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown inner fixture format %q", format)
	}
	if len(inner)%16 == 0 {
		t.Fatal("fixture must end inside an AES block")
	}
	if err = os.WriteFile(filepath.Join(dir, innerName), inner, 0600); err != nil {
		t.Fatal(err)
	}
	headerFlag := "-mhe=off"
	if encryptedHeaders {
		headerFlag = "-mhe=on"
	}
	cmd := exec.Command(tool, "a", "-t7z", "-m0=Copy", "-ms=off", headerFlag, "-p"+serviceAESCopyPassword, "outer.7z", innerName)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate synthetic AES Copy: %v: %s", err, out)
	}
	verify := exec.Command(tool, "l", "-slt", "-p"+serviceAESCopyPassword, "outer.7z")
	verify.Dir = dir
	details, err := verify.CombinedOutput()
	if err != nil || !strings.Contains(string(details), "Method = Copy 7zAES") {
		t.Fatalf("fixture must contain encrypted Copy: error=%v", err)
	}
	outer, err := os.ReadFile(filepath.Join(dir, "outer.7z"))
	if err != nil {
		t.Fatal(err)
	}
	return outer, payload
}

type serviceAESCopyAPI struct {
	url      string
	archive  []byte
	password string
}

func (a *serviceAESCopyAPI) CacheIdentity() string { return "synthetic-aes-copy-service" }
func (a *serviceAESCopyAPI) List(ctx context.Context, id int64) ([]panapi.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if id != 0 {
		return nil, syscall.ENOENT
	}
	return []panapi.File{{ID: 77, Name: "outer.7z", Size: int64(len(a.archive)), Version: "fixture-v1"}, {ID: 78, Name: "outer.7z.pwd", Size: int64(len(a.password)), Version: "password-v1"}}, nil
}
func (a *serviceAESCopyAPI) Detail(ctx context.Context, id int64) (panapi.File, error) {
	files, err := a.List(ctx, 0)
	if err != nil {
		return panapi.File{}, err
	}
	for _, f := range files {
		if f.ID == id {
			return f, nil
		}
	}
	return panapi.File{}, syscall.ENOENT
}
func (a *serviceAESCopyAPI) Infos(ctx context.Context, ids []int64) ([]panapi.File, error) {
	out := make([]panapi.File, 0, len(ids))
	for _, id := range ids {
		f, err := a.Detail(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}
func (a *serviceAESCopyAPI) DownloadURL(ctx context.Context, id int64) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if id != 77 {
		return "", syscall.ENOENT
	}
	return a.url, nil
}
func (a *serviceAESCopyAPI) ReadSmallFile(ctx context.Context, id, limit int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if id != 78 || int64(len(a.password)) > limit {
		return nil, syscall.EACCES
	}
	return []byte(a.password), nil
}

type serviceAESCopyCountWriter struct {
	http.ResponseWriter
	bytes *atomic.Int64
}

func (w serviceAESCopyCountWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.bytes.Add(int64(n))
	return n, err
}

func serviceAESCopySource(t *testing.T, data []byte, password string) (*Service, *storage.Cache, *atomic.Int64) {
	t.Helper()
	return serviceAESCopySourceWithHook(t, data, password, nil)
}

func serviceAESCopySourceWithHook(t *testing.T, data []byte, password string, hook func(http.ResponseWriter, *http.Request) bool) (*Service, *storage.Cache, *atomic.Int64) {
	t.Helper()
	sourceBytes := new(atomic.Int64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hook != nil && hook(w, r) {
			return
		}
		w.Header().Set("ETag", `"synthetic-aes-copy"`)
		http.ServeContent(serviceAESCopyCountWriter{w, sourceBytes}, r, "outer.7z", time.Unix(100, 0), bytes.NewReader(data))
	}))
	t.Cleanup(server.Close)
	cache, err := storage.NewCache(t.TempDir(), 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	api := &serviceAESCopyAPI{url: server.URL, archive: data, password: password}
	s, err := NewService(ctx, api, cache, 0, true, Options{DisableReadAhead: true, MaxConcurrentBuilds: 1})
	if err != nil {
		t.Fatal(err)
	}
	return s, cache, sourceBytes
}

func TestServiceAESCopyNestedIndexUsesSparseSourceReads(t *testing.T) {
	for _, format := range []string{"zip", "7z"} {
		t.Run(format, func(t *testing.T) {
			innerName := "inner." + format
			data, _ := serviceAESCopyFixture(t, format)
			service, cache, sourceBytes := serviceAESCopySource(t, data, serviceAESCopyPassword)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			outer, err := service.ListArchive(ctx, 77, "")
			if err != nil || len(outer) != 1 || outer[0].Name != innerName {
				t.Fatalf("outer listing=%v,error=%v", outer, err)
			}
			// Warm only the outer index. Count source ReaderAt requests separately from
			// the HTTP cache windows; no nested index/member is opened beforehand.
			before := cache.Stats().Foreground.RequestedBytes
			httpBefore := sourceBytes.Load()
			inner, err := service.ListArchive(ctx, 77, innerName)
			if err != nil || len(inner) != 1 || inner[0].Name != "payload.bin" {
				t.Fatalf("inner listing=%v,error=%v", inner, err)
			}
			exact := cache.Stats().Foreground.RequestedBytes - before
			windows := sourceBytes.Load() - httpBefore
			t.Logf("nested index source ReaderAt requested=%d bytes; HTTP body=%d bytes; outer=%d bytes", exact, windows, len(data))
			if exact >= 256<<10 {
				t.Errorf("nested index requested %d source ReaderAt bytes, want <256 KiB", exact)
			}
			if windows >= 3<<20 {
				t.Errorf("nested index downloaded %d HTTP window bytes, want <3 MiB", windows)
			}
		})
	}
}

var _ API = (*serviceAESCopyAPI)(nil)

func TestServiceAESCopyNestedContentHeadTailAndCRC(t *testing.T) {
	for _, format := range []string{"zip", "7z"} {
		t.Run(format, func(t *testing.T) {
			data, want := serviceAESCopyFixture(t, format)
			service, _, _ := serviceAESCopySource(t, data, serviceAESCopyPassword)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			reader, size, err := service.OpenArchiveMember(ctx, 77, "inner."+format+"/payload.bin")
			if err != nil {
				t.Fatal(err)
			}
			if closer, ok := reader.(io.Closer); ok {
				defer closer.Close()
			}
			if size != int64(len(want)) {
				t.Fatalf("size=%d,want=%d", size, len(want))
			}
			for _, offset := range []int64{0, size - 4096, 17, size - 19} {
				got := make([]byte, 4096)
				n, err := reader.ReadAt(got, offset)
				length := min(len(got), len(want)-int(offset))
				if n != length || (err != nil && !errors.Is(err, io.EOF)) || !bytes.Equal(got[:n], want[int(offset):int(offset)+length]) {
					t.Fatalf("range off=%d,n=%d,err=%v,expected=%d", offset, n, err, length)
				}
			}
			// A complete ReadAt verifies the existing CRC path, beyond sparse indexing.
			got := make([]byte, len(want))
			if n, err := reader.ReadAt(got, 0); n != len(got) || err != nil || !bytes.Equal(got, want) {
				t.Fatalf("full CRC-checked read n=%d,err=%v,match=%v", n, err, bytes.Equal(got, want))
			}
		})
	}
}

func TestServiceAESCopyNestedZIPFullReadRejectsBadCRC(t *testing.T) {
	data, want := serviceAESCopyGenerateFixture(t, "zip", true, true)
	service, _, _ := serviceAESCopySource(t, data, serviceAESCopyPassword)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	reader, size, err := service.OpenArchiveMember(ctx, 77, "inner.zip/payload.bin")
	if err != nil {
		t.Fatal(err)
	}
	if closer, ok := reader.(io.Closer); ok {
		defer closer.Close()
	}
	prefix := make([]byte, 4096)
	if n, err := reader.ReadAt(prefix, 0); n != len(prefix) || err != nil || !bytes.Equal(prefix, want[:len(prefix)]) {
		t.Fatalf("valid prefix n=%d,err=%v", n, err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if n, err := reader.ReadAt(make([]byte, size), 0); !errors.Is(err, zip.ErrChecksum) {
			t.Fatalf("full read attempt=%d,n=%d,err=%v,want ZIP checksum error", attempt, n, err)
		}
	}
}

func TestServiceAESCopyClearHeadersWrongPasswordIsDenied(t *testing.T) {
	data, _ := serviceAESCopyGenerateFixture(t, "zip", false, false)
	service, _, _ := serviceAESCopySource(t, data, "wrong-synthetic-password")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Plain outer headers must list normally; denial belongs to encrypted body.
	outer, err := service.ListArchive(ctx, 77, "")
	if err != nil || len(outer) != 1 {
		t.Fatalf("clear outer headers=%v,err=%v", outer, err)
	}
	if rows, err := service.ListArchive(ctx, 77, "inner.zip"); !errors.Is(err, syscall.EACCES) {
		t.Fatalf("wrong encrypted-body password listing rows=%v,err=%v,want EACCES", rows, err)
	}
}

func TestServiceAESCopyNestedReadCancellationCanRetry(t *testing.T) {
	data, want := serviceAESCopyFixture(t, "zip")
	var armed atomic.Bool
	started := make(chan struct{})
	release := make(chan struct{})
	hook := func(w http.ResponseWriter, r *http.Request) bool {
		if armed.CompareAndSwap(true, false) {
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
		return false
	}
	service, _, _ := serviceAESCopySourceWithHook(t, data, serviceAESCopyPassword, hook)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	operation, stop := context.WithCancel(ctx)
	defer stop()
	reader, size, err := service.OpenArchiveMember(operation, 77, "inner.zip/payload.bin")
	if err != nil {
		t.Fatal(err)
	}
	if closer, ok := reader.(io.Closer); ok {
		defer closer.Close()
	}
	if size != int64(len(want)) {
		t.Fatalf("size=%d", size)
	}
	// This middle window is untouched by the sparse head/tail archive index.
	const offset = 4<<20 + 17
	armed.Store(true)
	done := make(chan error, 1)
	go func() { _, err := reader.ReadAt(make([]byte, 4096), offset); done <- err }()
	select {
	case <-started:
	case <-ctx.Done():
		close(release)
		t.Fatal("middle source range did not start")
	}
	stop()
	var readErr error
	select {
	case readErr = <-done:
	case <-ctx.Done():
		close(release)
		t.Fatal("cancelled read did not return")
	}
	close(release)
	if !errors.Is(readErr, context.Canceled) || errors.Is(readErr, syscall.EACCES) {
		t.Fatalf("cancelled encrypted read=%v,want context cancellation", readErr)
	}
	retry, _, err := service.OpenArchiveMember(ctx, 77, "inner.zip/payload.bin")
	if err != nil {
		t.Fatal(err)
	}
	if closer, ok := retry.(io.Closer); ok {
		defer closer.Close()
	}
	got := make([]byte, 4096)
	if n, err := retry.ReadAt(got, offset); n != len(got) || err != nil || !bytes.Equal(got, want[offset:offset+len(got)]) {
		t.Fatalf("retry n=%d,err=%v,match=%v", n, err, bytes.Equal(got, want[offset:offset+len(got)]))
	}
}

func TestServiceAESCopyNestedSourceFailureIsNotPasswordFailure(t *testing.T) {
	data, _ := serviceAESCopyGenerateFixture(t, "zip", false, false)
	var fail atomic.Bool
	hook := func(w http.ResponseWriter, r *http.Request) bool {
		if fail.Load() {
			http.Error(w, "synthetic source failure", http.StatusBadGateway)
			return true
		}
		return false
	}
	service, _, _ := serviceAESCopySourceWithHook(t, data, serviceAESCopyPassword, hook)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	reader, _, err := service.OpenArchiveMember(ctx, 77, "inner.zip/payload.bin")
	if err != nil {
		t.Fatal(err)
	}
	if closer, ok := reader.(io.Closer); ok {
		defer closer.Close()
	}
	fail.Store(true)
	if n, err := reader.ReadAt(make([]byte, 4096), 4<<20+17); err == nil || errors.Is(err, syscall.EACCES) {
		t.Fatalf("source failure n=%d,err=%v; must fail without EACCES", n, err)
	}
}

func TestServiceAESCopyNestedIndexSourceFailureIsNotPasswordFailure(t *testing.T) {
	data, _ := serviceAESCopyGenerateFixture(t, "zip", false, false)
	for _, failure := range []string{"http_status", "short_response"} {
		t.Run(failure, func(t *testing.T) {
			var fail atomic.Bool
			var rejected atomic.Int64
			hook := func(w http.ResponseWriter, r *http.Request) bool {
				if !fail.Load() {
					return false
				}
				rejected.Add(1)
				if failure == "http_status" {
					http.Error(w, "synthetic index source failure", http.StatusBadGateway)
					return true
				}
				var start, end int64
				if n, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); n != 2 || err != nil || start < 0 || end >= int64(len(data)) {
					http.Error(w, "unexpected synthetic range", http.StatusBadRequest)
					return true
				}
				w.Header().Set("ETag", `"synthetic-aes-copy"`)
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
				w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write(data[start : start+min(int64(32), end-start+1)])
				return true
			}
			service, _, _ := serviceAESCopySourceWithHook(t, data, serviceAESCopyPassword, hook)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := service.ListArchive(ctx, 77, ""); err != nil {
				t.Fatal(err)
			}
			// Only outer metadata is ready. The inner ZIP's encrypted tail window still
			// requires a source request during volume-hint/index preparation.
			fail.Store(true)
			rows, err := service.ListArchive(ctx, 77, "inner.zip")
			if err == nil || errors.Is(err, syscall.EACCES) || ctx.Err() != nil {
				t.Fatalf("index source failure rows=%v,err=%v,context=%v; want source error without EACCES", rows, err, ctx.Err())
			}
			if failure == "short_response" {
				var cause *faults.Error
				if !errors.As(err, &cause) || cause.Kind != faults.Network {
					t.Fatalf("short index response=%v,want preserved storage network fault", err)
				}
			}
			if rejected.Load() == 0 {
				t.Fatal("fixture did not exercise an uncached index source range")
			}
		})
	}
}

func TestServiceAESCopyNestedIndexCancellationCanRetry(t *testing.T) {
	data, _ := serviceAESCopyGenerateFixture(t, "zip", false, false)
	var armed atomic.Bool
	started := make(chan struct{})
	release := make(chan struct{})
	hook := func(w http.ResponseWriter, r *http.Request) bool {
		if armed.CompareAndSwap(true, false) {
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
		return false
	}
	service, _, _ := serviceAESCopySourceWithHook(t, data, serviceAESCopyPassword, hook)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := service.ListArchive(ctx, 77, ""); err != nil {
		t.Fatal(err)
	}
	operation, stop := context.WithCancel(ctx)
	defer stop()
	armed.Store(true)
	done := make(chan error, 1)
	go func() { _, err := service.ListArchive(operation, 77, "inner.zip"); done <- err }()
	select {
	case <-started:
	case <-ctx.Done():
		close(release)
		t.Fatal("nested index source request did not start")
	}
	stop()
	var indexErr error
	select {
	case indexErr = <-done:
	case <-ctx.Done():
		close(release)
		t.Fatal("cancelled index request did not return")
	}
	close(release)
	if !errors.Is(indexErr, context.Canceled) || errors.Is(indexErr, syscall.EACCES) {
		t.Fatalf("cancelled encrypted index=%v,want context cancellation", indexErr)
	}
	rows, err := service.ListArchive(ctx, 77, "inner.zip")
	if err != nil || len(rows) != 1 || rows[0].Name != "payload.bin" {
		t.Fatalf("retry listing=%v,error=%v", rows, err)
	}
}
