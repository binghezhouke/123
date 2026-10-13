package mountfs

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func TestFUSEZIPLookupRetainsFrozenVolumeInput(t *testing.T) {
	for _, path := range []string{"lookup", "readdirplus"} {
		t.Run(path, func(t *testing.T) {
			var encoded bytes.Buffer
			zw := zip.NewWriter(&encoded)
			payload := bytes.Repeat([]byte("sample-content-"), 35000)
			w, err := zw.CreateHeader(&zip.FileHeader{Name: "payload.bin", Method: zip.Store})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = w.Write(payload); err != nil {
				t.Fatal(err)
			}
			if err = zw.Close(); err != nil {
				t.Fatal(err)
			}
			_, root, api := rootZIPSplitFixture(t, encoded.Bytes(), 64<<10, nil, Options{DisableReadAhead: true})
			archive := lookup(t, root, "sample.zip.001")
			var leaf *Node
			if path == "lookup" {
				leaf = lookup(t, archive, "payload.bin")
			} else {
				h, _, errno := archive.OpendirHandle(context.Background(), 0)
				if errno != 0 {
					t.Fatal(errno)
				}
				defer h.(fs.FileReleasedirer).Releasedir(context.Background(), 0)
				inode, errno := h.(fs.FileLookuper).Lookup(context.Background(), "payload.bin", &fuse.EntryOut{})
				if errno != 0 {
					t.Fatal(errno)
				}
				leaf = inode.Operations().(*Node)
			}
			if leaf.item.index == nil {
				t.Fatal("metadata lookup omitted frozen index")
			}
			// A new object ID changes the listing, while the old immutable volume and
			// old open snapshot remain available. Its endpoint is deliberately absent.
			api.mu.Lock()
			api.files[1].ID = 82
			api.mu.Unlock()
			if _, err = root.RefreshDirectory(context.Background(), ""); err != nil {
				t.Fatal(err)
			}
			got := readNode(t, leaf, 70000, 100)
			if !bytes.Equal(got, payload[70000:70100]) {
				t.Fatalf("old snapshot bytes=%q", got)
			}
		})
	}
}

func TestCloud7zSplitPreservesLegacyCacheIdentity(t *testing.T) {
	files := []panapi.File{{ID: 77, Name: "sample.7z.001", Size: 3, Version: "v1"}, {ID: 78, Name: "sample.7z.002", Size: 3, Version: "v1"}}
	root, _ := passwordSaveFixture(t, files, map[int64][]byte{77: []byte("abc"), 78: []byte("def")})
	source, err := root.tree.cloudFileSource(context.Background(), &files[0], root.tree.opts.SourceTTL)
	if err != nil {
		t.Fatal(err)
	}
	a := &archiveDescriptor{id: 77, name: files[0].Name, size: 3, version: "v1"}
	prepared, err := root.tree.prepareCloudArchiveInput(context.Background(), source, a)
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{root.tree.cloudCacheKey(&files[0]), root.tree.cloudCacheKey(&files[1])}
	want := archiveIdentity(source, a) + ":" + strings.Join(keys, "|")
	if prepared.identity != want {
		t.Fatalf("existing 7z key changed: %q != %q", prepared.identity, want)
	}
}

func TestNestedArchivesPreserveLegacyCacheIdentity(t *testing.T) {
	seven, err := os.ReadFile("testdata/encrypted/plain.7z")
	if err != nil {
		t.Fatal(err)
	}
	ordinary := multilevelFixture(t, "zip", map[string][]byte{"payload.txt": []byte("sample")})
	for _, tc := range []struct {
		name    string
		entries map[string][]byte
	}{
		{"inner.zip", map[string][]byte{"inner.zip": ordinary}},
		{"inner.7z", map[string][]byte{"inner.7z": seven}},
		{"inner.7z.001", map[string][]byte{"inner.7z.001": seven[:len(seven)/2], "inner.7z.002": seven[len(seven)/2:]}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outer := multilevelFixture(t, "zip", tc.entries)
			service := multilevelService(t, "zip", outer, Options{}, 4<<20)
			idx, a, source, password, err := service.outerArchiveIndex(context.Background(), 77)
			if err != nil {
				t.Fatal(err)
			}
			defer clear(password)
			parent, err := service.tree.rootArchiveView(context.Background(), source, a, idx, password)
			if err != nil {
				t.Fatal(err)
			}
			defer parent.Close()
			m := idx.members[tc.name]
			child, err := service.tree.enterNestedArchive(context.Background(), parent, tc.name, m)
			if err != nil {
				t.Fatal(err)
			}
			defer child.Close()
			parts, _, err := nestedParts(idx, m)
			if err != nil {
				t.Fatal(err)
			}
			type legacyPart struct {
				Name    string
				Ordinal int
				Size    uint64
				CRC     uint32
			}
			identities := make([]legacyPart, len(parts))
			for i, part := range parts {
				identities[i] = legacyPart{part.name, part.ordinal, part.size, part.crc}
			}
			parentPassword, err := service.tree.viewPassword(context.Background(), parent, false)
			if err != nil {
				t.Fatal(err)
			}
			defer clear(parentPassword)
			encoded, _ := json.Marshal(struct {
				Parent, ParentPassword, Format string
				Parts                          []legacyPart
			}{parent.identity, service.tree.passwordTag(parent.archive, parentPassword), archiveKind(tc.name), identities})
			want := service.tree.cache.StableDigest("nested-view-input-v1", string(encoded))
			if child.cachePrefix != want {
				t.Fatalf("existing nested cache key changed: %q != %q", child.cachePrefix, want)
			}
		})
	}
}
