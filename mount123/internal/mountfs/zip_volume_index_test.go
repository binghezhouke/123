package mountfs

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"math"
	"syscall"
	"testing"
)

type zipVolumeTestRegion struct {
	offset int64
	data   []byte
}
type zipVolumeSparseInput struct {
	size      int64
	regions   []zipVolumeTestRegion
	readBytes int64
	cancelAt  int64
	cancel    context.CancelFunc
}

func (r *zipVolumeSparseInput) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if off < 0 {
		return 0, syscall.EINVAL
	}
	if off >= r.size {
		return 0, io.EOF
	}
	n := int(min(int64(len(p)), r.size-off))
	clear(p[:n])
	r.readBytes += int64(n)
	for _, region := range r.regions {
		start := max(off, region.offset)
		end := min(off+int64(n), region.offset+int64(len(region.data)))
		if end > start {
			copy(p[start-off:end-off], region.data[start-region.offset:end-region.offset])
		}
	}
	if r.cancel != nil && off == r.cancelAt {
		r.cancel()
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

type zipVolumeTestFixture struct {
	raw            *preparedArchiveInput
	sparse         *zipVolumeSparseInput
	local, central int64
	cd             []byte
}

// Four disk fixtures keep only tiny metadata/data regions in memory. The first
// disk is >4GiB, the directory straddles disks 1/2, and the ZIP64 locator on
// final disk3 points backwards to disk2. Local/body bytes straddle disks 1/2
// in the separate crossing fixture below.
func zipVolumeSparseFixture(zip64Member bool) zipVolumeTestFixture {
	sizes := []int64{1<<32 + 32, 200, 300, 200}
	starts := []int64{0, sizes[0], sizes[0] + 200, sizes[0] + 500}
	payload := []byte("body")
	name := []byte("file.bin")
	local := make([]byte, 30+len(name)+len(payload))
	binary.LittleEndian.PutUint32(local, 0x04034b50)
	binary.LittleEndian.PutUint16(local[4:], 20)
	binary.LittleEndian.PutUint32(local[14:], crc32.ChecksumIEEE(payload))
	binary.LittleEndian.PutUint32(local[18:], 4)
	binary.LittleEndian.PutUint32(local[22:], 4)
	binary.LittleEndian.PutUint16(local[26:], uint16(len(name)))
	copy(local[30:], name)
	copy(local[30+len(name):], payload)
	extra := []byte{0xfe, 0xca, 3, 0, 1, 2, 3}
	if zip64Member {
		z := make([]byte, 32)
		binary.LittleEndian.PutUint16(z, 1)
		binary.LittleEndian.PutUint16(z[2:], 28)
		binary.LittleEndian.PutUint64(z[4:], 4)
		binary.LittleEndian.PutUint64(z[12:], 4)
		binary.LittleEndian.PutUint64(z[20:], 10)
		binary.LittleEndian.PutUint32(z[28:], 1)
		extra = append(extra, z...)
	}
	cd := make([]byte, 46+len(name)+len(extra)+3)
	binary.LittleEndian.PutUint32(cd, zipCentralSignature)
	binary.LittleEndian.PutUint16(cd[4:], 0x314)
	binary.LittleEndian.PutUint16(cd[6:], 20)
	binary.LittleEndian.PutUint32(cd[16:], crc32.ChecksumIEEE(payload))
	binary.LittleEndian.PutUint32(cd[20:], 4)
	binary.LittleEndian.PutUint32(cd[24:], 4)
	binary.LittleEndian.PutUint16(cd[28:], uint16(len(name)))
	binary.LittleEndian.PutUint16(cd[30:], uint16(len(extra)))
	binary.LittleEndian.PutUint16(cd[32:], 3)
	binary.LittleEndian.PutUint16(cd[34:], 1)
	binary.LittleEndian.PutUint32(cd[38:], 0x81a40000)
	binary.LittleEndian.PutUint32(cd[42:], 10)
	copy(cd[46:], name)
	copy(cd[46+len(name):], extra)
	copy(cd[len(cd)-3:], "hey")
	if zip64Member {
		binary.LittleEndian.PutUint32(cd[20:], math.MaxUint32)
		binary.LittleEndian.PutUint32(cd[24:], math.MaxUint32)
		binary.LittleEndian.PutUint16(cd[34:], math.MaxUint16)
		binary.LittleEndian.PutUint32(cd[42:], math.MaxUint32)
	}
	record := make([]byte, 56)
	binary.LittleEndian.PutUint32(record, zip64EOCDSignature)
	binary.LittleEndian.PutUint64(record[4:], 44)
	binary.LittleEndian.PutUint16(record[12:], 45)
	binary.LittleEndian.PutUint16(record[14:], 45)
	binary.LittleEndian.PutUint32(record[16:], 2)
	binary.LittleEndian.PutUint32(record[20:], 1)
	binary.LittleEndian.PutUint64(record[24:], 1)
	binary.LittleEndian.PutUint64(record[32:], 1)
	binary.LittleEndian.PutUint64(record[40:], uint64(len(cd)))
	binary.LittleEndian.PutUint64(record[48:], 170)
	final := make([]byte, 42)
	binary.LittleEndian.PutUint32(final, zip64LocatorSignature)
	binary.LittleEndian.PutUint32(final[4:], 2)
	binary.LittleEndian.PutUint64(final[8:], 110)
	binary.LittleEndian.PutUint32(final[16:], 4)
	h := final[20:]
	binary.LittleEndian.PutUint32(h, zipEOCDSignature)
	binary.LittleEndian.PutUint16(h[4:], 3)
	binary.LittleEndian.PutUint16(h[6:], 1)
	binary.LittleEndian.PutUint16(h[8:], 0)
	binary.LittleEndian.PutUint16(h[10:], 1)
	binary.LittleEndian.PutUint32(h[12:], uint32(len(cd)))
	binary.LittleEndian.PutUint32(h[16:], 170)
	total := starts[3] + sizes[3]
	sparse := &zipVolumeSparseInput{size: total, regions: []zipVolumeTestRegion{{starts[1] + 10, local}, {starts[1] + 170, cd}, {starts[2] + 110, record}, {starts[3] + 50, final}}}
	return zipVolumeTestFixture{raw: &preparedArchiveInput{input: sparse, size: total, layout: archiveVolumeLayout{kind: archiveZIPDisks, sizes: sizes}}, sparse: sparse, local: starts[1] + 10, central: starts[1] + 170, cd: cd}
}

func TestZIPVolumeSparseZIP64Normalization(t *testing.T) {
	for _, member64 := range []bool{false, true} {
		t.Run(map[bool]string{false: "promoteZIP32Offset", true: "conditionalZIP64Fields"}[member64], func(t *testing.T) {
			f := zipVolumeSparseFixture(member64)
			ctx := context.Background()
			last := f.raw.layout.sizes[3]
			final := archiveOffsetInput{input: f.raw.input, start: f.raw.size - last, size: last}
			hint, err := readZIPVolumeHint(ctx, final, last)
			if err != nil || hint.kind != archiveZIPDisks || hint.lastDisk != 3 || hint.centralDisk != 1 {
				t.Fatalf("hint=%+v error=%v", hint, err)
			}
			input, size, err := prepareZIPIndexInput(ctx, f.raw, 10, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			if f.sparse.readBytes > 1000 {
				t.Fatalf("index preparation read payload bytes: %d", f.sparse.readBytes)
			}
			bound := archiveRequestReader{ctx: ctx, input: input}
			if err := preflightZIP(bound, size, 10, 1<<20); err != nil {
				t.Fatal(err)
			}
			offsets, err := zipLocalHeaderOffsets(bound, size, 10)
			if err != nil || len(offsets) != 1 || offsets[0] != f.local {
				t.Fatalf("offsets=%v error=%v", offsets, err)
			}
			zr, err := zip.NewReader(bound, size)
			if err != nil {
				t.Fatal(err)
			}
			if len(zr.File) != 1 {
				t.Fatalf("files=%d", len(zr.File))
			}
			file := zr.File[0]
			if file.Comment != "hey" || file.CreatorVersion != 0x314 || !bytes.Contains(file.Extra, []byte{0xfe, 0xca, 3, 0, 1, 2, 3}) {
				t.Fatalf("metadata was not preserved: %+v", file.FileHeader)
			}
			r, err := file.Open()
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(r)
			r.Close()
			if err != nil || string(data) != "body" {
				t.Fatalf("body=%q error=%v", data, err)
			}
			if size <= f.raw.size {
				t.Fatal("synthetic size does not include metadata")
			}
			rawHeader := make([]byte, 46)
			if _, err := f.raw.input.ReadAtContext(ctx, rawHeader, f.central); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(rawHeader, f.cd[:46]) {
				t.Fatal("raw metadata changed")
			}
		})
	}
}

func TestZIPVolumeMalformedAndLimits(t *testing.T) {
	tests := []struct {
		name    string
		change  func(zipVolumeTestFixture)
		entries int
		budget  int64
		want    error
	}{
		{"entryLimit", func(zipVolumeTestFixture) {}, 0, 1 << 20, syscall.EFBIG},
		{"metadataLimit", func(zipVolumeTestFixture) {}, 10, 100, syscall.EFBIG},
		{"diskOutside", func(f zipVolumeTestFixture) { binary.LittleEndian.PutUint16(f.cd[34:], 4) }, 10, 1 << 20, syscall.EIO},
		{"offsetAtDiskEnd", func(f zipVolumeTestFixture) { binary.LittleEndian.PutUint32(f.cd[42:], 200) }, 10, 1 << 20, syscall.EIO},
		{"offsetOutside", func(f zipVolumeTestFixture) { binary.LittleEndian.PutUint32(f.cd[42:], 201) }, 10, 1 << 20, syscall.EIO},
		{"missingZIP64", func(f zipVolumeTestFixture) { binary.LittleEndian.PutUint32(f.cd[42:], math.MaxUint32) }, 10, 1 << 20, syscall.EIO},
		{"extraTruncated", func(f zipVolumeTestFixture) { binary.LittleEndian.PutUint16(f.cd[46+8+2:], 200) }, 10, 1 << 20, syscall.EIO},
		{"strongEncryption", func(f zipVolumeTestFixture) { binary.LittleEndian.PutUint16(f.cd[8:], 1<<6) }, 10, 1 << 20, syscall.EOPNOTSUPP},
		{"geometrySum", func(f zipVolumeTestFixture) { f.raw.size++ }, 10, 1 << 20, syscall.EIO},
		{"geometryOverflow", func(f zipVolumeTestFixture) { f.raw.layout.sizes = []int64{math.MaxInt64, 1} }, 10, 1 << 20, syscall.EFBIG},
		{"geometryZero", func(f zipVolumeTestFixture) { f.raw.layout.sizes[0] = 0 }, 10, 1 << 20, syscall.EIO},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := zipVolumeSparseFixture(false)
			test.change(f)
			_, _, err := prepareZIPIndexInput(context.Background(), f.raw, test.entries, test.budget)
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v want %v", err, test.want)
			}
		})
	}
}

func TestZIPVolumeCancellationAndRetry(t *testing.T) {
	f := zipVolumeSparseFixture(false)
	ctx, cancel := context.WithCancel(context.Background())
	f.sparse.cancelAt = f.central
	f.sparse.cancel = cancel
	_, _, err := prepareZIPIndexInput(ctx, f.raw, 10, 1<<20)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	f.sparse.cancel = nil
	if _, _, err := prepareZIPIndexInput(context.Background(), f.raw, 10, 1<<20); err != nil {
		t.Fatal(err)
	}
	canceled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if _, _, err := prepareZIPIndexInput(canceled, f.raw, 10, 1<<20); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
}

func TestZIPVolumePassThroughAndDisguisedByteSplit(t *testing.T) {
	ctx := context.Background()
	single := &preparedArchiveInput{input: archivePlainInput{reader: bytes.NewReader([]byte("SFX prefix"))}, size: 10}
	input, size, err := prepareZIPIndexInput(ctx, single, 1, 1)
	if err != nil || input != single.input || size != single.size {
		t.Fatalf("single input changed: %v", err)
	}
	f := zipVolumeSparseFixture(false)
	f.raw.layout.kind = archiveByteSplit
	_, _, err = prepareZIPIndexInput(ctx, f.raw, 10, 1<<20)
	if !errors.Is(err, syscall.EOPNOTSUPP) {
		t.Fatalf("error=%v", err)
	}
}

func TestZIPVolumeHintRejectsSentinelsWithoutLocator(t *testing.T) {
	h := make([]byte, 22)
	binary.LittleEndian.PutUint32(h, zipEOCDSignature)
	binary.LittleEndian.PutUint16(h[4:], math.MaxUint16)
	_, err := readZIPVolumeHint(context.Background(), archivePlainInput{reader: bytes.NewReader(h)}, int64(len(h)))
	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("error=%v", err)
	}
}

func TestZIPVolumeAppendedReaderBoundary(t *testing.T) {
	r := &zipIndexAppendInput{raw: archivePlainInput{reader: bytes.NewReader([]byte("raw"))}, rawSize: 3, tail: []byte("tail")}
	p := make([]byte, 7)
	n, err := r.ReadAtContext(context.Background(), p, 1)
	if n != 6 || !errors.Is(err, io.EOF) || string(p[:n]) != "awtail" {
		t.Fatalf("read=%q n=%d err=%v", p, n, err)
	}
}

func TestZIPVolumeZIP32LocalHeaderAndBodyCrossDisks(t *testing.T) {
	ctx := context.Background()
	payload := []byte("crossing-body")
	name := []byte("file.bin")
	local := make([]byte, 30+len(name)+len(payload))
	binary.LittleEndian.PutUint32(local, 0x04034b50)
	binary.LittleEndian.PutUint16(local[4:], 20)
	binary.LittleEndian.PutUint32(local[14:], crc32.ChecksumIEEE(payload))
	binary.LittleEndian.PutUint32(local[18:], uint32(len(payload)))
	binary.LittleEndian.PutUint32(local[22:], uint32(len(payload)))
	binary.LittleEndian.PutUint16(local[26:], uint16(len(name)))
	copy(local[30:], name)
	copy(local[30+len(name):], payload)
	cd := make([]byte, 46+len(name))
	binary.LittleEndian.PutUint32(cd, zipCentralSignature)
	binary.LittleEndian.PutUint16(cd[6:], 20)
	binary.LittleEndian.PutUint32(cd[16:], crc32.ChecksumIEEE(payload))
	binary.LittleEndian.PutUint32(cd[20:], uint32(len(payload)))
	binary.LittleEndian.PutUint32(cd[24:], uint32(len(payload)))
	binary.LittleEndian.PutUint16(cd[28:], uint16(len(name)))
	binary.LittleEndian.PutUint32(cd[42:], 10)
	copy(cd[46:], name)
	joined := make([]byte, 40+40+len(cd)+22)
	binary.LittleEndian.PutUint32(joined, 0x08074b50)
	copy(joined[10:], local)
	copy(joined[80:], cd)
	end := joined[80+len(cd):]
	binary.LittleEndian.PutUint32(end, zipEOCDSignature)
	binary.LittleEndian.PutUint16(end[4:], 2)
	binary.LittleEndian.PutUint16(end[6:], 2)
	binary.LittleEndian.PutUint16(end[8:], 1)
	binary.LittleEndian.PutUint16(end[10:], 1)
	binary.LittleEndian.PutUint32(end[12:], uint32(len(cd)))
	binary.LittleEndian.PutUint32(end[16:], 40)
	sizes := []int64{20, 20, int64(len(joined) - 40)}
	parts := []archiveReaderAt{archivePlainInput{bytes.NewReader(joined[:20])}, archivePlainInput{bytes.NewReader(joined[20:40])}, archivePlainInput{bytes.NewReader(joined[40:])}}
	raw := &preparedArchiveInput{input: archiveConcatInput{parts: parts, sizes: sizes}, size: int64(len(joined)), layout: archiveVolumeLayout{kind: archiveZIPDisks, sizes: sizes}}
	input, size, err := prepareZIPIndexInput(ctx, raw, 10, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(archiveRequestReader{ctx: ctx, input: input}, size)
	if err != nil {
		t.Fatal(err)
	}
	r, err := zr.File[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(r)
	r.Close()
	if err != nil || !bytes.Equal(body, payload) {
		t.Fatalf("body=%q error=%v", body, err)
	}
}

func TestZIPVolumeAllConditionalZIP64ExtraCombinations(t *testing.T) {
	f := zipVolumeSparseFixture(false)
	g, err := newZIPDiskGeometry(f.raw)
	if err != nil {
		t.Fatal(err)
	}
	for mask := 0; mask < 16; mask++ {
		t.Run(string(rune('A'+mask)), func(t *testing.T) {
			var h [zipCentralHdrLen]byte
			copy(h[:], f.cd[:46])
			name := f.cd[46:54]
			data := make([]byte, 0, 28)
			add64 := func(v uint64) { var p [8]byte; binary.LittleEndian.PutUint64(p[:], v); data = append(data, p[:]...) }
			if mask&1 != 0 {
				binary.LittleEndian.PutUint32(h[24:], math.MaxUint32)
				add64(4)
			}
			if mask&2 != 0 {
				binary.LittleEndian.PutUint32(h[20:], math.MaxUint32)
				add64(4)
			}
			if mask&4 != 0 {
				binary.LittleEndian.PutUint32(h[42:], math.MaxUint32)
				add64(10)
			}
			if mask&8 != 0 {
				binary.LittleEndian.PutUint16(h[34:], math.MaxUint16)
				var d [4]byte
				binary.LittleEndian.PutUint32(d[:], 1)
				data = append(data, d[:]...)
			}
			extra := make([]byte, 4)
			binary.LittleEndian.PutUint16(extra, 1)
			binary.LittleEndian.PutUint16(extra[2:], uint16(len(data)))
			extra = append(extra, data...)
			binary.LittleEndian.PutUint16(h[30:], uint16(len(extra)))
			variable := append(append(append([]byte{}, name...), extra...), []byte("hey")...)
			normalized, err := normalizeZIPDiskRecord(h, variable, len(name), len(extra), g, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			extraLength := int(binary.LittleEndian.Uint16(normalized[30:]))
			newExtra := normalized[54 : 54+extraLength]
			global, err := zip64LocalHeaderOffset(newExtra, mask&1 != 0, mask&2 != 0)
			if err != nil || global != uint64(f.local) {
				t.Fatalf("global=%d error=%v mask=%d", global, err, mask)
			}
			if binary.LittleEndian.Uint16(normalized[34:]) != 0 {
				t.Fatal("disk was not normalized")
			}
			if _, err := normalizeZIPDiskRecord(h, variable, len(name), len(extra), g, int64(len(normalized)-1)); !errors.Is(err, syscall.EFBIG) {
				t.Fatalf("expansion allocation budget error=%v", err)
			}
			if mask != 0 {
				truncated := append([]byte(nil), variable...)
				binary.LittleEndian.PutUint16(truncated[len(name)+2:], uint16(len(data)-1))
				truncated = append(truncated[:len(name)+len(extra)-1], truncated[len(name)+len(extra):]...)
				_, err := normalizeZIPDiskRecord(h, truncated, len(name), len(extra)-1, g, 1<<20)
				if !errors.Is(err, syscall.EIO) {
					t.Fatalf("missing conditional byte error=%v", err)
				}
			}
		})
	}
}

func TestZIPVolumeSingleZIP64SentinelHint(t *testing.T) {
	h := make([]byte, 42)
	binary.LittleEndian.PutUint32(h, zip64LocatorSignature)
	binary.LittleEndian.PutUint32(h[16:], 1)
	end := h[20:]
	binary.LittleEndian.PutUint32(end, zipEOCDSignature)
	binary.LittleEndian.PutUint16(end[4:], math.MaxUint16)
	binary.LittleEndian.PutUint16(end[6:], math.MaxUint16)
	hint, err := readZIPVolumeHint(context.Background(), archivePlainInput{reader: bytes.NewReader(h)}, int64(len(h)))
	if err != nil || hint.kind != archiveSingle || hint.lastDisk != 0 {
		t.Fatalf("hint=%+v error=%v", hint, err)
	}
}

func TestZIPVolumeInvalidZIP64EndGeometry(t *testing.T) {
	changes := []struct {
		name   string
		change func(zipVolumeTestFixture)
		want   error
	}{
		{"locatorCount", func(f zipVolumeTestFixture) { binary.LittleEndian.PutUint32(f.sparse.regions[3].data[16:], 5) }, syscall.EIO},
		{"locatorRecordDisk", func(f zipVolumeTestFixture) { binary.LittleEndian.PutUint32(f.sparse.regions[3].data[4:], 4) }, syscall.EIO},
		{"endDisk", func(f zipVolumeTestFixture) { binary.LittleEndian.PutUint32(f.sparse.regions[2].data[16:], 1) }, syscall.EIO},
		{"endSizeOverflow", func(f zipVolumeTestFixture) {
			binary.LittleEndian.PutUint64(f.sparse.regions[2].data[4:], math.MaxUint64)
		}, syscall.EIO},
		{"endDirectoryDisk", func(f zipVolumeTestFixture) {
			binary.LittleEndian.PutUint32(f.sparse.regions[2].data[20:], 4)
			binary.LittleEndian.PutUint16(f.sparse.regions[3].data[26:], math.MaxUint16)
		}, syscall.EIO},
		{"countOverTotal", func(f zipVolumeTestFixture) { binary.LittleEndian.PutUint64(f.sparse.regions[2].data[24:], 2) }, syscall.EIO},
		{"ZIP32CountOverTotal", func(f zipVolumeTestFixture) { binary.LittleEndian.PutUint16(f.sparse.regions[3].data[28:], 2) }, syscall.EIO},
		{"ZIP32CountMismatch", func(f zipVolumeTestFixture) { binary.LittleEndian.PutUint16(f.sparse.regions[3].data[30:], 2) }, syscall.EIO},
		{"directoryOverlap", func(f zipVolumeTestFixture) {
			binary.LittleEndian.PutUint64(f.sparse.regions[2].data[40:], 200)
			binary.LittleEndian.PutUint32(f.sparse.regions[3].data[32:], 200)
		}, syscall.EIO},
		{"sentinelCentralDisk", func(f zipVolumeTestFixture) {
			binary.LittleEndian.PutUint16(f.sparse.regions[3].data[26:], math.MaxUint16)
		}, nil},
	}
	for _, test := range changes {
		t.Run(test.name, func(t *testing.T) {
			f := zipVolumeSparseFixture(false)
			test.change(f)
			_, _, err := prepareZIPIndexInput(context.Background(), f.raw, 10, 1<<20)
			if test.want == nil {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, test.want) {
				t.Fatalf("error=%v want=%v", err, test.want)
			}
		})
	}
}

func TestZIPVolumeRejectsDuplicateAndAmbiguousZIP64Extras(t *testing.T) {
	f := zipVolumeSparseFixture(false)
	g, err := newZIPDiskGeometry(f.raw)
	if err != nil {
		t.Fatal(err)
	}
	var h [46]byte
	copy(h[:], f.cd[:46])
	name := f.cd[46:54]
	for _, extra := range [][]byte{{1, 0, 0, 0, 1, 0, 0, 0}, {1, 0, 8, 0, 0, 0, 0, 0, 0, 0, 0, 0}} {
		variable := append(append(append([]byte{}, name...), extra...), []byte("hey")...)
		if _, err := normalizeZIPDiskRecord(h, variable, 8, len(extra), g, 1<<20); !errors.Is(err, syscall.EIO) {
			t.Fatalf("extra=%v error=%v", extra, err)
		}
	}
	extra := make([]byte, 65534)
	binary.LittleEndian.PutUint16(extra, 0xbeef)
	binary.LittleEndian.PutUint16(extra[2:], 65530)
	variable := append(append(append([]byte{}, name...), extra...), []byte("hey")...)
	if _, err := normalizeZIPDiskRecord(h, variable, 8, len(extra), g, 1<<20); !errors.Is(err, syscall.EFBIG) {
		t.Fatalf("promoted extra length error=%v", err)
	}
}

func TestZIPVolumeSyntheticSizeOverflow(t *testing.T) {
	end := make([]byte, 22)
	binary.LittleEndian.PutUint32(end, zipEOCDSignature)
	binary.LittleEndian.PutUint16(end[4:], 1)
	sparse := &zipVolumeSparseInput{size: math.MaxInt64, regions: []zipVolumeTestRegion{{math.MaxInt64 - 22, end}}}
	raw := &preparedArchiveInput{input: sparse, size: math.MaxInt64, layout: archiveVolumeLayout{kind: archiveZIPDisks, sizes: []int64{math.MaxInt64 - 200, 200}}}
	_, _, err := prepareZIPIndexInput(context.Background(), raw, 1, 1<<20)
	if !errors.Is(err, syscall.EFBIG) {
		t.Fatalf("error=%v", err)
	}
}

func TestZIPVolumeByteSplitMissingEndIsIntegrityError(t *testing.T) {
	data := make([]byte, 100)
	raw := &preparedArchiveInput{input: archivePlainInput{reader: bytes.NewReader(data)}, size: int64(len(data)), layout: archiveVolumeLayout{kind: archiveByteSplit}}
	_, _, err := prepareZIPIndexInput(context.Background(), raw, 1, 1<<20)
	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("error=%v", err)
	}
}
