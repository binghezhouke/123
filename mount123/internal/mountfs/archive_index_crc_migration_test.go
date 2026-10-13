package mountfs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"os"
	"sync/atomic"
	"testing"

	"github.com/binghezhouke/123/mount123/internal/storage"
)

// Old sevenzip versions could persist the wrong CRC for a legal mixed-group
// archive. Immutable input bytes do not make that derived metadata trustworthy.
func TestArchiveIndexCRCMetadataMigration(t *testing.T) {
	for _, kind := range []string{".7z", ".zip", ".rar"} {
		for _, nested := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/nested=%t", kind, nested), func(t *testing.T) {
				ctx := context.Background()
				cache, err := storage.NewCache(t.TempDir(), 1<<20)
				if err != nil {
					t.Fatal(err)
				}
				defer cache.Close()
				tree := New(ctx, indexStoreTestAPI{}, cache, 0, true).tree
				key := tree.archiveIndexCacheKey(kind, "synthetic-source", &archiveDescriptor{id: 77}, nil)
				if nested {
					key = "archive-index-nested-dto-v1:" + key
				}
				// A directory-only DTO is valid for all three formats and lets
				// this compatibility check remain independent of parser changes.
				legacy := archiveIndexDTO{Version: archiveIndexFormatVersion, Kind: kind, ArchiveSize: 100, IdentityDigest: "identity", Complete: true,
					Entries: []archiveIndexEntryDTO{{Path: "folder", Directory: true}}}
				idx, err := tree.indexFromDTO(ctx, kind, legacy, 100)
				if err != nil {
					t.Fatal(err)
				}
				data, err := json.Marshal(legacy)
				if err != nil {
					t.Fatal(err)
				}
				if err := cache.StoreArchiveIndex(ctx, key, data); err != nil {
					t.Fatal(err)
				}
				if err := cache.Store(ctx, "synthetic-payload-block", []byte("retained bytes")); err != nil {
					t.Fatal(err)
				}
				_, ok := tree.loadPersistentArchiveIndex(ctx, key, kind, 100, "identity")
				if ok != (kind != ".7z") {
					t.Fatalf("legacy %s restored=%t; only old 7z metadata must be rejected", kind, ok)
				}
				if kind == ".7z" {
					if h, err := cache.OpenArchiveIndex(key); err == nil {
						h.Close()
						t.Fatal("obsolete metadata still occupies its cache key")
					}
				}
				// Publishing a rebuilt index must make the next independent
				// Tree restore it, with the unrelated body block left intact.
				tree.persistArchiveIndex(ctx, key, kind, 100, "identity", idx)
				tree = New(ctx, indexStoreTestAPI{}, cache, 0, true).tree
				if got, ok := tree.loadPersistentArchiveIndex(ctx, key, kind, 100, "identity"); !ok || got.root.dirs["folder"] == nil {
					t.Fatal("rebuilt index could not be restored")
				}
				h, err := cache.Open("synthetic-payload-block")
				if err != nil {
					t.Fatal("metadata migration evicted payload:", err)
				}
				defer h.Close()
				got := make([]byte, h.Size())
				if _, err := h.ReadAt(got, 0); err != nil || string(got) != "retained bytes" {
					t.Fatalf("payload changed: %q %v", got, err)
				}
			})
		}
	}
}

func TestSevenZIPRebuildsLegacyIncorrectMemberCRCs(t *testing.T) {
	data, err := os.ReadFile("testdata/solid7z/mixed-crc.7z")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cache, err := storage.NewCache(t.TempDir(), 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	tree := New(ctx, indexStoreTestAPI{}, cache, 0, true).tree
	var deny atomic.Bool
	var requests atomic.Int64
	source, server := indexTestRemote(t, ctx, tree, cache, "archive.7z", bytes.NewReader(data), int64(len(data)), &deny, &requests)
	defer server.Close()
	a := &archiveDescriptor{id: 77, name: "archive.7z", version: "v1", size: int64(len(data))}
	if _, err := tree.otherIndex(ctx, source, a, nil); err != nil {
		t.Fatal(err)
	}
	key := tree.archiveIndexCacheKey(".7z", archiveIdentity(source, a), a, nil)
	h, err := cache.OpenArchiveIndex(key)
	if err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, h.Size())
	_, err = h.ReadAt(raw, 0)
	h.Close()
	if err != nil {
		t.Fatal(err)
	}
	var legacy archiveIndexDTO
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatal(err)
	}
	legacy.SevenCRCVersion = 0
	for i := range legacy.Entries {
		legacy.Entries[i].CRC = 0 // Simulate the obsolete parser's wrong mapping.
	}
	raw, err = json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.ReplaceArchiveIndex(ctx, key, raw); err != nil {
		t.Fatal(err)
	}
	// A fresh Tree must rebuild bad metadata from the immutable source, then
	// the persisted result must be usable by another Tree without rebuilding.
	tree = New(ctx, indexStoreTestAPI{}, cache, 0, true).tree
	idx, err := tree.otherIndex(ctx, source, a, nil)
	if err != nil || len(idx.members) != 9 {
		t.Fatalf("rebuild: index=%v err=%v", idx, err)
	}
	for i := 0; i < 9; i++ {
		name := fmt.Sprintf("member-%02d.bin", i)
		if i == 0 {
			name = "standalone.bin"
		}
		m := idx.members[name]
		if m == nil || m.crc != crc32.ChecksumIEEE([]byte{byte('A' + i)}) {
			t.Fatalf("member %d retained an incorrect CRC", i)
		}
	}
	tree = New(ctx, indexStoreTestAPI{}, cache, 0, true).tree
	if restored, ok := tree.loadPersistentArchiveIndex(ctx, key, ".7z", a.size, legacy.IdentityDigest); !ok || len(restored.members) != 9 {
		t.Fatal("rebuilt metadata required another migration")
	}
}
