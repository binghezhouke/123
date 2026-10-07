package mountfs

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
)

const (
	zipEOCDLen       = 22
	zipEOCDMaxTail   = zipEOCDLen + 65535
	zip64LocatorLen  = 20
	zip64EOCDMinLen  = 56
	zipCentralHdrLen = 46

	zipEOCDSignature      = 0x06054b50
	zip64LocatorSignature = 0x07064b50
	zip64EOCDSignature    = 0x06064b50
	zipCentralSignature   = 0x02014b50
)

// preflightZIP validates and bounds the ZIP central directory before
// archive/zip.NewReader can allocate its file index. maxIndexBytes measures
// the encoded central-directory records, including their fixed headers.
func preflightZIP(r io.ReaderAt, size int64, maxEntries int, maxIndexBytes int64) error {
	if r == nil || size < 0 || maxEntries < 0 || maxIndexBytes < 0 {
		return errors.New("invalid ZIP preflight limits")
	}
	if size < zipEOCDLen {
		return errors.New("ZIP end record is missing")
	}
	tailLen := int64(zipEOCDMaxTail)
	if size < tailLen {
		tailLen = size
	}
	tail := make([]byte, int(tailLen))
	if err := zipReadAt(r, tail, size-tailLen); err != nil {
		return zipPreflightReadError(err, "ZIP end record is truncated")
	}

	var eocdPos int64 = -1
	var eocd []byte
	for i := len(tail) - zipEOCDLen; i >= 0; i-- {
		if binary.LittleEndian.Uint32(tail[i:]) != zipEOCDSignature {
			continue
		}
		commentLen := int(binary.LittleEndian.Uint16(tail[i+20:]))
		if i+zipEOCDLen+commentLen > len(tail) {
			continue
		}
		eocdPos = size - tailLen + int64(i)
		eocd = tail[i : i+zipEOCDLen]
		break
	}
	if eocdPos < 0 {
		return errors.New("ZIP end record is missing or malformed")
	}

	disk := binary.LittleEndian.Uint16(eocd[4:])
	cdDisk := binary.LittleEndian.Uint16(eocd[6:])
	entriesOnDisk := uint64(binary.LittleEndian.Uint16(eocd[8:]))
	entries := uint64(binary.LittleEndian.Uint16(eocd[10:]))
	cdSize := uint64(binary.LittleEndian.Uint32(eocd[12:]))
	cdOffset := uint64(binary.LittleEndian.Uint32(eocd[16:]))
	zip64 := entriesOnDisk == 0xffff || entries == 0xffff || cdSize == 0xffffffff || cdOffset == 0xffffffff
	var directoryEnd int64
	if zip64 {
		if eocdPos < zip64LocatorLen {
			return errors.New("ZIP64 locator is missing")
		}
		var locator [zip64LocatorLen]byte
		if err := zipReadAt(r, locator[:], eocdPos-zip64LocatorLen); err != nil {
			return zipPreflightReadError(err, "ZIP64 locator is truncated")
		}
		if binary.LittleEndian.Uint32(locator[:]) != zip64LocatorSignature ||
			binary.LittleEndian.Uint32(locator[4:]) != 0 || binary.LittleEndian.Uint32(locator[16:]) != 1 {
			return errors.New("ZIP64 locator is invalid")
		}
		zip64Offset := binary.LittleEndian.Uint64(locator[8:])
		locatorOffset := eocdPos - zip64LocatorLen
		end, valid, readErr := readZIP64End(r, size, int64(zip64Offset), locatorOffset)
		if readErr != nil && (errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded)) {
			return zipPreflightReadError(readErr, "ZIP64 end record is truncated")
		}
		if !valid {
			// Self-extracting archives sometimes leave this locator offset
			// relative to the ZIP payload. The common fixed-size ZIP64 record
			// still has an unambiguous position immediately before the locator.
			fallback := locatorOffset - zip64EOCDMinLen
			var fallbackErr error
			end, valid, fallbackErr = readZIP64End(r, size, fallback, locatorOffset)
			if fallbackErr != nil && (errors.Is(fallbackErr, context.Canceled) || errors.Is(fallbackErr, context.DeadlineExceeded)) {
				return zipPreflightReadError(fallbackErr, "ZIP64 end record is truncated")
			}
			if !valid {
				return errors.New("ZIP64 end record is missing or invalid")
			}
			zip64Offset = uint64(fallback)
		}
		if binary.LittleEndian.Uint32(end[16:]) != 0 || binary.LittleEndian.Uint32(end[20:]) != 0 {
			return errors.New("multi-disk ZIP archives are unsupported")
		}
		entriesOnDisk = binary.LittleEndian.Uint64(end[24:])
		entries = binary.LittleEndian.Uint64(end[32:])
		cdSize = binary.LittleEndian.Uint64(end[40:])
		cdOffset = binary.LittleEndian.Uint64(end[48:])
		directoryEnd = int64(zip64Offset)
	} else {
		if disk != 0 || cdDisk != 0 || entriesOnDisk != entries {
			return errors.New("multi-disk ZIP archives are unsupported")
		}
		directoryEnd = eocdPos
	}
	if entriesOnDisk != entries {
		return errors.New("ZIP entry counts do not match")
	}
	if entries > uint64(maxEntries) {
		return errors.New("ZIP contains too many entries")
	}
	if cdSize > uint64(maxIndexBytes) {
		return errors.New("ZIP index exceeds configured size limit")
	}
	if cdOffset > uint64(^uint64(0)>>1) || cdSize > uint64(^uint64(0)>>1) {
		return errors.New("ZIP directory is out of bounds")
	}
	// A normal archive places its directory directly before the end record.
	// Their difference also identifies the prefix length in self-extracting ZIPs.
	if uint64(directoryEnd) < cdOffset || uint64(directoryEnd)-cdOffset < cdSize {
		return errors.New("ZIP directory is out of bounds")
	}
	baseOffset := uint64(directoryEnd) - cdOffset - cdSize
	if baseOffset > uint64(size) || cdOffset > uint64(size)-baseOffset || cdSize > uint64(size)-baseOffset-cdOffset {
		return errors.New("ZIP directory is out of bounds")
	}
	directoryStart := int64(baseOffset + cdOffset)
	directoryLimit := directoryStart + int64(cdSize)
	if directoryLimit > directoryEnd {
		return errors.New("ZIP directory overlaps its end record")
	}
	return scanZIPDirectory(r, directoryStart, directoryLimit, entries, maxEntries, maxIndexBytes)
}

func readZIP64End(r io.ReaderAt, size int64, offset, locatorOffset int64) ([zip64EOCDMinLen]byte, bool, error) {
	var end [zip64EOCDMinLen]byte
	if offset < 0 || offset > size-zip64EOCDMinLen || offset > locatorOffset-12 {
		return end, false, nil
	}
	if err := zipReadAt(r, end[:], offset); err != nil {
		return end, false, err
	}
	if binary.LittleEndian.Uint32(end[:]) != zip64EOCDSignature {
		return end, false, nil
	}
	recordSize := binary.LittleEndian.Uint64(end[4:])
	if recordSize < 44 || recordSize > uint64(size-offset-12) || recordSize > uint64(locatorOffset-offset-12) {
		return end, false, nil
	}
	return end, true, nil
}

func scanZIPDirectory(r io.ReaderAt, start, limit int64, entries uint64, maxEntries int, maxBytes int64) error {
	reader := bufio.NewReaderSize(io.NewSectionReader(r, start, limit-start), 64<<10)
	var indexedBytes int64
	for count := 0; count < maxEntries && uint64(count) < entries; count++ {
		if limit-start-indexedBytes < zipCentralHdrLen {
			return errors.New("ZIP central directory is truncated")
		}
		var header [zipCentralHdrLen]byte
		if _, err := io.ReadFull(reader, header[:]); err != nil {
			return zipPreflightReadError(err, "ZIP central directory is truncated")
		}
		if binary.LittleEndian.Uint32(header[:]) != zipCentralSignature {
			return errors.New("ZIP central directory record is invalid")
		}
		nameLen := int64(binary.LittleEndian.Uint16(header[28:]))
		extraLen := int64(binary.LittleEndian.Uint16(header[30:]))
		commentLen := int64(binary.LittleEndian.Uint16(header[32:]))
		recordLen := int64(zipCentralHdrLen) + nameLen + extraLen + commentLen
		if recordLen > limit-start-indexedBytes {
			return errors.New("ZIP central directory record is truncated")
		}
		if recordLen > maxBytes-indexedBytes {
			return errors.New("ZIP index exceeds configured size limit")
		}
		if _, err := io.CopyN(io.Discard, reader, recordLen-zipCentralHdrLen); err != nil {
			return zipPreflightReadError(err, "ZIP central directory record is truncated")
		}
		indexedBytes += recordLen
	}
	if uint64(maxEntries) < entries {
		return errors.New("ZIP contains too many entries")
	}
	if start+indexedBytes != limit {
		return errors.New("ZIP central directory size does not match its entries")
	}
	return nil
}

func zipReadAt(r io.ReaderAt, p []byte, off int64) error {
	n, err := r.ReadAt(p, off)
	if n == len(p) {
		return nil
	}
	if err != nil {
		return err
	}
	return io.ErrUnexpectedEOF
}

func zipPreflightReadError(err error, fallback string) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return errors.New(fallback)
}
