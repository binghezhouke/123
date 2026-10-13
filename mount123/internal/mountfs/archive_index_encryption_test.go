package mountfs

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/storage"
)

// RAR body encryption is independent of the ZIP flags field. Restored clear
// headers must still trigger password discovery when a member is opened.
func TestRARIndexPersistenceKeepsBodyEncryption(t *testing.T) {
	ctx := context.Background()
	data, err := os.ReadFile("../../../tests/fixtures/rar/rar5-psw.rar")
	if err != nil {
		t.Fatal(err)
	}
	members, err := indexNestedRAR(ctx, bytes.NewReader(data), int64(len(data)), nil, defaultNestedArchivePolicy)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := nestedIndexFromMembers(members, defaultNestedArchivePolicy)
	if err != nil {
		t.Fatal(err)
	}
	protected := 0
	for _, m := range idx.members {
		if m.encrypted {
			protected++
		}
	}
	if protected == 0 {
		t.Fatal("fixture did not expose encrypted body metadata without a password")
	}
	cache, err := storage.NewCache(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	tree := New(ctx, indexStoreTestAPI{}, cache, 0, true).tree
	a := &archiveDescriptor{id: 77, name: "body-encrypted.rar", size: int64(len(data))}
	key := tree.archiveIndexCacheKey(".rar", "synthetic-body-encrypted", a, nil)
	identity := cache.StableDigest("test", "body-encrypted")
	tree.persistArchiveIndex(ctx, key, ".rar", a.size, identity, idx)
	restored, ok := tree.loadPersistentArchiveIndex(ctx, key, ".rar", a.size, identity)
	if !ok {
		t.Fatal("newly persisted index did not restore")
	}
	for name, original := range idx.members {
		got := restored.members[name]
		if got == nil || got.encrypted != original.encrypted {
			t.Errorf("restored member lost body encryption state")
		}
	}

	// A pre-fix DTO has no explicit encryption state. It must be rebuilt,
	// rather than interpreting a missing field as an unencrypted member.
	h, err := cache.OpenArchiveIndex(key)
	if err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, h.Size())
	_, err = h.ReadAt(raw, 0)
	_ = h.Close()
	if err != nil {
		t.Fatal(err)
	}
	var old map[string]json.RawMessage
	if err = json.Unmarshal(raw, &old); err != nil {
		t.Fatal(err)
	}
	var entries []map[string]json.RawMessage
	if err = json.Unmarshal(old["entries"], &entries); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		delete(e, "encrypted")
	}
	old["entries"], err = json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if err = cache.ReplaceArchiveIndex(ctx, key, raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := tree.loadPersistentArchiveIndex(ctx, key, ".rar", a.size, identity); ok {
		t.Error("legacy RAR index with unknown encryption state was accepted")
	}
}

func TestArchiveIndexCancelledRestoreRetainsValidSnapshot(t *testing.T) {
	ctx := context.Background()
	data, err := os.ReadFile("../../../tests/fixtures/rar/rar5-crc.rar")
	if err != nil {
		t.Fatal(err)
	}
	members, err := indexNestedRAR(ctx, bytes.NewReader(data), int64(len(data)), nil, defaultNestedArchivePolicy)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := nestedIndexFromMembers(members, defaultNestedArchivePolicy)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := storage.NewCache(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	tree := New(ctx, indexStoreTestAPI{}, cache, 0, true).tree
	a := &archiveDescriptor{id: 77, name: "synthetic.rar", size: int64(len(data))}
	key := tree.archiveIndexCacheKey(".rar", "cancelled-restore", a, nil)
	identity := cache.StableDigest("test", "cancelled-restore")
	for _, deadline := range []bool{false, true} {
		tree.persistArchiveIndex(ctx, key, ".rar", a.size, identity, idx)
		request, cancel := context.WithCancel(ctx)
		if deadline {
			cancel()
			request, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
		}
		cancel()
		if _, ok := tree.loadPersistentArchiveIndex(request, key, ".rar", a.size, identity); ok {
			t.Fatal("cancelled request restored a snapshot")
		}
		if _, ok := tree.loadPersistentArchiveIndex(ctx, key, ".rar", a.size, identity); !ok {
			t.Error("cancelled restore removed a valid immutable snapshot")
		}
	}
}
