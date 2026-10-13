package mountfs

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
)

type rootSolid7zSplitFixture struct {
	tree   *Tree
	source *storage.Remote
	a      *archiveDescriptor
	idx    *zipIndex
	view   *nestedArchive
}

// Parse locally to model a completed index restored without opening every
// volume source. Only the first physical source is initialized in this Tree.
func newRootSolid7zSplitFixture(t *testing.T) *rootSolid7zSplitFixture {
	t.Helper()
	data, err := os.ReadFile("testdata/solid7z/inner.7z")
	if err != nil {
		t.Fatal(err)
	}
	const cut = 64 // Signature is in volume one; the next header is in two.
	files := []panapi.File{{ID: 77, Name: "archive.7z.001", Size: cut, Version: "immutable"}, {ID: 78, Name: "archive.7z.002", Size: int64(len(data) - cut), Version: "immutable"}}
	base, api := passwordSaveFixture(t, files, map[int64][]byte{77: data[:cut], 78: data[cut:]})
	ctx, cancel := context.WithCancel(context.Background())
	service, err := NewService(ctx, api, base.tree.cache, 0, true, Options{MaxConcurrentBuilds: 1, DisableReadAhead: true, DisableStreamMembers: true})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		service.tree.nestedSolid7zPool.Close()
		service.tree.nestedDeflatePool.Close()
	})
	tree := service.tree
	source, err := tree.cloudArchiveSource(ctx, &files[0])
	if err != nil {
		t.Fatal(err)
	}
	members, err := indexNested7z(ctx, bytes.NewReader(data), int64(len(data)), nil, defaultNestedArchivePolicy)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := nestedIndexFromMembers(members, defaultNestedArchivePolicy)
	if err != nil {
		t.Fatal(err)
	}
	a := &archiveDescriptor{id: 77, name: files[0].Name, size: cut, version: files[0].Version}
	view, err := tree.rootArchiveView(ctx, source, a, idx, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { view.Close() })
	volumes, ok := view.input.(*volumeReaderAt)
	if !ok || len(volumes.sources) != 2 || volumes.sources[1] != nil {
		t.Fatal("fixture did not preserve an uninitialized second-volume source")
	}
	return &rootSolid7zSplitFixture{tree: tree, source: source, a: a, idx: idx, view: view}
}

func (f *rootSolid7zSplitFixture) run(t *testing.T, stage string, operation func(context.Context) error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- operation(ctx) }()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	var ticks <-chan time.Time = ticker.C
	for {
		select {
		case <-ctx.Done():
			// Let a canceled foreground operation release its view references
			// before fixture cleanup; the worker result channel is buffered.
			select {
			case <-done:
			case <-time.After(250 * time.Millisecond):
				t.Error("operation did not return promptly after cancellation")
			}
			t.Fatalf("%s did not complete before its bounded deadline: %v", stage, ctx.Err())
		case err := <-done:
			if err != nil {
				t.Fatalf("%s must complete with one build slot and a cold second-volume source: %v", stage, err)
			}
			return
		case <-ticks:
			stats := f.tree.buildGate.Stats()
			if stats.Active == 1 && stats.Waiting > 0 {
				f.tree.mu.Lock()
				var initializingVolume bool
				for key := range f.tree.sources {
					initializingVolume = initializingVolume || strings.HasPrefix(key, "archive-source:")
				}
				f.tree.mu.Unlock()
				t.Logf("%s: gate limit=%d active=%d waiting=%d, volume source flight=%t", stage, stats.Limit, stats.Active, stats.Waiting, initializingVolume)
				ticks = nil
			}
		}
	}
}

func TestOther7zSplitColdSourceSingleSlot(t *testing.T) {
	for _, persisted := range []bool{false, true} {
		stage := "native-header-scan"
		if persisted {
			stage = "persisted-index"
		}
		t.Run(stage, func(t *testing.T) {
			f := newRootSolid7zSplitFixture(t)
			if persisted {
				_, size, identity, err := f.tree.archiveSource(context.Background(), f.source, f.a)
				if err != nil {
					t.Fatal(err)
				}
				key := f.tree.archiveIndexCacheKey(".7z", identity, f.a, nil)
				digest := f.tree.cache.StableDigest("archive-index-identity-v1", identity)
				f.tree.persistArchiveIndex(context.Background(), key, ".7z", size, digest, f.idx)
				if _, ok := f.tree.loadPersistentArchiveIndex(context.Background(), key, ".7z", size, digest); !ok {
					t.Fatal("completed index DTO was not persisted")
				}
			}
			f.run(t, stage, func(ctx context.Context) error {
				idx, err := f.tree.otherIndex(ctx, f.source, f.a, nil)
				if err == nil && (!idx.complete || len(idx.members) != 3) {
					return fmt.Errorf("index is incomplete or has %d members", len(idx.members))
				}
				return err
			})
		})
	}
}
