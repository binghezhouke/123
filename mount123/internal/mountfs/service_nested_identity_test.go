package mountfs

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/binghezhouke/123/mount123/internal/storage"
)

func nestedIdentityFixture(t *testing.T, missingLeft bool) ([]byte, map[string][]byte, map[string]string) {
	t.Helper()
	payloads := map[string][]byte{"left": bytes.Repeat([]byte("L"), 8192), "right": bytes.Repeat([]byte("R"), 8192)}
	names := map[string]string{"left": "left.bin", "right": "rght.bin"}
	entries := make(map[string][]byte)
	for _, directory := range []string{"left", "right"} {
		inner := rarSparseStoredZIP(t, names[directory], payloads[directory])
		mid := len(inner) / 2
		entries[directory+"/inner.zip.001"] = inner[:mid]
		if directory != "left" || !missingLeft {
			entries[directory+"/inner.zip.002"] = inner[mid:]
		}
	}
	return multilevelFixture(t, "zip", entries), payloads, names
}

func nestedIdentityRestartService(t *testing.T, service *Service) *Service {
	t.Helper()
	cache, api := service.tree.cache, service.tree.api
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	restored, err := storage.NewCache(cache.Directory(), cache.Capacity())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restored.Close() })
	service, err = NewService(context.Background(), api, restored, 0, true, Options{DisableReadAhead: true, DisableStreamMembers: true, MaxConcurrentBuilds: 1})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

// Both directories contain the same split basenames and equally sized inner
// archives. The complete ancestor path and member locations must keep their
// metadata and plaintext independent across interleaved Service requests.
func TestServiceNestedSplitIdentityKeepsSiblingDirectoriesSeparate(t *testing.T) {
	data, payloads, names := nestedIdentityFixture(t, false)
	service := multilevelService(t, "zip", data, Options{DisableReadAhead: true, DisableStreamMembers: true, MaxConcurrentBuilds: 1}, 32<<20)
	for _, phase := range []string{"same_tree", "restored_tree"} {
		if phase == "restored_tree" {
			service = nestedIdentityRestartService(t, service)
		}
		for round := 0; round < 2; round++ {
			for _, directory := range []string{"left", "right", "left", "right"} {
				path := directory + "/inner.zip.001"
				ctx, endRequest := context.WithCancel(context.Background())
				rows, err := service.ListArchive(ctx, 77, path)
				endRequest()
				if err != nil || len(rows) != 1 || rows[0].Name != names[directory] || rows[0].Size != uint64(len(payloads[directory])) {
					t.Fatalf("%s round=%d directory=%s listing=%v error=%v", phase, round, directory, rows, err)
				}
				multilevelRead(t, service, path+"/"+names[directory], payloads[directory])
			}
		}
	}
}

func TestServiceNestedSplitIdentityMissingVolumeDoesNotBorrowSibling(t *testing.T) {
	data, payloads, names := nestedIdentityFixture(t, true)
	service := multilevelService(t, "zip", data, Options{DisableReadAhead: true, DisableStreamMembers: true, MaxConcurrentBuilds: 1}, 32<<20)
	for _, phase := range []string{"same_tree", "restored_tree"} {
		if phase == "restored_tree" {
			service = nestedIdentityRestartService(t, service)
		}
		for attempt := 0; attempt < 2; attempt++ {
			// Warm the complete right-hand package first, including its .002.
			// The absent left-hand .002 must remain absent after that cache hit.
			rows, err := service.ListArchive(context.Background(), 77, "right/inner.zip.001")
			if err != nil || len(rows) != 1 || rows[0].Name != names["right"] {
				t.Fatalf("%s right listing=%v error=%v", phase, rows, err)
			}
			multilevelRead(t, service, "right/inner.zip.001/"+names["right"], payloads["right"])
			if rows, err := service.ListArchive(context.Background(), 77, "left/inner.zip.001"); err == nil || len(rows) != 0 {
				t.Fatalf("%s attempt=%d missing left volume listing=%v error=%v", phase, attempt, rows, err)
			}
			reader, _, err := service.OpenArchiveMember(context.Background(), 77, "left/inner.zip.001/"+names["left"])
			if reader != nil {
				if closer, ok := reader.(io.Closer); ok {
					_ = closer.Close()
				}
			}
			if err == nil || reader != nil {
				t.Fatalf("%s attempt=%d missing left volume returned a reader: error=%v", phase, attempt, err)
			}
		}
	}
}
