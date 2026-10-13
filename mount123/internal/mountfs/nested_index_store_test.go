package mountfs

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/binghezhouke/123/mount123/internal/storage"
)

func TestNestedIndexPersistenceRequiresCompletedSnapshot(t *testing.T) {
	ctx := context.Background()
	cache, err := storage.NewCache(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	tree := New(ctx, indexStoreTestAPI{}, cache, 0, true).tree
	dto := archiveIndexDTO{Version: archiveIndexFormatVersion, Kind: ".zip", ArchiveSize: 100, IdentityDigest: "synthetic-identity", Entries: []archiveIndexEntryDTO{{Path: "entry.txt", Name: "entry.txt", Size: 1, Compressed: 1, HeaderOffset: 20}}}
	for _, prefix := range []string{"archive-index-nested-dto-v1:", "archive-index-dto-v1:"} {
		key := prefix + "synthetic-partial"
		raw, err := json.Marshal(dto)
		if err != nil {
			t.Fatal(err)
		}
		if err = cache.StoreArchiveIndex(ctx, key, raw); err != nil {
			t.Fatal(err)
		}
		_, ok := tree.loadPersistentArchiveIndex(ctx, key, ".zip", dto.ArchiveSize, dto.IdentityDigest)
		if prefix == "archive-index-nested-dto-v1:" && ok {
			t.Fatal("partial nested snapshot was restored as complete")
		}
		if prefix == "archive-index-dto-v1:" && !ok {
			t.Fatal("legacy outer completion-marker compatibility was lost")
		}
	}
	dto.Complete = true
	raw, err := json.Marshal(dto)
	if err != nil {
		t.Fatal(err)
	}
	key := "archive-index-nested-dto-v1:synthetic-complete"
	if err = cache.StoreArchiveIndex(ctx, key, raw); err != nil {
		t.Fatal(err)
	}
	first, ok := tree.loadPersistentArchiveIndex(ctx, key, ".zip", dto.ArchiveSize, dto.IdentityDigest)
	if !ok {
		t.Fatal("completed nested snapshot did not restore")
	}
	second, ok := tree.loadPersistentArchiveIndex(ctx, key, ".zip", dto.ArchiveSize, dto.IdentityDigest)
	if !ok {
		t.Fatal("second request did not restore completed snapshot")
	}
	if first == second || first.root == second.root || first.members["entry.txt"] == second.members["entry.txt"] {
		t.Fatal("requests share mutable index or member state")
	}
	if first.members["entry.txt"].reader != nil || second.members["entry.txt"].reader != nil {
		t.Fatal("persisted snapshot retained a request reader")
	}
	first.members["entry.txt"].name = "request-local-change.txt"
	if second.members["entry.txt"].name != "entry.txt" {
		t.Fatal("request mutation changed another restored index")
	}
}
