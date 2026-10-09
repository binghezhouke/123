package mountfs

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/hanwen/go-fuse/v2/fs"
)

type passwordSaveAPI struct {
	mu    sync.Mutex
	files []panapi.File
	data  map[int64][]byte
	url   string
	saves int
	reads atomic.Int64
}

func (a *passwordSaveAPI) List(context.Context, int64) ([]panapi.File, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]panapi.File(nil), a.files...), nil
}
func (a *passwordSaveAPI) DownloadURL(_ context.Context, id int64) (string, error) {
	return fmt.Sprintf("%s/%d", a.url, id), nil
}
func (a *passwordSaveAPI) ReadSmallFile(_ context.Context, id, max int64) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]byte(nil), a.data[id]...), nil
}
func (a *passwordSaveAPI) SaveArchivePassword(ctx context.Context, archive panapi.File, password []byte, overwrite bool) (panapi.File, bool, error) {
	if err := ctx.Err(); err != nil {
		return panapi.File{}, false, err
	}
	name, err := panapi.PasswordSidecarName(archive)
	if err != nil {
		return panapi.File{}, false, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, f := range a.files {
		if f.Name == name {
			if !overwrite {
				return f, true, nil
			}
			a.saves++
			a.data[f.ID] = append([]byte(nil), password...)
			return f, false, nil
		}
	}
	f := panapi.File{ID: archive.ID + 1000, ParentID: archive.ParentID, Name: name, Size: int64(len(password)), Version: "p1"}
	a.saves++
	a.files = append(a.files, f)
	a.data[f.ID] = append([]byte(nil), password...)
	return f, false, nil
}

func passwordSaveFixture(t *testing.T, files []panapi.File, data map[int64][]byte) (*Node, *passwordSaveAPI) {
	t.Helper()
	a := &passwordSaveAPI{files: files, data: data}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.reads.Add(1)
		id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/"), 10, 64)
		a.mu.Lock()
		data := append([]byte(nil), a.data[id]...)
		a.mu.Unlock()
		w.Header().Set("ETag", `"fixture"`)
		http.ServeContent(w, r, "archive", time.Unix(1, 0), bytes.NewReader(data))
	}))
	t.Cleanup(server.Close)
	a.url = server.URL
	cache, err := storage.NewCache(t.TempDir(), 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	root := NewWithOptions(ctx, a, cache, 0, true, Options{PrefetchFiles: -1})
	fs.NewNodeFS(root, &fs.Options{})
	return root, a
}

func TestUnlockArchivesValidatesFormatsBeforeSaving(t *testing.T) {
	for _, tc := range []struct{ fixture, name, password string }{
		{"testdata/encrypted/aes256-deflate.zip", "private.zip", "mount-test-password"},
		{"testdata/encrypted/headers.7z", "private.7z", "mount-test-password"},
		{"testdata/encrypted/password.7z", "private.7z", "mount-test-password"},
		{"testdata/encrypted/solid.7z", "private.7z", "mount-test-password"},
		{"testdata/encrypted/copy.7z", "private.7z", "mount-test-password"},
		{"../../../tests/fixtures/rar/rar5-psw.rar", "private.rar", "password"},
		{"../../../tests/fixtures/rar/rar5-hpsw.rar", "private.rar", "password"},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			data, err := os.ReadFile(tc.fixture)
			if err != nil {
				t.Fatal(err)
			}
			root, api := passwordSaveFixture(t, []panapi.File{{ID: 77, Name: tc.name, Size: int64(len(data)), Version: "v1"}}, map[int64][]byte{77: data})
			summary, err := root.UnlockArchives(context.Background(), tc.name, []byte("wrong"), UnlockOptions{}, nil)
			if err != nil || summary.Failed != 1 || summary.Saved != 0 || api.saves != 0 {
				t.Fatalf("wrong password: summary=%+v err=%v saves=%d", summary, err, api.saves)
			}
			summary, err = root.UnlockArchives(context.Background(), tc.name, []byte(tc.password), UnlockOptions{}, nil)
			if err != nil || summary.Saved != 1 || !summary.Refreshed || api.saves != 1 {
				t.Fatalf("correct password: summary=%+v err=%v saves=%d", summary, err, api.saves)
			}
			if tc.name == "private.7z" {
				readOther(t, lookup(t, lookup(t, lookup(t, root, tc.name), "folder"), "hello.txt"), bytes.Repeat([]byte("mount 7z fixture contents\n"), 200))
			}
		})
	}
}

func TestUnlockDetectedBatchSkipsExistingAndDeduplicatesAliases(t *testing.T) {
	data, err := os.ReadFile("testdata/encrypted/headers.7z")
	if err != nil {
		t.Fatal(err)
	}
	root, api := passwordSaveFixture(t, []panapi.File{
		{ID: 77, Name: "NO.001", Size: int64(len(data)), Version: "v1"},
		{ID: 78, Name: "NO.002", Size: int64(len(data)), Version: "v1"},
		{ID: 1078, Name: "NO.002.pwd", Size: 3, Version: "p1"},
		{ID: 79, Name: "video.mp4", Size: 5, Version: "v1"},
	}, map[int64][]byte{77: data, 78: data, 1078: []byte("old"), 79: []byte("video")})
	readProbeControl(t, root, probeControlName)
	waitProbeStopped(t, root)
	reads := api.reads.Load()
	summary, err := root.UnlockArchives(context.Background(), ".", []byte("mount-test-password"), UnlockOptions{All: true, SkipValidation: true}, nil)
	if err != nil || summary.Total != 2 || summary.Saved != 1 || summary.Skipped != 1 || !summary.Refreshed {
		t.Fatalf("summary=%+v err=%v", summary, err)
	}
	if api.reads.Load() != reads {
		t.Fatal("skip-validation fetched archive contents")
	}
	files, _ := api.List(context.Background(), 0)
	found := false
	for _, f := range files {
		if f.Name == "NO.001.pwd" {
			found = true
		}
		if f.Name == "NO.001.7z.pwd" {
			t.Fatal("password saved for alias instead of original cloud name")
		}
	}
	if !found || string(api.data[1078]) != "old" {
		t.Fatal("missing canonical sidecar or existing password overwritten")
	}
	readOther(t, lookup(t, lookup(t, lookup(t, root, "NO.001.7z"), "folder"), "hello.txt"), bytes.Repeat([]byte("mount 7z fixture contents\n"), 200))
	summary, err = root.UnlockArchives(context.Background(), "NO.002.7z", []byte("mount-test-password"), UnlockOptions{Overwrite: true}, nil)
	if err != nil || summary.Saved != 1 {
		t.Fatalf("overwrite: %+v %v", summary, err)
	}
	if _, err = root.UnlockArchives(context.Background(), "video.mp4", []byte("password"), UnlockOptions{SkipValidation: true}, nil); !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("ordinary file accepted: %v", err)
	}
}

func TestUnlockCancelledBatchRefreshesSavedSidecars(t *testing.T) {
	root, api := passwordSaveFixture(t, []panapi.File{{ID: 1, Name: "a.zip", Version: "v1"}, {ID: 2, Name: "b.zip", Version: "v1"}}, map[int64][]byte{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	summary, err := root.UnlockArchives(ctx, ".", []byte("password"), UnlockOptions{All: true, SkipValidation: true}, func(UnlockProgress) error { cancel(); return nil })
	if !errors.Is(err, context.Canceled) || summary.Saved != 1 || !summary.Cancelled || !summary.Refreshed || api.saves != 1 {
		t.Fatalf("summary=%+v err=%v saves=%d", summary, err, api.saves)
	}
	entries, err := root.list(context.Background())
	if err != nil || entries["a.zip.pwd"] == nil || entries["b.zip.pwd"] != nil {
		t.Fatalf("partial writes not refreshed: %v", err)
	}
}

func TestUnlock7zSplitUsesCanonicalSidecar(t *testing.T) {
	data, err := os.ReadFile("testdata/encrypted/headers.7z")
	if err != nil {
		t.Fatal(err)
	}
	cut := len(data) / 2
	root, api := passwordSaveFixture(t, []panapi.File{{ID: 77, Name: "private.7z.001", Size: int64(cut), Version: "v1"}, {ID: 78, Name: "private.7z.002", Size: int64(len(data) - cut), Version: "v1"}}, map[int64][]byte{77: data[:cut], 78: data[cut:]})
	summary, err := root.UnlockArchives(context.Background(), "private.7z.001", []byte("mount-test-password"), UnlockOptions{}, nil)
	if err != nil || summary.Saved != 1 {
		t.Fatalf("summary=%+v err=%v", summary, err)
	}
	files, _ := api.List(context.Background(), 0)
	if files[len(files)-1].Name != "private.7z.pwd" {
		t.Fatalf("split password filename=%q", files[len(files)-1].Name)
	}
	readOther(t, lookup(t, lookup(t, lookup(t, root, "private.7z.001"), "folder"), "hello.txt"), bytes.Repeat([]byte("mount 7z fixture contents\n"), 200))
}

func TestUnlockValidationLimitDoesNotUpload(t *testing.T) {
	data, err := os.ReadFile("testdata/encrypted/aes256-deflate.zip")
	if err != nil {
		t.Fatal(err)
	}
	central := bytes.Index(data, []byte("PK\x01\x02"))
	if central < 0 {
		t.Fatal("missing fixture central directory")
	}
	binary.LittleEndian.PutUint32(data[central+24:central+28], passwordValidationBytes+1)
	root, api := passwordSaveFixture(t, []panapi.File{{ID: 77, Name: "large.zip", Size: int64(len(data)), Version: "v1"}}, map[int64][]byte{77: data})
	var progress UnlockProgress
	summary, err := root.UnlockArchives(context.Background(), "large.zip", []byte("mount-test-password"), UnlockOptions{}, func(p UnlockProgress) error { progress = p; return nil })
	if err != nil || summary.Failed != 1 || api.saves != 0 || progress.Reason != "validation_limit" {
		t.Fatalf("summary=%+v err=%v progress=%+v", summary, err, progress)
	}
}
