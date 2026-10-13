package mountfs

import (
	"context"
	"errors"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
)

type archiveMetadataAPI struct {
	serviceAPI
	listCalls atomic.Int32
	reads     atomic.Int32
	lastRead  atomic.Int64
}

func TestArchiveMetadataSnapshotFallbacks(t *testing.T) {
	for _, state := range []string{"fresh", "missing", "expired", "stale", "removed", "cancelled"} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			listed := panapi.File{ID: 70, ParentID: 7, Name: "listed.zip", Size: 12, Version: "listed-version"}
			detail := panapi.File{ID: 70, ParentID: 3, Name: "detail.zip", Size: 13, Version: "detail-version"}
			api := &serviceAPI{files: map[int64][]panapi.File{7: {listed}}, details: map[int64]panapi.File{70: detail}}
			service, err := NewService(ctx, api, nil, 0, true, Options{DirectoryTTL: time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			if state != "missing" {
				if _, err = service.ListDirectory(ctx, 7); err != nil {
					t.Fatal(err)
				}
			}
			switch state {
			case "expired", "stale":
				service.tree.mu.Lock()
				item := service.tree.meta["dir:7"]
				if state == "expired" {
					item.expires = time.Now().Add(-time.Second)
				} else {
					item.stale = true
				}
				service.tree.mu.Unlock()
			case "removed":
				api.files[7] = nil
				if _, err = service.tree.refreshCloudDirectory(ctx, 7, nil, ""); err != nil {
					t.Fatal(err)
				}
				delete(api.details, 70)
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			got, err := service.tree.archiveFileMetadata(ctx, 70)
			switch state {
			case "fresh":
				if err != nil || got != listed || api.detailCalls != 0 {
					t.Fatalf("metadata=%#v err=%v Detail=%d", got, err, api.detailCalls)
				}
			case "cancelled":
				if !errors.Is(err, context.Canceled) || api.detailCalls != 0 {
					t.Fatalf("err=%v Detail=%d", err, api.detailCalls)
				}
			case "removed":
				if !errors.Is(err, syscall.ENOENT) || api.detailCalls != 1 {
					t.Fatalf("err=%v Detail=%d", err, api.detailCalls)
				}
			default:
				if err != nil || got != detail || api.detailCalls != 1 {
					t.Fatalf("metadata=%#v err=%v Detail=%d", got, err, api.detailCalls)
				}
			}
		})
	}
}

func TestArchiveMetadataUsesLatestRestoredMove(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	old := panapi.File{ID: 70, ParentID: 0, Name: "before.zip", Size: 12, Version: "v1"}
	newest := old
	newest.ParentID, newest.Name = 3, "renamed"
	api := &archiveMetadataAPI{serviceAPI: serviceAPI{
		files:   map[int64][]panapi.File{0: {old}, 3: {newest}},
		details: map[int64]panapi.File{70: old},
	}}
	dir := t.TempDir()
	cache := openDirectoryCache(t, dir)
	opts := Options{DirectoryTTL: time.Hour}
	seed, err := NewService(ctx, api, cache, 0, true, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{0, 3} {
		if _, err = seed.ListDirectory(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if err = cache.Close(); err != nil {
		t.Fatal(err)
	}
	cache = openDirectoryCache(t, dir)
	defer cache.Close()
	service, err := NewService(ctx, api, cache, 0, true, opts)
	if err != nil {
		t.Fatal(err)
	}
	// Restore the newer destination before the old source directory.
	for _, id := range []int64{3, 0} {
		if _, err = service.ListDirectory(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	got, err := service.tree.archiveFileMetadata(ctx, 70)
	if err != nil || got != newest || api.detailCalls != 0 {
		t.Fatalf("metadata=%#v err=%v Detail=%d", got, err, api.detailCalls)
	}
	service.tree.mu.Lock()
	service.tree.meta["dir:3"].expires = time.Now().Add(-time.Second)
	service.tree.mu.Unlock()
	api.details[70] = newest
	got, err = service.tree.archiveFileMetadata(ctx, 70)
	if err != nil || got != newest || api.detailCalls != 1 {
		t.Fatalf("expired destination reused old location: metadata=%#v err=%v Detail=%d", got, err, api.detailCalls)
	}
}

func TestOrdinaryFileMetadataStillUsesDetail(t *testing.T) {
	ctx := context.Background()
	listed := panapi.File{ID: 70, Name: "archive.zip", Size: 12, Version: "v1"}
	detail := listed
	detail.Name = "detail.zip"
	api := &serviceAPI{files: map[int64][]panapi.File{0: {listed}}, details: map[int64]panapi.File{70: detail}}
	service, err := NewService(ctx, api, nil, 0, true, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.ListDirectory(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if got, err := service.File(ctx, 70); err != nil || *got != detail || api.detailCalls != 1 {
		t.Fatalf("File=%#v err=%v Detail=%d", got, err, api.detailCalls)
	}
	// DirectoryMetadata remains a directory-only shortcut: a non-directory
	// must retain the full Detail-backed behavior, even with a cached listing.
	if got, err := service.DirectoryMetadata(ctx, 70); err != nil || *got != detail || api.detailCalls != 1 {
		t.Fatalf("DirectoryMetadata=%#v err=%v Detail=%d", got, err, api.detailCalls)
	}
}

func (*archiveMetadataAPI) CacheIdentity() string { return "synthetic-archive-metadata-account" }
func (a *archiveMetadataAPI) List(ctx context.Context, id int64) ([]panapi.File, error) {
	a.listCalls.Add(1)
	return a.serviceAPI.List(ctx, id)
}
func (a *archiveMetadataAPI) ReadSmallFile(_ context.Context, id, _ int64) ([]byte, error) {
	a.reads.Add(1)
	a.lastRead.Store(id)
	return []byte("secret\n"), nil
}

func TestSharedPasswordAncestorMetadataFallback(t *testing.T) {
	for _, state := range []string{"missing", "expired", "removed"} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			api := &archiveMetadataAPI{serviceAPI: serviceAPI{
				files: map[int64][]panapi.File{
					0: {{ID: 7, ParentID: 0, Name: "old-parent", IsDir: true}, {ID: 1, Name: ".mount123.pwd", Size: 7}},
					3: {{ID: 31, ParentID: 3, Name: ".mount123.pwd", Size: 7}},
				},
				details: map[int64]panapi.File{7: {ID: 7, ParentID: 3, Name: "moved-parent", IsDir: true}},
			}}
			service, err := NewService(ctx, api, nil, 0, true, Options{DirectoryTTL: time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			if state != "missing" {
				if _, err = service.ListDirectory(ctx, 0); err != nil {
					t.Fatal(err)
				}
			}
			if state == "missing" {
				service.tree.mu.Lock()
				if item := service.tree.meta["dir:0"]; item != nil {
					delete(service.tree.meta, "dir:0")
					service.tree.metaBytes -= item.bytes
				}
				service.tree.mu.Unlock()
			} else if state == "expired" {
				service.tree.mu.Lock()
				service.tree.meta["dir:0"].expires = time.Now().Add(-time.Second)
				service.tree.mu.Unlock()
			} else if state == "removed" {
				api.files[0] = api.files[0][1:]
				if _, err = service.tree.refreshCloudDirectory(ctx, 0, nil, ""); err != nil {
					t.Fatal(err)
				}
			}
			password, err := service.SharedPassword(ctx, 7)
			if err != nil || password != "secret" || api.detailCalls != 1 || api.lastRead.Load() != 31 {
				t.Fatalf("SharedPassword=%q err=%v Detail=%d password-file=%d", password, err, api.detailCalls, api.lastRead.Load())
			}
		})
	}
}

func TestSharedPasswordReusesRestoredAncestorMetadata(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api := &archiveMetadataAPI{serviceAPI: serviceAPI{
		files: map[int64][]panapi.File{
			0: {{ID: 3, ParentID: 0, Name: "ancestor", IsDir: true}, {ID: 1, ParentID: 0, Name: ".mount123.pwd", Size: 7, Version: "p1"}},
			3: {{ID: 7, ParentID: 3, Name: "nested", IsDir: true}},
			7: {{ID: 70, ParentID: 7, Name: "archive.zip", Size: 10, Version: "v1"}},
		},
		details: map[int64]panapi.File{
			3: {ID: 3, ParentID: 0, Name: "ancestor", IsDir: true},
			7: {ID: 7, ParentID: 3, Name: "nested", IsDir: true},
		},
	}}
	dir := t.TempDir()
	cache := openDirectoryCache(t, dir)
	opts := Options{DirectoryTTL: time.Hour}
	seed, err := NewService(ctx, api, cache, 0, true, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{0, 3, 7} {
		if _, err = seed.ListDirectory(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if err = cache.Close(); err != nil {
		t.Fatal(err)
	}
	cache = openDirectoryCache(t, dir)
	defer cache.Close()
	service, err := NewService(ctx, api, cache, 0, true, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{0, 3, 7} {
		if _, err = service.ListDirectory(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	before := api.listCalls.Load()
	for i := 0; i < 2; i++ {
		password, err := service.SharedPassword(ctx, 7)
		if err != nil || password != "secret" {
			t.Fatalf("SharedPassword = %q, err=%v", password, err)
		}
	}
	if api.detailCalls != 0 || api.listCalls.Load() != before {
		t.Errorf("ancestor discovery made Detail=%d, List=%d calls", api.detailCalls, api.listCalls.Load()-before)
	}
	if api.reads.Load() != 2 {
		t.Errorf("password sidecar reads=%d, want 2", api.reads.Load())
	}
}

func TestArchiveMetadataIgnoresRetainedProbeOverlayAfterRemoval(t *testing.T) {
	ctx := context.Background()
	file := panapi.File{ID: 70, ParentID: 0, Name: "renamed", Size: 12, Version: "v1"}
	ancestor := panapi.File{ID: 7, ParentID: 0, Name: "ancestor", IsDir: true}
	api := &serviceAPI{files: map[int64][]panapi.File{0: {file, ancestor}}, details: map[int64]panapi.File{}}
	service, err := NewService(ctx, api, nil, 0, true, Options{DirectoryTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	record := &archiveProbeRecord{Version: 1, ParentID: 0, Created: time.Now(), Results: []archiveProbeResult{
		{ID: file.ID, Version: file.Version, Size: file.Size, Kind: ".zip", State: "detected"},
	}}
	if state := service.tree.saveProbeRecord(ctx, record); state != "memory" {
		t.Fatalf("probe record state=%s", state)
	}
	overlay, err := service.tree.cloudDirectory(ctx, 0)
	if err != nil || overlay.entries["renamed.zip"] == nil {
		t.Fatalf("probe overlay not built: err=%v", err)
	}
	api.files[0] = nil
	if _, err = service.tree.refreshCloudDirectory(ctx, 0, nil, ""); err != nil {
		t.Fatal(err)
	}
	// Existing directory handles may still hold this overlay. Current
	// metadata resolution must use the refreshed canonical directory.
	service.tree.mu.Lock()
	retained := false
	for _, item := range service.tree.meta {
		if item.value == overlay {
			retained = true
		}
	}
	service.tree.mu.Unlock()
	if !retained {
		t.Fatal("test requires the previous probe overlay to remain cached")
	}
	if got, err := service.ArchiveMetadata(ctx, file.ID); !errors.Is(err, syscall.ENOENT) {
		t.Errorf("removed archive metadata=%#v err=%v, want Detail ENOENT", got, err)
	}
	if got, err := service.DirectoryMetadata(ctx, ancestor.ID); !errors.Is(err, syscall.ENOENT) {
		t.Errorf("removed ancestor metadata=%#v err=%v, want Detail ENOENT", got, err)
	}
	if api.detailCalls != 2 {
		t.Errorf("Detail calls=%d, want 2", api.detailCalls)
	}
}
