//go:build linux

package main

import (
	"path/filepath"
	"testing"
)

func TestOpenCacheDurabilityModes(t *testing.T) {
	root := t.TempDir()
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
	if ephemeral.Directory() != dir {
		t.Fatal("cache directory changed while closing")
	}
	if _, err = openCache(filepath.Join(root, "invalid"), 1024, "other"); err == nil {
		t.Fatal("invalid durability mode was accepted")
	}
}
