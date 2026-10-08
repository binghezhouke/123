package discimage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

const testSectorSize = 2048

func TestUDFRealtimeVideoRemainsVisibleAndReadable(t *testing.T) {
	data := readDiscFixture(t, "udf250-metadata.iso")
	// split.bin's extended file entry is at physical partition block 13.
	// Blu-ray video streams use UDF real-time file type 249 instead of 5.
	data[(400+13)*testSectorSize+27] = 249
	image, err := Open(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := image.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	if entrySize(t, entries, "split.bin") != 8 {
		t.Fatal("real-time file omitted")
	}
	r, err := image.OpenFile("split.bin")
	if err != nil {
		t.Fatal(err)
	}
	data = make([]byte, 4)
	if n, err := r.ReadAt(data, 2); n != 4 || err != nil || string(data) != "CDEF" {
		t.Fatalf("real-time read = %q, %d, %v", data, n, err)
	}
}

func readDiscFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "mountfs", "testdata", "disc", "images", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestUDF250MetadataPartitionMultiExtentAndSparseReadAt(t *testing.T) {
	data := readDiscFixture(t, "udf250-metadata.iso")
	image, err := Open(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("open UDF 2.50 image: %v", err)
	}
	if image.Format != "udf" {
		t.Fatalf("format = %q, want udf", image.Format)
	}
	entries, err := image.ReadDir(".")
	if err != nil {
		t.Fatalf("read metadata-partition root: %v", err)
	}
	if got := entrySize(t, entries, "split.bin"); got != 8 {
		t.Fatalf("split.bin size = %d, want 8", got)
	}
	if got := entrySize(t, entries, "sparse.bin"); got != (2<<20)+4 {
		t.Fatalf("sparse.bin size = %d, want %d", got, (2<<20)+4)
	}

	split, err := image.OpenFile("split.bin")
	if err != nil {
		t.Fatalf("open split.bin: %v", err)
	}
	chunk := make([]byte, 4)
	if n, err := split.ReadAt(chunk, 2); err != nil || n != len(chunk) || string(chunk) != "CDEF" {
		t.Fatalf("split.bin ReadAt(2) = %q, n=%d, err=%v; want CDEF", chunk, n, err)
	}

	sparse, err := image.OpenFile("sparse.bin")
	if err != nil {
		t.Fatalf("open sparse.bin: %v", err)
	}
	gap := make([]byte, 16)
	if n, err := sparse.ReadAt(gap, 1<<20); err != nil || n != len(gap) || !bytes.Equal(gap, make([]byte, len(gap))) {
		t.Fatalf("sparse hole ReadAt = %x, n=%d, err=%v; want zeroes", gap, n, err)
	}
	tail := make([]byte, 4)
	if n, err := sparse.ReadAt(tail, (2 << 20)); err != nil || n != len(tail) || string(tail) != "TAIL" {
		t.Fatalf("sparse tail ReadAt = %q, n=%d, err=%v; want TAIL", tail, n, err)
	}
}

func entrySize(t *testing.T, entries []Entry, name string) int64 {
	t.Helper()
	for _, entry := range entries {
		if entry.Name == name {
			return entry.Size
		}
	}
	t.Fatalf("entry %q not found", name)
	return 0
}

func TestISORejectsMultiExtentAndInterleavedRecords(t *testing.T) {
	base := readDiscFixture(t, "iso9660-base.iso")
	for _, tc := range []struct {
		name   string
		mutate func([]byte)
	}{
		{name: "multi-extent", mutate: func(record []byte) { record[25] |= 0x80 }},
		{name: "interleaved", mutate: func(record []byte) { record[26], record[27] = 1, 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := append([]byte(nil), base...)
			mutateISOFileRecord(t, data, tc.mutate)
			image, err := Open(bytes.NewReader(data), int64(len(data)))
			if err != nil {
				t.Fatalf("open image: %v", err)
			}
			if _, err := image.ReadDir("."); !errors.Is(err, syscall.EOPNOTSUPP) {
				t.Fatalf("ReadDir error = %v, want EOPNOTSUPP", err)
			}
		})
	}
}

func TestISORejectsRockRidgeSparseFileRecord(t *testing.T) {
	data := readDiscFixture(t, "rockridge-long-path.iso")
	rootOffset, rootSize := isoRootExtent(t, data)
	mutated := data[rootOffset : rootOffset+rootSize]
	var self []byte
	for pos := 0; pos < len(mutated); {
		length := int(mutated[pos])
		if length == 0 {
			pos = (pos/testSectorSize + 1) * testSectorSize
			continue
		}
		record := mutated[pos : pos+length]
		if record[32] == 1 && record[33] == 0 {
			self = record
			break
		}
		pos += length
	}
	if self == nil {
		t.Fatal("Rock Ridge self record not found")
	}
	systemUse := 33 + int(self[32])
	if self[32]%2 == 0 {
		systemUse++
	}
	for pos := systemUse; pos+4 <= len(self); {
		size := int(self[pos+2])
		if size < 4 || pos+size > len(self) {
			break
		}
		if string(self[pos:pos+2]) == "TF" && size >= 16 {
			// Replace the 26-byte timestamp field with a valid RRIP 1.10 SF
			// record, a SUSP terminator, and zero padding.
			clear(self[pos : pos+size])
			copy(self[pos:], "SF")
			self[pos+2], self[pos+3] = 12, 1
			copy(self[pos+12:], []byte{'S', 'T', 4, 1})
			goto check
		}
		pos += size
	}
	t.Fatal("Rock Ridge timestamp extension not found")

check:
	image, err := Open(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("open image: %v", err)
	}
	if _, err := image.ReadDir("."); !errors.Is(err, syscall.EOPNOTSUPP) {
		t.Fatalf("ReadDir error = %v, want EOPNOTSUPP for SF record", err)
	}
}

func TestISORejectsOversizedMetadataAndTruncatedDirectory(t *testing.T) {
	base := readDiscFixture(t, "iso9660-base.iso")
	oversized := append([]byte(nil), base...)
	binary.LittleEndian.PutUint32(oversized[16*testSectorSize+132:], maxDirectoryBytes+1)
	if _, err := Open(bytes.NewReader(oversized), int64(len(oversized))); !errors.Is(err, syscall.EFBIG) {
		t.Fatalf("oversized metadata Open error = %v, want EFBIG", err)
	}

	rootOffset, rootSize := isoRootExtent(t, base)
	truncatedSize := rootOffset + rootSize - 1
	truncated := base[:truncatedSize]
	image, err := Open(bytes.NewReader(truncated), int64(len(truncated)))
	if err == nil {
		_, err = image.ReadDir(".")
	}
	if err == nil {
		t.Fatalf("truncated root directory error = %v, want a short-read error", err)
	}
}

func mutateISOFileRecord(t *testing.T, image []byte, mutate func([]byte)) {
	t.Helper()
	rootOffset, rootSize := isoRootExtent(t, image)
	root := image[rootOffset : rootOffset+rootSize]
	for pos := 0; pos < len(root); {
		length := int(root[pos])
		if length == 0 {
			pos = (pos/testSectorSize + 1) * testSectorSize
			continue
		}
		record := root[pos : pos+length]
		nameLength := int(record[32])
		name := strings.TrimSuffix(string(record[33:33+nameLength]), ";1")
		if name == "CHECK.BIN" {
			mutate(record)
			return
		}
		pos += length
	}
	t.Fatal("CHECK.BIN directory record not found")
}

func isoRootExtent(t *testing.T, image []byte) (int, int) {
	t.Helper()
	if len(image) < 17*testSectorSize {
		t.Fatal("ISO image is too short for its primary descriptor")
	}
	pvd := image[16*testSectorSize : 17*testSectorSize]
	if string(pvd[1:6]) != "CD001" || pvd[0] != 1 {
		t.Fatal("primary volume descriptor missing")
	}
	root := pvd[156:190]
	lba := int(binary.LittleEndian.Uint32(root[2:]))
	size := int(binary.LittleEndian.Uint32(root[10:]))
	offset := lba * testSectorSize
	if offset < 0 || size < 0 || offset > len(image) || size > len(image)-offset {
		t.Fatal("ISO root extent is outside image")
	}
	return offset, size
}

func TestUDFAllocationDescriptorChainBeyondLegacyDepth(t *testing.T) {
	data := readDiscFixture(t, "udf250-metadata.iso")
	installUDFAllocationChain(t, data, 35, false)
	image, err := Open(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("open image: %v", err)
	}
	r, err := image.OpenFile("split.bin")
	if err != nil {
		t.Fatalf("open split.bin through 35 allocation extents: %v", err)
	}
	got := make([]byte, 8)
	if n, err := r.ReadAt(got, 0); err != nil || n != len(got) || string(got) != "CHAINED!" {
		t.Fatalf("ReadAt through descriptor chain = %q, n=%d err=%v", got, n, err)
	}
}

func TestUDFAllocationDescriptorCycleAndByteBudgetAreRejected(t *testing.T) {
	t.Run("cycle", func(t *testing.T) {
		data := readDiscFixture(t, "udf250-metadata.iso")
		installUDFAllocationChain(t, data, 35, true)
		image, err := Open(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatalf("open image: %v", err)
		}
		if _, err := image.OpenFile("split.bin"); err == nil {
			t.Fatal("OpenFile accepted a cyclic allocation-descriptor chain")
		}
	})

	t.Run("descriptor-bytes", func(t *testing.T) {
		data := readDiscFixture(t, "udf250-metadata.iso")
		fe := data[(400+13)*testSectorSize:]
		binary.LittleEndian.PutUint32(fe[212:], 16)
		binary.LittleEndian.PutUint32(fe[216:], (3<<30)|((16<<20)+1))
		binary.LittleEndian.PutUint32(fe[220:], 40)
		binary.LittleEndian.PutUint16(fe[224:], 0)
		image, err := Open(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatalf("open image: %v", err)
		}
		if _, err := image.OpenFile("split.bin"); err == nil {
			t.Fatal("OpenFile accepted a descriptor chain over the 16 MiB budget")
		}
	})
}

func installUDFAllocationChain(t *testing.T, image []byte, blocks int, cycle bool) {
	t.Helper()
	const (
		firstDescriptor = uint32(40)
		payloadBlock    = uint32(75)
	)
	if blocks < 1 || firstDescriptor+uint32(blocks) > payloadBlock {
		t.Fatalf("invalid chain fixture length %d", blocks)
	}
	fe := image[(400+13)*testSectorSize:]
	binary.LittleEndian.PutUint32(fe[212:], 16)
	binary.LittleEndian.PutUint32(fe[216:], (3<<30)|40)
	binary.LittleEndian.PutUint32(fe[220:], firstDescriptor)
	binary.LittleEndian.PutUint16(fe[224:], 0)

	for i := 0; i < blocks; i++ {
		block := firstDescriptor + uint32(i)
		aed := image[(400+int(block))*testSectorSize:]
		binary.LittleEndian.PutUint16(aed[0:], 0x102)
		binary.LittleEndian.PutUint16(aed[2:], 3)
		binary.LittleEndian.PutUint32(aed[20:], 16)
		ad := aed[24:]
		switch {
		case i < blocks-1:
			binary.LittleEndian.PutUint32(ad[0:], (3<<30)|40)
			binary.LittleEndian.PutUint32(ad[4:], block+1)
		case cycle:
			binary.LittleEndian.PutUint32(ad[0:], (3<<30)|40)
			binary.LittleEndian.PutUint32(ad[4:], firstDescriptor)
		default:
			binary.LittleEndian.PutUint32(ad[0:], 8)
			binary.LittleEndian.PutUint32(ad[4:], payloadBlock)
		}
		binary.LittleEndian.PutUint16(ad[8:], 0)
	}
	if !cycle {
		copy(image[(400+int(payloadBlock))*testSectorSize:], "CHAINED!")
	}
}
