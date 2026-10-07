package mountfs

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

type cancelAfterFirstRead struct {
	reader *bytes.Reader
	reads  int
}

func (r *cancelAfterFirstRead) ReadAt(p []byte, off int64) (int, error) {
	r.reads++
	if r.reads > 1 {
		return 0, context.Canceled
	}
	return r.reader.ReadAt(p, off)
}

func makeZIPFixture(t *testing.T, names ...string) []byte {
	t.Helper()
	var out bytes.Buffer
	w := zip.NewWriter(&out)
	for _, name := range names {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = io.WriteString(f, "x"); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func eocdOffset(data []byte) int {
	for i := len(data) - zipEOCDLen; i >= 0; i-- {
		if binary.LittleEndian.Uint32(data[i:]) == zipEOCDSignature {
			return i
		}
	}
	return -1
}

func makeZIP64Fixture(t *testing.T) []byte {
	t.Helper()
	classic := makeZIPFixture(t, "one")
	eocd := eocdOffset(classic)
	if eocd < 0 {
		t.Fatal("fixture has no EOCD")
	}
	entries := binary.LittleEndian.Uint16(classic[eocd+10:])
	cdSize := binary.LittleEndian.Uint32(classic[eocd+12:])
	cdOffset := binary.LittleEndian.Uint32(classic[eocd+16:])
	zip64Offset := uint64(eocd)
	data := make([]byte, 0, len(classic)+zip64EOCDMinLen+zip64LocatorLen)
	data = append(data, classic[:eocd]...)
	var end [zip64EOCDMinLen]byte
	binary.LittleEndian.PutUint32(end[0:], zip64EOCDSignature)
	binary.LittleEndian.PutUint64(end[4:], 44)
	binary.LittleEndian.PutUint16(end[12:], 45)
	binary.LittleEndian.PutUint16(end[14:], 45)
	binary.LittleEndian.PutUint64(end[24:], uint64(entries))
	binary.LittleEndian.PutUint64(end[32:], uint64(entries))
	binary.LittleEndian.PutUint64(end[40:], uint64(cdSize))
	binary.LittleEndian.PutUint64(end[48:], uint64(cdOffset))
	data = append(data, end[:]...)
	var locator [zip64LocatorLen]byte
	binary.LittleEndian.PutUint32(locator[0:], zip64LocatorSignature)
	binary.LittleEndian.PutUint64(locator[8:], zip64Offset)
	binary.LittleEndian.PutUint32(locator[16:], 1)
	data = append(data, locator[:]...)
	var classicEnd [zipEOCDLen]byte
	binary.LittleEndian.PutUint32(classicEnd[0:], zipEOCDSignature)
	binary.LittleEndian.PutUint16(classicEnd[8:], 0xffff)
	binary.LittleEndian.PutUint16(classicEnd[10:], 0xffff)
	binary.LittleEndian.PutUint32(classicEnd[12:], 0xffffffff)
	binary.LittleEndian.PutUint32(classicEnd[16:], 0xffffffff)
	data = append(data, classicEnd[:]...)
	return data
}

func TestPreflightZIPAcceptsZIPAndZIP64(t *testing.T) {
	for name, data := range map[string][]byte{
		"zip":   makeZIPFixture(t, "one", "two"),
		"zip64": makeZIP64Fixture(t),
	} {
		t.Run(name, func(t *testing.T) {
			if err := preflightZIP(bytes.NewReader(data), int64(len(data)), 10, 1024); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPreflightZIPBoundsEntriesAndIndexBytes(t *testing.T) {
	data := makeZIPFixture(t, "one", "two", "three", "four")
	if err := preflightZIP(bytes.NewReader(data), int64(len(data)), 3, 1024); err == nil {
		t.Fatal("expected max entry rejection")
	}

	declaredCount := append([]byte(nil), data...)
	eocd := eocdOffset(declaredCount)
	binary.LittleEndian.PutUint16(declaredCount[eocd+8:], 0xffff)
	binary.LittleEndian.PutUint16(declaredCount[eocd+10:], 0xffff)
	if err := preflightZIP(bytes.NewReader(declaredCount), int64(len(declaredCount)), 100, 1024); err == nil {
		t.Fatal("expected oversized declared count rejection")
	}

	tooMuchIndex := append([]byte(nil), data...)
	eocd = eocdOffset(tooMuchIndex)
	binary.LittleEndian.PutUint32(tooMuchIndex[eocd+12:], 64<<20)
	if err := preflightZIP(bytes.NewReader(tooMuchIndex), int64(len(tooMuchIndex)), 100, 1<<20); err == nil {
		t.Fatal("expected oversized directory rejection")
	}

	underdeclared := append([]byte(nil), data...)
	eocd = eocdOffset(underdeclared)
	binary.LittleEndian.PutUint16(underdeclared[eocd+8:], 1)
	binary.LittleEndian.PutUint16(underdeclared[eocd+10:], 1)
	if err := preflightZIP(bytes.NewReader(underdeclared), int64(len(underdeclared)), 100, 1024); err == nil {
		t.Fatal("expected actual entry count mismatch")
	}
}

func TestPreflightZIPAcceptsSelfExtractingPrefixAndRejectsTruncation(t *testing.T) {
	for name, original := range map[string][]byte{
		"zip":   makeZIPFixture(t, "one"),
		"zip64": makeZIP64Fixture(t),
	} {
		t.Run(name, func(t *testing.T) {
			prefix := []byte("MZ self extracting prefix")
			data := append(append([]byte(nil), prefix...), original...)
			if err := preflightZIP(bytes.NewReader(data), int64(len(data)), 10, 1024); err != nil {
				t.Fatal(err)
			}
			truncated := data[:len(data)-8]
			if err := preflightZIP(bytes.NewReader(truncated), int64(len(truncated)), 10, 1024); err == nil {
				t.Fatal("expected truncation rejection")
			}
		})
	}
}

func TestPreflightZIPPreservesContextCancellation(t *testing.T) {
	data := makeZIPFixture(t, "one")
	r := &cancelAfterFirstRead{reader: bytes.NewReader(data)}
	if err := preflightZIP(r, int64(len(data)), 10, 1024); !errors.Is(err, context.Canceled) {
		t.Fatalf("preflight error = %v, want context.Canceled", err)
	}
}
