//go:build linux

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/binghezhouke/123/mount123/internal/storage"
)

func TestHelpDocumentsHiddenDirectoryControls(t *testing.T) {
	var output bytes.Buffer
	printUsage(&output)
	for _, name := range []string{".mount123-refresh", ".mount123-probe", ".mount123-probe-status"} {
		if !bytes.Contains(output.Bytes(), []byte(name)) {
			t.Fatalf("help output does not mention %s: %s", name, output.String())
		}
	}
}

func TestOpenCacheWithIndexBudget(t *testing.T) {
	root := t.TempDir()
	const cacheBytes = int64(64) << 20

	derived, err := openCacheWithIndexBudget(filepath.Join(root, "derived"), cacheBytes, 0, "durable", storage.DefaultDownloadConfig())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := derived.Stats().IndexBudgetBytes, cacheBytes/8; got != want {
		t.Fatalf("derived index budget = %d, want %d", got, want)
	}
	if err := derived.Close(); err != nil {
		t.Fatal(err)
	}

	explicit, err := openCacheWithIndexBudget(filepath.Join(root, "explicit"), cacheBytes, 32<<20, "durable", storage.DefaultDownloadConfig())
	if err != nil {
		t.Fatal(err)
	}
	if got := explicit.Stats().IndexBudgetBytes; got != 32<<20 {
		t.Fatalf("explicit index budget = %d, want %d", got, 32<<20)
	}
	if err := explicit.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := openCacheWithIndexBudget(filepath.Join(root, "oversized"), cacheBytes, cacheBytes+1, "durable", storage.DefaultDownloadConfig()); err == nil {
		t.Fatal("an index budget larger than the cache was accepted")
	}
	if _, err := openCacheWithIndexBudget(filepath.Join(root, "invalid"), cacheBytes, 1<<20, "other", storage.DefaultDownloadConfig()); err == nil {
		t.Fatal("invalid durability mode was accepted")
	}
}

func TestOpenCacheDurabilityModes(t *testing.T) {
	root := t.TempDir()
	tokenPath := filepath.Join(root, "ephemeral", "token.json")
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte("simulated token"), 0600); err != nil {
		t.Fatal(err)
	}
	durable, err := openCache(filepath.Join(root, "durable"), 1024, "durable")
	if err != nil {
		t.Fatal(err)
	}
	if durable.Directory() != filepath.Join(root, "durable") {
		t.Fatalf("durable cache directory = %q", durable.Directory())
	}
	if err = durable.Close(); err != nil {
		t.Fatal(err)
	}
	ephemeral, err := openCache(filepath.Join(root, "ephemeral"), 1024, "ephemeral")
	if err != nil {
		t.Fatal(err)
	}
	dir := ephemeral.Directory()
	if filepath.Dir(filepath.Dir(dir)) != root || filepath.Base(filepath.Dir(dir)) != "ephemeral" {
		t.Fatalf("ephemeral cache is not private under the selected root: %q", dir)
	}
	if err = ephemeral.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(tokenPath); err != nil || string(got) != "simulated token" {
		t.Fatalf("ephemeral cleanup changed root token cache: %q, %v", got, err)
	}
	if ephemeral.Directory() != dir {
		t.Fatal("cache directory changed while closing")
	}
	if _, err = openCache(filepath.Join(root, "invalid"), 1024, "other"); err == nil {
		t.Fatal("invalid durability mode was accepted")
	}
}
