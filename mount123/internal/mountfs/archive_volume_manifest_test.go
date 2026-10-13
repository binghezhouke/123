package mountfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"syscall"
	"testing"

	"github.com/binghezhouke/123/mount123/internal/panapi"
)

func TestArchiveVolumeCanonicalNames(t *testing.T) {
	for _, tc := range []struct{ name, kind, sidecar string }{
		{"sample.zip.001", ".zip", "sample.zip.pwd"},
		{"sample.ZIP.001", ".zip", "sample.ZIP.pwd"},
		{"folder/sample.7z.001", ".7z", "folder/sample.7z.pwd"},
		{"sample.zip.002", "", "sample.zip.002.pwd"},
		{"sample.zip.0001", "", "sample.zip.0001.pwd"},
		{"sample.z01", "", "sample.z01.pwd"},
	} {
		if got := archiveKind(tc.name); got != tc.kind {
			t.Errorf("kind(%q)=%q", tc.name, got)
		}
		if got := archivePasswordName(tc.name); got != tc.sidecar {
			t.Errorf("sidecar(%q)=%q", tc.name, got)
		}
	}
}

func TestArchiveVolumeManifestValidation(t *testing.T) {
	valid := []archiveVolumeCandidate{{"folder/sample.zip.002", 2, false}, {"other/sample.zip.003", 7, false}, {"folder/sample.zip.001", 3, false}}
	indices, sizes, total, err := discoverArchiveVolumes("folder/sample.zip.001", archiveByteSplit, 0, valid)
	if err != nil || fmt.Sprint(indices) != "[2 0]" || fmt.Sprint(sizes) != "[3 2]" || total != 5 {
		t.Fatalf("manifest=%v %v %d %v", indices, sizes, total, err)
	}
	for _, tc := range []struct {
		name  string
		parts []archiveVolumeCandidate
		want  error
	}{
		{"gap", []archiveVolumeCandidate{{"sample.zip.001", 1, false}, {"sample.zip.003", 1, false}}, syscall.EIO},
		{"alias", []archiveVolumeCandidate{{"sample.zip.001", 1, false}, {"sample.zip.0002", 1, false}}, syscall.EIO},
		{"duplicate", []archiveVolumeCandidate{{"sample.zip.001", 1, false}, {"sample.zip.001", 1, false}}, syscall.EIO},
		{"zero", []archiveVolumeCandidate{{"sample.zip.001", 0, false}}, syscall.EIO},
		{"directory", []archiveVolumeCandidate{{"sample.zip.001", 1, false}, {"sample.zip.002", 1, true}}, syscall.EIO},
		{"overflow", []archiveVolumeCandidate{{"sample.zip.001", math.MaxInt64, false}, {"sample.zip.002", 1, false}}, syscall.EFBIG},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, err := discoverArchiveVolumes("sample.zip.001", archiveByteSplit, 0, tc.parts)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err=%v", err)
			}
		})
	}
	// Canonical ordinal 1000 grows to four digits; zero-padded aliases fail.
	parts := make([]archiveVolumeCandidate, 1000)
	for i := range parts {
		parts[i] = archiveVolumeCandidate{fmt.Sprintf("sample.7z.%03d", i+1), 1, false}
	}
	indices, _, total, err = discoverArchiveVolumes("sample.7z.001", archiveByteSplit, 0, parts)
	if err != nil || len(indices) != 1000 || total != 1000 {
		t.Fatalf("four-digit ordinal: %d %d %v", len(indices), total, err)
	}
}

func TestZIPDiskManifestOnlyDeclaredSameStem(t *testing.T) {
	parts := []archiveVolumeCandidate{{"sample.Z01", 4, false}, {"sample.z02", 5, false}, {"sample.zip", 6, false}, {"sample.z03", 0, true}, {"other/sample.z01", 99, false}}
	indices, _, total, err := discoverArchiveVolumes("sample.zip", archiveZIPDisks, 2, parts)
	if err != nil || fmt.Sprint(indices) != "[0 1 2]" || total != 15 {
		t.Fatalf("disk manifest %v %d %v", indices, total, err)
	}
	parts = append(parts, archiveVolumeCandidate{"sample.z01", 4, false})
	if _, _, _, err = discoverArchiveVolumes("sample.zip", archiveZIPDisks, 2, parts); !errors.Is(err, syscall.EIO) {
		t.Fatalf("case alias error=%v", err)
	}
}

func TestCloudSplitIdentityPinsImmutablePartIDsWithoutEagerURLs(t *testing.T) {
	for _, version := range []string{"v1", ""} {
		t.Run("version="+version, func(t *testing.T) {
			files := []panapi.File{{ID: 77, Name: "sample.zip.001", Size: 3, Version: version}, {ID: 78, Name: "sample.zip.002", Size: 3, Version: version}}
			root, api := passwordSaveFixture(t, files, map[int64][]byte{77: []byte("abc"), 78: []byte("def")})
			source, err := root.tree.cloudFileSource(context.Background(), &files[0], root.tree.opts.SourceTTL)
			if err != nil {
				t.Fatal(err)
			}
			before := api.reads.Load()
			a := &archiveDescriptor{id: 77, name: files[0].Name, size: 3, version: version}
			first, err := root.tree.prepareCloudArchiveInput(context.Background(), source, a)
			if err != nil {
				t.Fatal(err)
			}
			second, err := root.tree.prepareCloudArchiveInput(context.Background(), source, a)
			if err != nil {
				t.Fatal(err)
			}
			if first.identity != second.identity || !first.durable || first.size != 6 {
				t.Fatalf("identity/durability %v %v", first, second)
			}
			if api.reads.Load() != before {
				t.Fatal("preparation probed untouched volumes")
			}
			volumes := first.input.(*volumeReaderAt)
			if volumes.sources[1] != nil {
				t.Fatal("untouched part initialized")
			}
			// The preparation remains pinned even if the directory's object is replaced.
			api.mu.Lock()
			api.files[1].ID = 79
			api.data[79] = []byte("ghi")
			api.mu.Unlock()
			replacement := append([]panapi.File(nil), files...)
			replacement[1].ID = 79
			root.tree.mu.Lock()
			for key, item := range root.tree.meta {
				if directory, ok := item.value.(*cloudDirectory); ok {
					copy := *directory
					copy.files = replacement
					item.value = &copy
					root.tree.meta[key] = item
				}
			}
			root.tree.mu.Unlock()
			changed, err := root.tree.prepareCloudArchiveInput(context.Background(), source, a)
			if err != nil {
				t.Fatal(err)
			}
			if changed.identity == first.identity {
				t.Fatal("replacement part ID reused identity")
			}
			got := make([]byte, 3)
			if _, err = first.input.ReadAtContext(context.Background(), got, 3); err != nil || !bytes.Equal(got, []byte("def")) {
				t.Fatalf("pinned bytes %q %v", got, err)
			}
			canceled, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err = root.tree.prepareCloudArchiveInput(canceled, source, a); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel=%v", err)
			}
		})
	}
}

func TestNestedVolumeInitializationCancellationRetry(t *testing.T) {
	// A stored member maps directly only when first read; canceled requests leave
	// the pinned part reusable without an ancestor decoder or leaked closer.
	data := []byte("PK\x03\x04" + string(make([]byte, 26)) + "payload")
	idx := &zipIndex{members: map[string]*member{"sample.zip.001": {name: "sample.zip.001", size: 7, compressed: 7, headerOffset: 0}}}
	idx.attachZIPReader(archivePlainInput{reader: bytes.NewReader(data)})
	parent := &nestedArchive{format: ".zip", size: int64(len(data)), index: idx, input: archivePlainInput{reader: bytes.NewReader(data)}, refs: 1}
	// Local name must agree with central metadata; use empty raw name for this
	// narrowly scoped synthetic local-header mapping test.
	m := idx.members["sample.zip.001"]
	m.name = ""
	idx.members = map[string]*member{"": m}
	root, _ := passwordSaveFixture(t, nil, nil)
	part := &nestedVolumeInput{owner: root.tree, parent: parent, member: m, passwordKnown: true}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := part.ReadAtContext(canceled, make([]byte, 1), 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
	if part.input != nil {
		t.Fatal("canceled request initialized part")
	}
	got := make([]byte, 7)
	n, err := part.ReadAtContext(context.Background(), got, 0)
	if err != nil || n != 7 || string(got) != "payload" {
		t.Fatalf("retry %d %q %v", n, got, err)
	}
	if err = part.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = part.ReadAtContext(context.Background(), make([]byte, 1), 0); err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("closed error=%v", err)
	}
}
