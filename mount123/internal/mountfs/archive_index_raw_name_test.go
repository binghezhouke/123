package mountfs

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/binghezhouke/123/mount123/internal/storage"
)

func TestZIPLossyIndexMigrationRetainsContent(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(map[bool]string{false: "root", true: "nested"}[nested], func(t *testing.T) {
			ctx := context.Background()
			cache, err := storage.NewCache(t.TempDir(), 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer cache.Close()
			tree := New(ctx, indexStoreTestAPI{}, cache, 0, true).tree
			const raw = "\xd6\xd0\xce\xc4.bin"
			idx := &zipIndex{complete: true, root: &zipDir{dirs: map[string]*zipDir{}, files: map[string]*member{"中文.bin": {name: raw, displayPath: "中文.bin", size: 4, compressed: 4}}}}
			key := "synthetic-index"
			if nested {
				key = "archive-index-nested-dto-v1:" + key
			}
			tree.persistArchiveIndex(ctx, key, ".zip", 100, "synthetic-identity", idx)
			h, err := cache.OpenArchiveIndex(key)
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(io.NewSectionReader(h, 0, h.Size()))
			h.Close()
			if err != nil {
				t.Fatal(err)
			}
			var dto archiveIndexDTO
			if err := json.Unmarshal(data, &dto); err != nil {
				t.Fatal(err)
			}
			if string(dto.Entries[0].RawName) != raw {
				t.Fatal("raw name was not preserved")
			}
			// Old encoding/json replaced the raw byte string with U+FFFD sequences.
			dto.Entries[0].Name = raw
			dto.Entries[0].RawName = nil
			legacy, err := json.Marshal(dto)
			if err != nil {
				t.Fatal(err)
			}
			if err := cache.ReplaceArchiveIndex(ctx, key, legacy); err != nil {
				t.Fatal(err)
			}
			if err := cache.Store(ctx, "verified-content", []byte("body")); err != nil {
				t.Fatal(err)
			}
			if _, ok := tree.loadPersistentArchiveIndex(ctx, key, ".zip", 100, "synthetic-identity"); ok {
				t.Fatal("lossy legacy index restored")
			}
			body, err := cache.OpenCached("verified-content", 4)
			if err != nil || body == nil {
				t.Fatal("metadata migration evicted content")
			}
			body.Close()
			tree.persistArchiveIndex(ctx, key, ".zip", 100, "synthetic-identity", idx)
			restored, ok := tree.loadPersistentArchiveIndex(ctx, key, ".zip", 100, "synthetic-identity")
			if !ok || restored.members[raw] == nil || restored.members[raw].displayPath != "中文.bin" {
				t.Fatal("rebuilt raw/display identity not restored")
			}
		})
	}
}
