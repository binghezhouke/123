package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/iostats"
	"github.com/binghezhouke/123/mount123/internal/workqueue"
)

// indexKindByName reports the family by name so failures read in domain terms.
func indexKindByName(t *testing.T, summary iostats.CacheSummary, name string) iostats.CacheIndexKindSummary {
	t.Helper()
	for _, kind := range summary.IndexKinds {
		if kind.Kind == name {
			return kind
		}
	}
	t.Fatalf("index kind %q missing from %+v", name, summary.IndexKinds)
	return iostats.CacheIndexKindSummary{}
}

func TestDefaultIndexBudgetScalesWithTheCacheSize(t *testing.T) {
	const cacheBytes = int64(50) << 30 // the documented default data cache
	if got, want := defaultIndexBudget(cacheBytes), cacheBytes/8; got != want {
		t.Fatalf("default index budget = %d, want %d", got, want)
	}
	// The previous policy capped the share at 64 MiB, which made large ZIP,
	// RAR and 7z indexes lose protection on a large cache.
	if got := defaultIndexBudget(cacheBytes); got <= 64<<20 {
		t.Fatalf("default index budget %d is still pinned near the old 64 MiB ceiling", got)
	}
	if _, err := NewCacheWithIndexBudget(t.TempDir(), 1<<20, 1<<20+1, DefaultDownloadConfig()); err == nil {
		t.Fatal("an index budget larger than the cache was accepted")
	}
}

func TestCacheStatsSeparateIndexKindsFromOrdinaryData(t *testing.T) {
	cache, err := NewCache(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	ctx := context.Background()

	objects := []struct {
		key  string
		size int
	}{
		{"archive-index-dto-v1:opaque", 300},
		{"directory-snapshot:opaque", 200},
		{"remote-identity-v1:opaque", 100},
		{"archive-probes:opaque", 50},
	}
	for _, item := range objects {
		if err := cache.ReplaceArchiveIndex(ctx, item.key, bytes.Repeat([]byte("m"), item.size)); err != nil {
			t.Fatalf("store %s: %v", item.key, err)
		}
	}
	if err := cache.Store(ctx, "ordinary", bytes.Repeat([]byte("d"), 64)); err != nil {
		t.Fatal(err)
	}

	summary := cache.Stats()
	for _, item := range []struct {
		name string
		size int64
	}{
		{"archive_index", 300},
		{"directory_snapshot", 200},
		{"remote_identity", 100},
		{"archive_probe", 50},
	} {
		kind := indexKindByName(t, summary, item.name)
		if kind.Entries != 1 || kind.Bytes != item.size {
			t.Errorf("%s residency = %+v, want one entry of %d bytes", item.name, kind, item.size)
		}
		if kind.CapacityEvictions != 0 || kind.Demotions != 0 {
			t.Errorf("%s attributed eviction/demotion = %+v", item.name, kind)
		}
	}
	if summary.IndexUsedBytes != 650 {
		t.Fatalf("protected index bytes = %d, want 650", summary.IndexUsedBytes)
	}
	// Ordinary data must stay out of every protected family and be visible in
	// the ordinary (probationary) class instead.
	var protectedEntries uint64
	for _, kind := range summary.IndexKinds {
		protectedEntries += kind.Entries
	}
	if protectedEntries != 4 {
		t.Fatalf("protected entries = %d, want 4", protectedEntries)
	}
	if summary.Classes[1].Entries != 1 || summary.Classes[1].Bytes != 64 {
		t.Fatalf("ordinary class residency = %+v", summary.Classes[1])
	}
}

func TestCacheStatsAttributeOverBudgetDemotionToItsKind(t *testing.T) {
	cache, err := NewCache(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	ctx := context.Background()
	archiveSize := int(cache.indexBudget) - 100

	// Take most of the share with an archive index, then grow a directory
	// snapshot past what remains.
	if err := cache.ReplaceArchiveIndex(ctx, "archive-index-dto-v1:x", bytes.Repeat([]byte("i"), archiveSize)); err != nil {
		t.Fatal(err)
	}
	if err := cache.ReplaceArchiveIndex(ctx, "directory-snapshot:x", bytes.Repeat([]byte("m"), 50)); err != nil {
		t.Fatal(err)
	}
	if got := indexKindByName(t, cache.Stats(), "directory_snapshot"); got.Entries != 1 || got.Bytes != 50 {
		t.Fatalf("directory snapshot residency = %+v", got)
	}
	if err := cache.ReplaceArchiveIndex(ctx, "directory-snapshot:x", bytes.Repeat([]byte("m"), int(cache.indexBudget))); err != nil {
		t.Fatal(err)
	}

	summary := cache.Stats()
	if got := indexKindByName(t, summary, "directory_snapshot"); got.Entries != 0 || got.Bytes != 0 {
		t.Fatalf("grown directory snapshot stayed protected: %+v", got)
	}
	if got := indexKindByName(t, summary, "directory_snapshot"); got.Demotions != 1 || got.DemotionBytes != int64(cache.indexBudget) {
		t.Fatalf("demotion attribution = %+v, want one demotion of the grown size", got)
	}
	if got := indexKindByName(t, summary, "archive_index"); got.Demotions != 0 || got.Bytes != int64(archiveSize) {
		t.Fatalf("unrelated kind absorbed the demotion: %+v", got)
	}
	// The demoted object is still readable, just no longer protected.
	h, err := cache.OpenArchiveIndex("directory-snapshot:x")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if h.Size() != cache.indexBudget {
		t.Fatalf("demoted object size = %d", h.Size())
	}
}

func TestCacheRestartRestoresIndexKindsAndTrimsToBudget(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	archiveBytes := bytes.Repeat([]byte("a"), 700)

	cache, err := NewCacheWithIndexBudget(dir, 1<<20, 2048, DefaultDownloadConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.ReplaceArchiveIndex(ctx, "archive-index-dto-v1:restart", archiveBytes); err != nil {
		t.Fatal(err)
	}
	if err := cache.ReplaceArchiveIndex(ctx, "directory-snapshot:restart", bytes.Repeat([]byte("d"), 700)); err != nil {
		t.Fatal(err)
	}
	if got := cache.Stats().IndexUsedBytes; got != 1400 {
		t.Fatalf("protected bytes before restart = %d, want 1400", got)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopening with a smaller share must trim the oldest protected object back
	// to ordinary data instead of failing or ignoring the configured budget.
	cache, err = NewCacheWithIndexBudget(dir, 1<<20, 800, DefaultDownloadConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	summary := cache.Stats()
	if summary.IndexUsedBytes != 700 {
		t.Fatalf("restored protected bytes = %d, want 700", summary.IndexUsedBytes)
	}
	if got := indexKindByName(t, summary, "directory_snapshot"); got.Entries != 1 || got.Bytes != 700 {
		t.Fatalf("restored directory snapshot = %+v", got)
	}
	if got := indexKindByName(t, summary, "archive_index"); got.Entries != 0 || got.Demotions != 1 || got.DemotionBytes != 700 {
		t.Fatalf("trimmed archive index = %+v, want one recorded demotion of 700 bytes", got)
	}
	// A trimmed index is still served from disk; only its retention changed.
	h, err := cache.OpenArchiveIndex("archive-index-dto-v1:restart")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	got := make([]byte, h.Size())
	if _, err := h.ReadAt(got, 0); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if !bytes.Equal(got, archiveBytes) {
		t.Fatal("trimmed archive index lost its persisted bytes")
	}
}

func TestCacheSpeculativePrefetchCannotEvictProtectedIndex(t *testing.T) {
	const maxBytes = 8 << 10
	cache, err := NewCache(t.TempDir(), maxBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	ctx := context.Background()
	background := workqueue.Background(ctx)

	keys := []string{"archive-index-dto-v1:one", "directory-snapshot:two", "remote-identity-v1:three"}
	indexSize := cache.indexBudget / int64(len(keys))
	for _, key := range keys {
		if err := cache.StoreArchiveIndex(ctx, key, bytes.Repeat([]byte("i"), int(indexSize))); err != nil {
			t.Fatalf("store %s: %v", key, err)
		}
	}
	if got := cache.Stats().IndexUsedBytes; got != indexSize*int64(len(keys)) {
		t.Fatalf("protected bytes = %d, want %d", got, indexSize*int64(len(keys)))
	}

	// Image prefetch fills through a background context, and ordinary reads
	// through a foreground one. Both have to fight for what is left, and
	// neither may displace a protected index object.
	filler := bytes.Repeat([]byte("f"), int(indexSize))
	var speculative, ordinary *Handle
	for i := 0; i < 48; i++ {
		name := "fill-" + string(rune('a'+i%26)) + "-" + string(rune('0'+i/26))
		speculative, err = cache.Acquire(background, "prefetch-"+name, indexSize, fillBytes(filler))
		if err != nil {
			t.Fatalf("speculative fill %d: %v", i, err)
		}
		ordinary, err = cache.Acquire(ctx, "ordinary-"+name, indexSize, fillBytes(filler))
		if err != nil {
			t.Fatalf("ordinary fill %d: %v", i, err)
		}
		_ = speculative.Close()
		_ = ordinary.Close()
	}

	summary := cache.Stats()
	if summary.IndexUsedBytes != indexSize*int64(len(keys)) {
		t.Fatalf("data pressure displaced protected bytes: %d, want %d", summary.IndexUsedBytes, indexSize*int64(len(keys)))
	}
	for _, name := range []string{"archive_index", "directory_snapshot", "remote_identity"} {
		kind := indexKindByName(t, summary, name)
		if kind.Entries != 1 || kind.Bytes != indexSize {
			t.Fatalf("%s after data pressure = %+v", name, kind)
		}
		if kind.CapacityEvictions != 0 || kind.Demotions != 0 {
			t.Fatalf("%s was downgraded under data pressure: %+v", name, kind)
		}
	}
	if summary.UsedBytes > maxBytes || summary.ReservedBytes != 0 {
		t.Fatalf("capacity invariant: used=%d reserved=%d max=%d", summary.UsedBytes, summary.ReservedBytes, maxBytes)
	}
	if summary.Classes[cacheSpeculative].CapacityEvictions == 0 {
		t.Fatal("data pressure never reclaimed speculative prefetch data")
	}
	// Every protected object is still readable after the pressure.
	for _, key := range keys {
		h, err := cache.OpenArchiveIndex(key)
		if err != nil {
			t.Fatalf("open %s after pressure: %v", key, err)
		}
		if h.Size() != indexSize {
			t.Fatalf("%s size after pressure = %d", key, h.Size())
		}
		_ = h.Close()
	}
}

func TestCacheUnclassifiedIndexNameStaysProtected(t *testing.T) {
	dir := t.TempDir()
	cache, err := NewCacheWithIndexBudget(dir, 1<<20, 1<<20, DefaultDownloadConfig())
	if err != nil {
		t.Fatal(err)
	}
	key := "unnamed-metadata-object"
	if err := cache.StoreArchiveIndex(context.Background(), key, []byte("index")); err != nil {
		t.Fatal(err)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	// Rewrite the current "<id>.iu.blob" name as the pre-kind "<id>.i.blob"
	// form so a legacy cache directory can be reopened.
	sum := sha256.Sum256([]byte(key))
	id := hex.EncodeToString(sum[:])
	current := filepath.Join(dir, id+".iu.blob")
	legacy := filepath.Join(dir, id+".i.blob")
	if err := os.Rename(current, legacy); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewCacheWithIndexBudget(dir, 1<<20, 1<<20, DefaultDownloadConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	summary := reopened.Stats()
	if summary.IndexUsedBytes != 5 {
		t.Fatalf("legacy protected bytes = %d, want 5", summary.IndexUsedBytes)
	}
	if got := indexKindByName(t, summary, "unclassified_index"); got.Entries != 1 || got.Bytes != 5 {
		t.Fatalf("legacy classification = %+v", got)
	}
}

func TestCacheLoadDropsDuplicateObjectForOneID(t *testing.T) {
	dir := t.TempDir()
	cache, err := NewCacheWithIndexBudget(dir, 1<<20, 1<<20, DefaultDownloadConfig())
	if err != nil {
		t.Fatal(err)
	}
	key := "archive-index:v1:duplicate-name"
	if err := cache.StoreArchiveIndex(context.Background(), key, []byte("index")); err != nil {
		t.Fatal(err)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(key))
	id := hex.EncodeToString(sum[:])
	current := filepath.Join(dir, id+".ia.blob")
	legacy := filepath.Join(dir, id+".i.blob")
	data, err := os.ReadFile(current)
	if err != nil {
		t.Fatal(err)
	}
	// A cache written before index families were recorded keeps the same id
	// under the old "<id>.i.blob" name alongside the new one.
	if err := os.WriteFile(legacy, data, 0600); err != nil {
		t.Fatal(err)
	}
	older := time.Now().Add(-time.Hour)
	if err := os.Chtimes(legacy, older, older); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewCacheWithIndexBudget(dir, 1<<20, 1<<20, DefaultDownloadConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	summary := reopened.Stats()
	if summary.UsedBytes != int64(len(data)) {
		t.Fatalf("resident bytes = %d, want %d", summary.UsedBytes, len(data))
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("stale duplicate %s survived the reopen: %v", legacy, err)
	}
	if _, err := os.Stat(current); err != nil {
		t.Fatalf("current object was removed: %v", err)
	}
}
