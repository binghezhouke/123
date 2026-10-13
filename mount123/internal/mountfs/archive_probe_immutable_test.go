package mountfs

import (
	"context"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/storage"
)

func TestProbeMissingContentVersionPersistsOnlyDetectedArchives(t *testing.T) {
	dir := t.TempDir()
	cache, err := storage.NewCache(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	tree := New(context.Background(), indexStoreTestAPI{}, cache, 0, true).tree
	record := &archiveProbeRecord{Version: 1, Created: time.Unix(1, 0).UTC(), Results: []archiveProbeResult{
		{ID: 77, Size: 123, State: "detected", Kind: ".zip"},
		{ID: 78, Size: 123, Version: ":123", State: "detected", Kind: ".7z"},
		{ID: 79, Size: 123, Version: ":123", State: "unknown"},
		{ID: 80, Size: 123, State: "not_archive"},
		{ID: 81, Size: 123, Version: "v1", State: "not_archive"},
	}}
	if tree.saveProbeRecord(context.Background(), record) != "disk" {
		t.Fatal("probe record not persisted")
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	cache, err = storage.NewCache(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	tree = New(context.Background(), indexStoreTestAPI{}, cache, 0, true).tree
	got, err := tree.loadProbeRecord(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Results) != 3 || got.Results[0].ID != 77 || got.Results[1].ID != 78 || got.Results[2].ID != 81 {
		t.Fatalf("restored results=%#v; only detected archives or versioned negatives may persist", got.Results)
	}
}
