package mountfs

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"syscall"
)

// ZIP disk addresses are specified by PKWARE APPNOTE 4.3.14, 4.4.16,
// 4.4.24 and 4.5.3. Only directory/end metadata is rewritten. In particular,
// the first disk's spanning marker and every local header remain at raw offsets.
type zipDiskEnd struct {
	pos        int64 // relative to the final input supplied to readZIPDiskEnd
	header     [zipEOCDLen]byte
	locator    [zip64LocatorLen]byte
	hasLocator bool
}

func zipDiskError(message string) error {
	return fmt.Errorf("ZIP disk metadata: %s: %w", message, syscall.EIO)
}
func zipDiskLimit(message string) error {
	return fmt.Errorf("ZIP disk metadata: %s: %w", message, syscall.EFBIG)
}
func zipDiskUnsupported(message string) error {
	return fmt.Errorf("ZIP disk metadata: %s: %w", message, syscall.EOPNOTSUPP)
}

func readZIPDiskEnd(ctx context.Context, input archiveReaderAt, size int64) (zipDiskEnd, error) {
	var end zipDiskEnd
	if input == nil || size < zipEOCDLen {
		return end, zipDiskError("end record is missing")
	}
	if err := ctx.Err(); err != nil {
		return end, err
	}
	length := min(size, int64(zipEOCDMaxTail))
	tail := make([]byte, int(length))
	r := archiveRequestReader{ctx: ctx, input: input}
	if err := zipReadAt(r, tail, size-length); err != nil {
		return end, err
	}
	found := false
	for i := len(tail) - zipEOCDLen; i >= 0; i-- {
		if binary.LittleEndian.Uint32(tail[i:]) != zipEOCDSignature {
			continue
		}
		if i+zipEOCDLen+int(binary.LittleEndian.Uint16(tail[i+20:])) > len(tail) {
			continue
		}
		end.pos = size - length + int64(i)
		copy(end.header[:], tail[i:i+zipEOCDLen])
		found = true
		break
	}
	if !found {
		return end, zipDiskUnsupported("end record is not reachable in the final-volume tail")
	}
	if end.pos >= zip64LocatorLen {
		if err := zipReadAt(r, end.locator[:], end.pos-zip64LocatorLen); err != nil {
			return end, err
		}
		end.hasLocator = binary.LittleEndian.Uint32(end.locator[:]) == zip64LocatorSignature
	}
	h := end.header[:]
	needs64 := binary.LittleEndian.Uint16(h[4:]) == math.MaxUint16 || binary.LittleEndian.Uint16(h[6:]) == math.MaxUint16 || binary.LittleEndian.Uint16(h[8:]) == math.MaxUint16 || binary.LittleEndian.Uint16(h[10:]) == math.MaxUint16 || binary.LittleEndian.Uint32(h[12:]) == math.MaxUint32 || binary.LittleEndian.Uint32(h[16:]) == math.MaxUint32
	if needs64 && !end.hasLocator {
		return end, zipDiskError("ZIP64 locator is missing")
	}
	if err := ctx.Err(); err != nil {
		return end, err
	}
	return end, nil
}

// readZIPVolumeHint reads only a bounded tail of the final volume. A ZIP64
// locator gives the volume count even when its end record lives on another disk.
func readZIPVolumeHint(ctx context.Context, finalInput archiveReaderAt, finalSize int64) (zipVolumeHint, error) {
	end, err := readZIPDiskEnd(ctx, finalInput, finalSize)
	if err != nil {
		return zipVolumeHint{}, err
	}
	return readZIPVolumeHintFromEnd(end)
}

// zipIndexAppendInput exposes immutable normalized metadata after the untouched
// raw archive. Index consumers see this larger size; member consumers keep raw.
type zipIndexAppendInput struct {
	raw     archiveReaderAt
	rawSize int64
	tail    []byte
}

func (r *zipIndexAppendInput) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if off < 0 {
		return 0, syscall.EINVAL
	}
	if len(p) == 0 {
		return 0, nil
	}
	total := 0
	if off < r.rawSize {
		amount := min(int64(len(p)), r.rawSize-off)
		n, err := r.raw.ReadAtContext(ctx, p[:amount], off)
		total = n
		off += int64(n)
		p = p[n:]
		if err != nil && !(err == io.EOF && int64(n) == amount) {
			return total, err
		}
		if int64(n) != amount {
			return total, io.ErrUnexpectedEOF
		}
	}
	if len(p) == 0 {
		return total, nil
	}
	if err := ctx.Err(); err != nil {
		return total, err
	}
	tailOff := off - r.rawSize
	if tailOff >= int64(len(r.tail)) {
		return total, io.EOF
	}
	n := copy(p, r.tail[tailOff:])
	total += n
	if n < len(p) {
		return total, io.EOF
	}
	return total, nil
}

type zipDiskGeometry struct {
	sizes, starts []int64
	total         int64
}

func newZIPDiskGeometry(raw *preparedArchiveInput) (zipDiskGeometry, error) {
	g := zipDiskGeometry{sizes: raw.layout.sizes}
	if len(g.sizes) < 2 {
		return g, zipDiskError("disk manifest is incomplete")
	}
	// Discovery has its own volume cap; this check also protects direct callers.
	if len(g.sizes) > 1000 {
		return g, zipDiskLimit("too many volumes")
	}
	g.starts = make([]int64, len(g.sizes)+1)
	for i, size := range g.sizes {
		if size <= 0 {
			return g, zipDiskError("volume size is invalid")
		}
		if size > math.MaxInt64-g.total {
			return g, zipDiskLimit("volume sizes overflow")
		}
		g.total += size
		g.starts[i+1] = g.total
	}
	if g.total != raw.size {
		return g, zipDiskError("manifest size disagrees with raw input")
	}
	return g, nil
}
func (g zipDiskGeometry) address(disk uint32, offset uint64, allowEnd bool) (int64, error) {
	if uint64(disk) >= uint64(len(g.sizes)) {
		return 0, zipDiskError("disk address is outside the manifest")
	}
	size := uint64(g.sizes[disk])
	if offset > size || (!allowEnd && offset == size) {
		return 0, zipDiskError("disk-relative offset is outside its volume")
	}
	return g.starts[disk] + int64(offset), nil
}

// prepareZIPIndexInput translates disk-relative addresses into raw global
// addresses and emits a bounded, single-disk ZIP64 directory for existing ZIP
// parsers. Single inputs keep their established self-extractor handling.
func prepareZIPIndexInput(ctx context.Context, raw *preparedArchiveInput, maxEntries int, maxBytes int64) (archiveReaderAt, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if raw == nil || raw.input == nil || raw.size < 0 || maxEntries < 0 || maxBytes < 0 {
		return nil, 0, zipDiskError("invalid input or limits")
	}
	if raw.layout.kind == archiveSingle {
		return raw.input, raw.size, nil
	}
	if raw.layout.kind == archiveByteSplit {
		hint, err := readZIPVolumeHint(ctx, raw.input, raw.size)
		if err != nil {
			if errors.Is(err, syscall.EOPNOTSUPP) {
				return nil, 0, zipDiskError("byte-split end record is missing or malformed")
			}
			return nil, 0, err
		}
		if hint.kind == archiveZIPDisks {
			return nil, 0, zipDiskUnsupported("byte-split input declares ZIP disk addresses")
		}
		return raw.input, raw.size, nil
	}
	if raw.layout.kind != archiveZIPDisks {
		return nil, 0, zipDiskError("unknown split kind")
	}
	g, err := newZIPDiskGeometry(raw)
	if err != nil {
		return nil, 0, err
	}
	last := len(g.sizes) - 1
	final := archiveOffsetInput{input: raw.input, start: g.starts[last], size: g.sizes[last]}
	end, err := readZIPDiskEnd(ctx, final, g.sizes[last])
	if err != nil {
		return nil, 0, err
	}
	hint, err := readZIPVolumeHintFromEnd(end)
	if err != nil {
		return nil, 0, err
	}
	if hint.kind != archiveZIPDisks || uint64(hint.lastDisk) != uint64(last) {
		return nil, 0, zipDiskError("end disk disagrees with manifest")
	}
	h := end.header[:]
	entriesOnDisk := uint64(binary.LittleEndian.Uint16(h[8:]))
	entries := uint64(binary.LittleEndian.Uint16(h[10:]))
	cdSize := uint64(binary.LittleEndian.Uint32(h[12:]))
	cdOffset := uint64(binary.LittleEndian.Uint32(h[16:]))
	cdDisk := uint32(binary.LittleEndian.Uint16(h[6:]))
	directoryEnd := g.starts[last] + end.pos
	r := archiveRequestReader{ctx: ctx, input: raw.input}
	if end.hasLocator {
		locatorPos := directoryEnd - zip64LocatorLen
		recordDisk := binary.LittleEndian.Uint32(end.locator[4:])
		recordPos, mapErr := g.address(recordDisk, binary.LittleEndian.Uint64(end.locator[8:]), false)
		if mapErr != nil {
			return nil, 0, mapErr
		}
		record, valid, readErr := readZIP64End(r, raw.size, recordPos, locatorPos)
		if readErr != nil {
			return nil, 0, readErr
		}
		if !valid {
			return nil, 0, zipDiskError("ZIP64 end record is invalid")
		}
		// ZIP64 V2 defines encrypted/compressed central directory metadata.
		if binary.LittleEndian.Uint16(record[14:]) >= 62 {
			return nil, 0, zipDiskUnsupported("ZIP64 encrypted central directory")
		}
		if binary.LittleEndian.Uint64(record[4:]) > uint64(maxBytes) {
			return nil, 0, zipDiskLimit("ZIP64 end record exceeds metadata limit")
		}
		if binary.LittleEndian.Uint32(record[16:]) != recordDisk {
			return nil, 0, zipDiskError("ZIP64 end disk disagrees with locator")
		}
		cdDisk = binary.LittleEndian.Uint32(record[20:])
		entriesOnDisk = binary.LittleEndian.Uint64(record[24:])
		entries = binary.LittleEndian.Uint64(record[32:])
		cdSize = binary.LittleEndian.Uint64(record[40:])
		cdOffset = binary.LittleEndian.Uint64(record[48:])
		if err := zipDiskCheckEndFields(h, record[:]); err != nil {
			return nil, 0, err
		}
		directoryEnd = recordPos
	}
	if originalCount := uint64(binary.LittleEndian.Uint16(h[8:])); originalCount != math.MaxUint16 && originalCount > entries {
		return nil, 0, zipDiskError("end per-disk entry count exceeds total")
	}
	if entriesOnDisk > entries {
		return nil, 0, zipDiskError("per-disk entry count exceeds total")
	}
	if entries > uint64(maxEntries) {
		return nil, 0, zipDiskLimit("too many entries")
	}
	if cdSize > uint64(maxBytes) || cdSize > uint64(math.MaxInt) {
		return nil, 0, zipDiskLimit("central directory exceeds metadata limit")
	}
	cdStart, err := g.address(cdDisk, cdOffset, cdSize == 0)
	if err != nil {
		return nil, 0, err
	}
	if cdStart > directoryEnd || cdSize > uint64(directoryEnd-cdStart) {
		return nil, 0, zipDiskError("central directory overlaps end records")
	}
	const endingSize = zip64EOCDMinLen + zip64LocatorLen + zipEOCDLen
	if maxBytes < endingSize {
		return nil, 0, zipDiskLimit("normalized metadata exceeds limit")
	}
	var normalized []byte
	pos := cdStart
	limit := cdStart + int64(cdSize)
	for i := uint64(0); i < entries; i++ {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		if limit-pos < zipCentralHdrLen {
			return nil, 0, zipDiskError("central directory is truncated")
		}
		var header [zipCentralHdrLen]byte
		if err := zipReadAt(r, header[:], pos); err != nil {
			return nil, 0, err
		}
		if binary.LittleEndian.Uint32(header[:]) == 0x08064b50 {
			return nil, 0, zipDiskUnsupported("archive extra data or encrypted central directory")
		}
		if binary.LittleEndian.Uint32(header[:]) != zipCentralSignature {
			return nil, 0, zipDiskError("central directory record signature is invalid")
		}
		nameLen := int(binary.LittleEndian.Uint16(header[28:]))
		extraLen := int(binary.LittleEndian.Uint16(header[30:]))
		commentLen := int(binary.LittleEndian.Uint16(header[32:]))
		variableLen := nameLen + extraLen + commentLen
		if int64(variableLen) > limit-pos-zipCentralHdrLen {
			return nil, 0, zipDiskError("central directory record is truncated")
		}
		variable := make([]byte, variableLen)
		if err := zipReadAt(r, variable, pos+zipCentralHdrLen); err != nil {
			return nil, 0, err
		}
		pos += zipCentralHdrLen + int64(variableLen)
		record, err := normalizeZIPDiskRecord(header, variable, nameLen, extraLen, g, min(maxBytes-endingSize-int64(len(normalized)), int64(math.MaxInt)-endingSize-int64(len(normalized))))
		if err != nil {
			return nil, 0, err
		}
		if int64(len(record)) > maxBytes-endingSize-int64(len(normalized)) {
			return nil, 0, zipDiskLimit("normalized metadata exceeds limit")
		}
		normalized = append(normalized, record...)
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if pos != limit {
		return nil, 0, zipDiskError("central directory size disagrees with entries")
	}
	if int64(len(normalized))+endingSize > math.MaxInt64-raw.size {
		return nil, 0, zipDiskLimit("normalized archive size overflows")
	}
	directorySize := len(normalized)
	var ending [endingSize]byte
	binary.LittleEndian.PutUint32(ending[:], zip64EOCDSignature)
	binary.LittleEndian.PutUint64(ending[4:], 44)
	binary.LittleEndian.PutUint16(ending[12:], 45)
	binary.LittleEndian.PutUint16(ending[14:], 45)
	binary.LittleEndian.PutUint64(ending[24:], entries)
	binary.LittleEndian.PutUint64(ending[32:], entries)
	binary.LittleEndian.PutUint64(ending[40:], uint64(directorySize))
	binary.LittleEndian.PutUint64(ending[48:], uint64(raw.size))
	locator := ending[zip64EOCDMinLen:]
	binary.LittleEndian.PutUint32(locator, zip64LocatorSignature)
	binary.LittleEndian.PutUint64(locator[8:], uint64(raw.size)+uint64(directorySize))
	binary.LittleEndian.PutUint32(locator[16:], 1)
	eocd := ending[zip64EOCDMinLen+zip64LocatorLen:]
	binary.LittleEndian.PutUint32(eocd, zipEOCDSignature)
	binary.LittleEndian.PutUint16(eocd[8:], math.MaxUint16)
	binary.LittleEndian.PutUint16(eocd[10:], math.MaxUint16)
	binary.LittleEndian.PutUint32(eocd[12:], math.MaxUint32)
	binary.LittleEndian.PutUint32(eocd[16:], math.MaxUint32)
	normalized = append(normalized, ending[:]...)
	return &zipIndexAppendInput{raw: raw.input, rawSize: raw.size, tail: normalized}, raw.size + int64(len(normalized)), nil
}

func readZIPVolumeHintFromEnd(end zipDiskEnd) (zipVolumeHint, error) {
	disk := uint32(binary.LittleEndian.Uint16(end.header[4:]))
	central := uint32(binary.LittleEndian.Uint16(end.header[6:]))
	if end.hasLocator {
		count := binary.LittleEndian.Uint32(end.locator[16:])
		recordDisk := binary.LittleEndian.Uint32(end.locator[4:])
		if count == 0 || recordDisk >= count || (disk != math.MaxUint16 && disk != count-1) {
			return zipVolumeHint{}, zipDiskError("ZIP64 locator disk count is invalid")
		}
		disk = count - 1
	}
	if central != math.MaxUint16 && central > disk {
		return zipVolumeHint{}, zipDiskError("central directory disk exceeds final disk")
	}
	kind := archiveSingle
	if disk != 0 || (central != 0 && central != math.MaxUint16) {
		kind = archiveZIPDisks
	}
	return zipVolumeHint{kind: kind, lastDisk: disk, centralDisk: central}, nil
}

func zipDiskCheckEndFields(h, end []byte) error {
	// Non-sentinel ZIP32 fields must agree with ZIP64. Disk numbers describe
	// their respective end record, so only the central disk is shared here.
	pairs := [][3]uint64{
		{uint64(binary.LittleEndian.Uint16(h[6:])), uint64(binary.LittleEndian.Uint32(end[20:])), math.MaxUint16},
		{uint64(binary.LittleEndian.Uint16(h[10:])), binary.LittleEndian.Uint64(end[32:]), math.MaxUint16},
		{uint64(binary.LittleEndian.Uint32(h[12:])), binary.LittleEndian.Uint64(end[40:]), math.MaxUint32},
		{uint64(binary.LittleEndian.Uint32(h[16:])), binary.LittleEndian.Uint64(end[48:]), math.MaxUint32},
	}
	for _, p := range pairs {
		if p[0] != p[2] && p[0] != p[1] {
			return zipDiskError("ZIP32 and ZIP64 end fields disagree")
		}
	}
	return nil
}

func normalizeZIPDiskRecord(h [zipCentralHdrLen]byte, variable []byte, nameLen, extraLen int, g zipDiskGeometry, maxRecordBytes int64) ([]byte, error) {
	if binary.LittleEndian.Uint16(h[8:])&((1<<6)|(1<<13)) != 0 {
		return nil, zipDiskUnsupported("strong encryption or masked central directory")
	}
	needU := binary.LittleEndian.Uint32(h[24:]) == math.MaxUint32
	needC := binary.LittleEndian.Uint32(h[20:]) == math.MaxUint32
	needO := binary.LittleEndian.Uint32(h[42:]) == math.MaxUint32
	needD := binary.LittleEndian.Uint16(h[34:]) == math.MaxUint16
	local := uint64(binary.LittleEndian.Uint32(h[42:]))
	disk := uint32(binary.LittleEndian.Uint16(h[34:]))
	var u, c uint64
	var zip64Data []byte
	found := false
	extra := variable[nameLen : nameLen+extraLen]
	preserved := make([]byte, 0, len(extra))
	for len(extra) > 0 {
		if len(extra) < 4 {
			return nil, zipDiskError("extra field header is truncated")
		}
		n := int(binary.LittleEndian.Uint16(extra[2:]))
		if n > len(extra)-4 {
			return nil, zipDiskError("extra field data is truncated")
		}
		field := extra[:4+n]
		extra = extra[4+n:]
		if binary.LittleEndian.Uint16(field) != 1 {
			preserved = append(preserved, field...)
			continue
		}
		if found {
			return nil, zipDiskError("duplicate ZIP64 extra field")
		}
		found = true
		zip64Data = field[4:]
	}
	if (needU || needC || needO || needD) && !found {
		return nil, zipDiskError("required ZIP64 extra field is missing")
	}
	take64 := func() (uint64, error) {
		if len(zip64Data) < 8 {
			return 0, zipDiskError("ZIP64 extra field is truncated")
		}
		v := binary.LittleEndian.Uint64(zip64Data)
		zip64Data = zip64Data[8:]
		return v, nil
	}
	var err error
	if needU {
		u, err = take64()
		if err != nil {
			return nil, err
		}
	}
	if needC {
		c, err = take64()
		if err != nil {
			return nil, err
		}
	}
	if needO {
		local, err = take64()
		if err != nil {
			return nil, err
		}
	}
	if needD {
		if len(zip64Data) < 4 {
			return nil, zipDiskError("ZIP64 disk number is missing")
		}
		disk = binary.LittleEndian.Uint32(zip64Data)
		zip64Data = zip64Data[4:]
	}
	// APPNOTE 4.5.3 forbids fields whose corresponding header is not sentinel.
	if len(zip64Data) != 0 {
		return nil, zipDiskError("ZIP64 extra field has ambiguous conditional fields")
	}
	global, err := g.address(disk, local, false)
	if err != nil {
		return nil, err
	}
	needGlobal64 := uint64(global) >= math.MaxUint32
	var rebuilt [28]byte
	n := 4
	if needU {
		binary.LittleEndian.PutUint64(rebuilt[n:], u)
		n += 8
	}
	if needC {
		binary.LittleEndian.PutUint64(rebuilt[n:], c)
		n += 8
	}
	if needGlobal64 {
		binary.LittleEndian.PutUint64(rebuilt[n:], uint64(global))
		n += 8
		binary.LittleEndian.PutUint32(h[42:], math.MaxUint32)
	} else {
		binary.LittleEndian.PutUint32(h[42:], uint32(global))
	}
	binary.LittleEndian.PutUint16(h[34:], 0)
	if n > 4 {
		binary.LittleEndian.PutUint16(rebuilt[:], 1)
		binary.LittleEndian.PutUint16(rebuilt[2:], uint16(n-4))
		preserved = append(preserved, rebuilt[:n]...)
		if binary.LittleEndian.Uint16(h[6:]) < 45 {
			binary.LittleEndian.PutUint16(h[6:], 45)
		}
	}
	if len(preserved) > math.MaxUint16 {
		return nil, zipDiskLimit("promoted extra field is too large")
	}
	binary.LittleEndian.PutUint16(h[30:], uint16(len(preserved)))
	recordLen := zipCentralHdrLen + len(preserved) + len(variable) - extraLen
	if int64(recordLen) > maxRecordBytes {
		return nil, zipDiskLimit("normalized metadata exceeds limit")
	}
	record := make([]byte, 0, recordLen)
	record = append(record, h[:]...)
	record = append(record, variable[:nameLen]...)
	record = append(record, preserved...)
	record = append(record, variable[nameLen+extraLen:]...)
	return record, nil
}
