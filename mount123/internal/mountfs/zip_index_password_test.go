package mountfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/hanwen/go-fuse/v2/fs"
)

type countedZIPPasswordAPI struct {
	*archiveAPI
	reads atomic.Int64
}

func (a *countedZIPPasswordAPI) ReadSmallFile(ctx context.Context, id, limit int64) ([]byte, error) {
	a.reads.Add(1)
	return a.archiveAPI.ReadSmallFile(ctx, id, limit)
}

func TestZIPMetadataDoesNotDiscoverBodyPassword(t *testing.T) {
	for _, encryption := range []string{"plain", "aes256-deflate", "zipcrypto-deflate"} {
		for _, adapter := range []string{"service", "fuse"} {
			t.Run(encryption+"/"+adapter, func(t *testing.T) {
				data, want, password := zipDisplayCacheFixture(t, "payload.bin", nil, encryption)
				if encryption == "plain" {
					password = []byte("unrelated-password")
				}
				original, base := otherArchiveFixture(t, "archive.zip", data, password, false)
				api := &countedZIPPasswordAPI{archiveAPI: base}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				for pass := 0; pass < 2; pass++ {
					root := New(ctx, api, original.tree.cache, 0, true)
					fs.NewNodeFS(root, &fs.Options{})
					if err := root.Prepare(ctx); err != nil {
						t.Fatal(err)
					}
					api.reads.Store(0)
					if _, err := NewServiceFromNode(root).ListArchive(ctx, 77, ""); err != nil {
						t.Fatal(err)
					}
					if n := api.reads.Load(); n != 0 {
						t.Fatalf("metadata pass %d read an unrelated body password %d times", pass, n)
					}
					readZIPDisplayCacheMember(t, root, adapter, "payload.bin", want)
					wantReads := int64(0)
					if encryption != "plain" {
						wantReads = 1
					}
					if n := api.reads.Load(); n != wantReads {
						t.Fatalf("body password reads=%d want=%d", n, wantReads)
					}
					status, err := root.StatusArchiveIndex(ctx, "archive.zip")
					if err != nil || status.State != "complete" || api.reads.Load() != wantReads {
						t.Fatalf("cached status rediscovered password or lost index: state=%s err=%v", status.State, err)
					}
				}
			})
		}
	}
}

func TestZIPIndexStatusDoesNotDiscoverBodyPassword(t *testing.T) {
	data, _, password := zipDisplayCacheFixture(t, "payload.bin", nil, "aes256-deflate")
	original, base := otherArchiveFixture(t, "archive.zip", data, password, false)
	api := &countedZIPPasswordAPI{archiveAPI: base}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	root := New(ctx, api, original.tree.cache, 0, true)
	if err := root.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	for {
		status, err := root.StatusArchiveIndex(ctx, "archive.zip")
		if err != nil || status.State == "failed" {
			t.Fatalf("status: %s, %v", status.State, err)
		}
		if n := api.reads.Load(); n != 0 {
			t.Fatalf("ZIP index status read body password %d times", n)
		}
		if status.State == "complete" {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("index status did not complete")
		case <-time.After(time.Millisecond):
		}
	}
}

type recursiveZIPPasswordAPI struct {
	*passwordSaveAPI
	passwordReads atomic.Int64
}

func (a *recursiveZIPPasswordAPI) List(_ context.Context, id int64) ([]panapi.File, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var files []panapi.File
	for _, f := range a.files {
		if f.ParentID == id {
			files = append(files, f)
		}
	}
	return files, nil
}

func (a *recursiveZIPPasswordAPI) Detail(_ context.Context, id int64) (panapi.File, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, f := range a.files {
		if f.ID == id {
			return f, nil
		}
	}
	return panapi.File{}, syscall.ENOENT
}

func (a *recursiveZIPPasswordAPI) Infos(ctx context.Context, ids []int64) ([]panapi.File, error) {
	var files []panapi.File
	for _, id := range ids {
		f, err := a.Detail(ctx, id)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, nil
}

func (a *recursiveZIPPasswordAPI) ReadSmallFile(ctx context.Context, id, limit int64) ([]byte, error) {
	a.passwordReads.Add(1)
	return a.passwordSaveAPI.ReadSmallFile(ctx, id, limit)
}

func TestZIPRecursivePasswordRefreshKeepsIndexButAuthenticatesBody(t *testing.T) {
	for _, encryption := range []string{"aes256-deflate", "zipcrypto-deflate"} {
		t.Run(encryption, func(t *testing.T) {
			data, want, password := zipDisplayCacheFixture(t, "payload.bin", nil, encryption)
			files := []panapi.File{
				{ID: 7, Name: "ancestor", IsDir: true},
				{ID: 9, ParentID: 7, Name: "child", IsDir: true},
				{ID: 77, ParentID: 9, Name: "archive.zip", Size: int64(len(data)), Version: "v1"},
				{ID: 9999, ParentID: 7, Name: ".mount123.pwd", Size: int64(len(password)), Version: "p1"},
			}
			original, base := passwordSaveFixture(t, files, map[int64][]byte{77: data, 9999: password})
			api := &recursiveZIPPasswordAPI{passwordSaveAPI: base}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			root := New(ctx, api, original.tree.cache, 0, true)
			fs.NewNodeFS(root, &fs.Options{})
			if err := root.Prepare(ctx); err != nil {
				t.Fatal(err)
			}
			service := NewServiceFromNode(root)
			index, _, _, secret, err := service.outerArchiveIndex(ctx, 77)
			clear(secret)
			if err != nil || api.passwordReads.Load() != 0 {
				t.Fatalf("initial index discovered body password: %v", err)
			}
			readZIPDisplayCacheMember(t, root, "service", "payload.bin", want)
			for _, correct := range []bool{false, true} {
				api.mu.Lock()
				if correct {
					api.data[9999] = password
				} else {
					// Keep sidecar ID, version and length unchanged.
					api.data[9999] = bytes.Repeat([]byte("!"), len(password))
				}
				api.mu.Unlock()
				if _, err := root.RefreshDirectory(ctx, "ancestor"); err != nil {
					t.Fatal(err)
				}
				before := api.passwordReads.Load()
				reused, _, _, secret, err := service.outerArchiveIndex(ctx, 77)
				clear(secret)
				if err != nil || reused != index || api.passwordReads.Load() != before {
					t.Fatalf("password refresh invalidated or delayed immutable index: %v", err)
				}
				r, _, err := service.OpenArchiveMember(ctx, 77, "payload.bin")
				var got []byte
				if err == nil {
					got = make([]byte, len(want))
					_, err = r.ReadAt(got, 0)
					r.(io.Closer).Close()
				}
				if correct {
					if err != nil || !bytes.Equal(got, want) {
						t.Fatalf("corrected shared password could not read: %v", err)
					}
				} else if !errors.Is(err, syscall.EACCES) {
					t.Fatalf("wrong shared password must fail authentication: %v", err)
				}
				if api.passwordReads.Load() != before+1 {
					t.Fatal("body did not discover the refreshed ancestor password exactly once")
				}
			}
		})
	}
}
