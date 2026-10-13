package rardecode

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Both fixture encoders use the documented stored-file layout and independent
// payload bytes. Native OpenMember remains the sequential checksum oracle.
func storedRangeFixture(t *testing.T, version int, payload []byte, crc uint32, hashExtra bool) ([]byte, MemberLocator, int64) {
	t.Helper()
	name := []byte("payload.bin")
	var data []byte
	var headerOffset, bodyOffset int64
	if version == 3 {
		block := func(kind byte, flags uint16, fields []byte) []byte {
			h := append([]byte{kind}, binary.LittleEndian.AppendUint16(nil, flags)...)
			h = binary.LittleEndian.AppendUint16(h, uint16(len(fields)+7))
			h = append(h, fields...)
			return append(binary.LittleEndian.AppendUint16(nil, uint16(crc32.ChecksumIEEE(h))), h...)
		}
		data = append([]byte("Rar!\x1a\x07\x00"), block(0x73, 0, make([]byte, 6))...)
		headerOffset = int64(len(data))
		fields := binary.LittleEndian.AppendUint32(nil, uint32(len(payload)))
		fields = binary.LittleEndian.AppendUint32(fields, uint32(len(payload)))
		fields = append(fields, 2)
		fields = binary.LittleEndian.AppendUint32(fields, crc)
		fields = binary.LittleEndian.AppendUint32(fields, 0)
		fields = append(fields, 20, 0x30)
		fields = binary.LittleEndian.AppendUint16(fields, uint16(len(name)))
		fields = binary.LittleEndian.AppendUint32(fields, 0x20)
		fields = append(fields, name...)
		data = append(data, block(0x74, 0x8000, fields)...)
		bodyOffset = int64(len(data))
		data = append(data, payload...)
		data = append(data, block(0x7b, 0, nil)...)
	} else {
		block := func(fields []byte) []byte {
			body := binary.AppendUvarint(nil, uint64(len(fields)))
			body = append(body, fields...)
			return append(binary.LittleEndian.AppendUint32(nil, crc32.ChecksumIEEE(body)), body...)
		}
		data = append([]byte("Rar!\x1a\x07\x01\x00"), block([]byte{1, 0, 0})...)
		headerOffset = int64(len(data))
		extra := []byte(nil)
		flags := uint64(2)
		if hashExtra {
			// RAR5 hash type 0 declares BLAKE2sp; the current parser does not
			// implement it, so a new direct path must conservatively decline it.
			record := append([]byte{2, 0}, make([]byte, 32)...)
			extra = append(binary.AppendUvarint(nil, uint64(len(record))), record...)
			flags |= 1
		}
		fields := binary.AppendUvarint(nil, 2)
		fields = binary.AppendUvarint(fields, flags)
		if len(extra) != 0 {
			fields = binary.AppendUvarint(fields, uint64(len(extra)))
		}
		fields = binary.AppendUvarint(fields, uint64(len(payload)))
		fields = binary.AppendUvarint(fields, 4)
		fields = binary.AppendUvarint(fields, uint64(len(payload)))
		fields = binary.AppendUvarint(fields, 0x20)
		fields = binary.LittleEndian.AppendUint32(fields, crc)
		fields = append(fields, 0, 0)
		fields = binary.AppendUvarint(fields, uint64(len(name)))
		fields = append(fields, name...)
		fields = append(fields, extra...)
		data = append(data, block(fields)...)
		bodyOffset = int64(len(data))
		data = append(data, payload...)
		data = append(data, block([]byte{5, 0, 0})...)
	}
	return data, MemberLocator{offset: headerOffset, valid: true}, bodyOffset
}

func openStoredRangeFixture(t *testing.T, data []byte, locator MemberLocator) *StoredMemberRange {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fixture.rar"), data, 0600); err != nil {
		t.Fatal(err)
	}
	h, plan, err := OpenStoredMemberRange("fixture.rar", locator, FileSystem(os.DirFS(dir)), BufferSize(64))
	if err != nil {
		t.Fatalf("open stored range: %v", err)
	}
	if h.Name != "payload.bin" || h.Encrypted || h.Solid {
		t.Fatalf("header: %#v", h)
	}
	t.Cleanup(func() { _ = plan.Close() })
	return plan
}

type storedRecordingReader struct {
	reader       io.ReaderAt
	bytes, calls int
	offset       int64
}

func (r *storedRecordingReader) ReadAt(p []byte, off int64) (int, error) {
	r.calls++
	r.offset = off
	n, err := r.reader.ReadAt(p, off)
	r.bytes += n
	return n, err
}

func TestStoredMemberRangeSparseReadDoesNotConsumePrefix(t *testing.T) {
	payload := bytes.Repeat([]byte("stored payload "), (8<<20)/15+1)
	for _, version := range []int{3, 5} {
		t.Run(string(rune('0'+version)), func(t *testing.T) {
			data, locator, bodyOffset := storedRangeFixture(t, version, payload, crc32.ChecksumIEEE(payload), false)
			plan := openStoredRangeFixture(t, data, locator)
			source := &storedRecordingReader{reader: bytes.NewReader(data)}
			got := make([]byte, 4096)
			off := int64(len(payload) - len(got))
			if n, err := plan.ReadAtWithReader(source, got, off); n != len(got) || err != nil || !bytes.Equal(got, payload[off:]) {
				t.Fatalf("tail n=%d,err=%v", n, err)
			}
			if source.bytes != len(got) || source.calls != 1 || source.offset != bodyOffset+off {
				t.Fatalf("source read bytes=%d,calls=%d,offset=%d", source.bytes, source.calls, source.offset)
			}
		})
	}
}

func TestStoredMemberRangeFullReadsRetainDeclaredCRC(t *testing.T) {
	for _, version := range []int{3, 5} {
		for _, payload := range [][]byte{[]byte("checked payload"), {0x9d, 0x0a, 0xd9, 0x6d}} {
			data, locator, bodyOffset := storedRangeFixture(t, version, payload, crc32.ChecksumIEEE(payload), false)
			plan := openStoredRangeFixture(t, data, locator)
			if n, err := plan.ReadAtWithReader(bytes.NewReader(data), make([]byte, len(payload)), 0); n != len(payload) || err != nil {
				t.Fatalf("valid full n=%d,err=%v", n, err)
			}
			bad := append([]byte(nil), data...)
			bad[bodyOffset] ^= 1
			if n, err := plan.ReadAtWithReader(bytes.NewReader(bad), make([]byte, len(payload)), 0); n != len(payload) || !errors.Is(err, ErrBadFileChecksum) {
				t.Fatalf("bad full n=%d,err=%v", n, err)
			}
		}
	}
}

type storedFullEOFReader struct{ io.ReaderAt }

func (r storedFullEOFReader) ReadAt(p []byte, off int64) (int, error) {
	n, err := r.ReaderAt.ReadAt(p, off)
	if n == len(p) {
		err = io.EOF
	}
	return n, err
}

func TestStoredMemberRangeFullEOFStillChecksCRC(t *testing.T) {
	payload := []byte("complete bytes plus EOF")
	data, locator, bodyOffset := storedRangeFixture(t, 5, payload, crc32.ChecksumIEEE(payload), false)
	plan := openStoredRangeFixture(t, data, locator)
	if n, err := plan.ReadAtWithReader(storedFullEOFReader{bytes.NewReader(data)}, make([]byte, len(payload)), 0); n != len(payload) || !errors.Is(err, io.EOF) {
		t.Fatalf("valid full EOF n=%d,err=%v", n, err)
	}
	bad := append([]byte(nil), data...)
	bad[bodyOffset] ^= 1
	if n, err := plan.ReadAtWithReader(storedFullEOFReader{bytes.NewReader(bad)}, make([]byte, len(payload)), 0); n != len(payload) || !errors.Is(err, ErrBadFileChecksum) {
		t.Fatalf("corrupt full EOF n=%d,err=%v,want checksum error", n, err)
	}
}

func TestStoredMemberRangeEmptyCRC(t *testing.T) {
	for _, crc := range []uint32{0, 1} {
		data, locator, _ := storedRangeFixture(t, 3, nil, crc, false)
		plan := openStoredRangeFixture(t, data, locator)
		if n, err := plan.ReadAtWithReader(bytes.NewReader(data), nil, 0); n != 0 || err != nil {
			t.Fatalf("empty buffer n=%d,err=%v", n, err)
		}
		_, err := plan.ReadAtWithReader(bytes.NewReader(data), make([]byte, 1), 0)
		if crc == 0 && !errors.Is(err, io.EOF) {
			t.Fatalf("valid empty CRC=%v", err)
		}
		if crc != 0 && !errors.Is(err, ErrBadFileChecksum) {
			t.Fatalf("corrupt empty CRC=%v", err)
		}
	}
}

func TestStoredMemberRangeContiguousCRCAndSparseRestart(t *testing.T) {
	payload := []byte("contiguous checksum data")
	data, locator, offset := storedRangeFixture(t, 5, payload, crc32.ChecksumIEEE(payload), false)
	bad := append([]byte(nil), data...)
	bad[offset+int64(len(payload))-1] ^= 1
	for _, source := range [][]byte{data, bad} {
		plan := openStoredRangeFixture(t, data, locator)
		reader := bytes.NewReader(source)
		if _, err := plan.ReadAtWithReader(reader, make([]byte, 4), 0); err != nil {
			t.Fatal(err)
		}
		// Overlap invalidates combined validation: this is not a complete pass.
		if _, err := plan.ReadAtWithReader(reader, make([]byte, 3), 2); err != nil {
			t.Fatal(err)
		}
		if _, err := plan.ReadAtWithReader(reader, make([]byte, len(payload)-5), 5); err != nil {
			t.Fatalf("sparse partial must not claim full CRC: %v", err)
		}
		// Starting at zero resets the pass after sparse or reversed reads.
		if _, err := plan.ReadAtWithReader(reader, make([]byte, 7), 0); err != nil {
			t.Fatal(err)
		}
		_, err := plan.ReadAtWithReader(storedFullEOFReader{reader}, make([]byte, len(payload)-7), 7)
		if bytes.Equal(source, data) && !errors.Is(err, io.EOF) {
			t.Fatalf("valid contiguous EOF=%v", err)
		}
		if !bytes.Equal(source, data) && !errors.Is(err, ErrBadFileChecksum) {
			t.Fatalf("bad contiguous CRC=%v", err)
		}
	}
}

type storedFaultReader struct {
	io.ReaderAt
	fault error
	short bool
}

func (r storedFaultReader) ReadAt(p []byte, off int64) (int, error) {
	if r.short {
		p = p[:len(p)-1]
	}
	n, _ := r.ReaderAt.ReadAt(p, off)
	return n, r.fault
}

func TestStoredMemberRangeSourceErrorsAndBounds(t *testing.T) {
	payload := []byte("source faults beat CRC")
	data, locator, offset := storedRangeFixture(t, 3, payload, crc32.ChecksumIEEE(payload), false)
	bad := append([]byte(nil), data...)
	bad[offset] ^= 1
	plan := openStoredRangeFixture(t, data, locator)
	fault := errors.New("synthetic source fault")
	if n, err := plan.ReadAtWithReader(storedFaultReader{ReaderAt: bytes.NewReader(bad), fault: fault}, make([]byte, len(payload)), 0); n != len(payload) || !errors.Is(err, fault) {
		t.Fatalf("full source fault n=%d,err=%v", n, err)
	}
	for _, sourceErr := range []error{nil, io.EOF} {
		if _, err := plan.ReadAtWithReader(storedFaultReader{ReaderAt: bytes.NewReader(data), fault: sourceErr, short: true}, make([]byte, 4), 0); !errors.Is(err, ErrShortFile) {
			t.Fatalf("short source error=%v", err)
		}
	}
	if _, err := plan.ReadAtWithReader(nil, make([]byte, 1), 0); !errors.Is(err, fs.ErrInvalid) {
		t.Fatalf("nil source=%v", err)
	}
	if _, err := plan.ReadAtWithReader(bytes.NewReader(data), make([]byte, 1), -1); !errors.Is(err, fs.ErrInvalid) {
		t.Fatalf("negative offset=%v", err)
	}
	source := &storedRecordingReader{reader: bytes.NewReader(data)}
	if n, err := plan.ReadAtWithReader(source, make([]byte, 1), math.MaxInt64); n != 0 || !errors.Is(err, io.EOF) || source.calls != 0 {
		t.Fatalf("overflow/end n=%d,err=%v,calls=%d", n, err, source.calls)
	}
	got := make([]byte, len(payload)+1)
	if n, err := plan.ReadAtWithReader(source, got, 0); n != len(payload) || !errors.Is(err, io.EOF) || !bytes.Equal(got[:n], payload) {
		t.Fatalf("oversized request n=%d,err=%v", n, err)
	}
}

type storedBlockingReader struct {
	io.ReaderAt
	started, release chan struct{}
}

func (r storedBlockingReader) ReadAt(p []byte, off int64) (int, error) {
	close(r.started)
	<-r.release
	return r.ReaderAt.ReadAt(p, off)
}

func TestStoredMemberRangeConcurrentPartialDoesNotCombineCRC(t *testing.T) {
	payload := []byte("concurrent CRC")
	data, locator, offset := storedRangeFixture(t, 3, payload, crc32.ChecksumIEEE(payload), false)
	bad := append([]byte(nil), data...)
	bad[offset+int64(len(payload))-1] ^= 1
	plan := openStoredRangeFixture(t, data, locator)
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := plan.ReadAtWithReader(storedBlockingReader{bytes.NewReader(bad), started, release}, make([]byte, 4), 0)
		done <- err
	}()
	<-started
	if _, err := plan.ReadAtWithReader(bytes.NewReader(bad), make([]byte, len(payload)-4), 4); err != nil {
		t.Fatalf("overlapping partial checksum=%v", err)
	}
	// An independent full read still validates while another read is active.
	if _, err := plan.ReadAtWithReader(bytes.NewReader(bad), make([]byte, len(payload)), 0); !errors.Is(err, ErrBadFileChecksum) {
		t.Fatalf("concurrent full CRC=%v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := plan.ReadAtWithReader(bytes.NewReader(bad), make([]byte, 4), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := plan.ReadAtWithReader(bytes.NewReader(bad), make([]byte, len(payload)-4), 4); !errors.Is(err, ErrBadFileChecksum) {
		t.Fatalf("restarted contiguous CRC=%v", err)
	}
}

func TestStoredMemberRangeCloseDoesNotWaitForSource(t *testing.T) {
	payload := []byte("active complete read")
	data, locator, _ := storedRangeFixture(t, 5, payload, crc32.ChecksumIEEE(payload), false)
	plan := openStoredRangeFixture(t, data, locator)
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := plan.ReadAtWithReader(storedBlockingReader{bytes.NewReader(data), started, release}, make([]byte, len(payload)), 0)
		done <- err
	}()
	<-started
	closed := make(chan error, 1)
	go func() { closed <- plan.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("Close waited for source I/O")
	}
	if _, err := plan.ReadAtWithReader(bytes.NewReader(data), make([]byte, 1), 0); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("read after Close=%v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("active read after Close=%v", err)
	}
	if err := plan.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStoredMemberRangeUnsupportedLayoutsFallBack(t *testing.T) {
	payload := []byte("stored bytes")
	for _, layout := range []string{"compressed", "solid-member", "solid-archive", "multivolume-archive", "encrypted", "split", "unknown-size", "size-mismatch", "directory", "symlink"} {
		t.Run(layout, func(t *testing.T) {
			data, locator, _ := storedRangeFixture(t, 3, payload, crc32.ChecksumIEEE(payload), false)
			off := locator.offset
			switch layout {
			case "compressed":
				data[off+25] = 0x33
			case "solid-member":
				binary.LittleEndian.PutUint16(data[off+3:], 0x8010)
			case "solid-archive":
				binary.LittleEndian.PutUint16(data[10:], 8)
			case "multivolume-archive":
				binary.LittleEndian.PutUint16(data[10:], 1)
			case "encrypted":
				binary.LittleEndian.PutUint16(data[off+3:], 0x8004)
			case "split":
				binary.LittleEndian.PutUint16(data[off+3:], 0x8002)
			case "unknown-size":
				binary.LittleEndian.PutUint32(data[off+11:], math.MaxUint32)
			case "size-mismatch":
				binary.LittleEndian.PutUint32(data[off+11:], uint32(len(payload)+1))
			case "directory":
				binary.LittleEndian.PutUint16(data[off+3:], 0x80e0)
			case "symlink":
				data[off+15] = 3
				binary.LittleEndian.PutUint32(data[off+28:], 0xa1ff)
			}
			for _, start := range []int64{7, off} {
				length := int64(binary.LittleEndian.Uint16(data[start+5:]))
				binary.LittleEndian.PutUint16(data[start:], uint16(crc32.ChecksumIEEE(data[start+2:start+length])))
			}
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "fixture.rar"), data, 0600); err != nil {
				t.Fatal(err)
			}
			_, plan, err := OpenStoredMemberRange("fixture.rar", locator, FileSystem(os.DirFS(dir)))
			if plan != nil || !errors.Is(err, ErrUnsupportedStoredRange) {
				t.Fatalf("unsupported layout plan=%v,err=%v", plan, err)
			}
		})
	}
	data, locator, _ := storedRangeFixture(t, 5, payload, crc32.ChecksumIEEE(payload), true)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hash.rar"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, plan, err := OpenStoredMemberRange("hash.rar", locator, FileSystem(os.DirFS(dir))); plan != nil || !errors.Is(err, ErrUnsupportedStoredRange) {
		t.Fatalf("unsupported hash plan=%v,err=%v", plan, err)
	}
}

func TestStoredMemberRangeConstructorBoundsAndNativeCRC(t *testing.T) {
	payload := []byte("native checksum retained")
	for _, version := range []int{3, 5} {
		data, locator, bodyOffset := storedRangeFixture(t, version, payload, crc32.ChecksumIEEE(payload), false)
		dir := t.TempDir()
		path := filepath.Join(dir, "fixture.rar")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		_, native, err := OpenMember("fixture.rar", locator, FileSystem(os.DirFS(dir)))
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(native)
		_ = native.Close()
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("native fixture bytes match=%v,err=%v", bytes.Equal(got, payload), err)
		}
		if _, _, err := OpenStoredMemberRange("fixture.rar", MemberLocator{offset: -1, valid: true}, FileSystem(os.DirFS(dir))); !errors.Is(err, ErrInvalidHeaderOff) {
			t.Fatalf("invalid locator=%v", err)
		}
		if err := os.WriteFile(path, data[:bodyOffset+int64(len(payload))-1], 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := OpenStoredMemberRange("fixture.rar", locator, FileSystem(os.DirFS(dir))); !errors.Is(err, ErrShortFile) {
			t.Fatalf("truncated declared body=%v", err)
		}
		bad := append([]byte(nil), data...)
		bad[bodyOffset] ^= 1
		if err := os.WriteFile(path, bad, 0600); err != nil {
			t.Fatal(err)
		}
		_, native, err = OpenMember("fixture.rar", locator, FileSystem(os.DirFS(dir)))
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.ReadAll(native)
		_ = native.Close()
		if !errors.Is(err, ErrBadFileChecksum) {
			t.Fatalf("native corrupt checksum=%v", err)
		}
	}
}

func TestStoredMemberRangeExistingCompressedAndEncryptedFixturesFallBack(t *testing.T) {
	for _, fixture := range []string{"rar5-crc.rar", "rar5-solid.rar", "rar5-psw.rar", "rar5-hpsw.rar"} {
		t.Run(fixture, func(t *testing.T) {
			path := filepath.Join("..", "..", "..", "tests", "fixtures", "rar", fixture)
			var locator MemberLocator
			stop := errors.New("first fixture member")
			err := WalkMembers(path, func(h *FileHeader, l MemberLocator) error { locator = l; return stop }, Password("password"))
			if !errors.Is(err, stop) {
				t.Fatalf("fixture metadata=%v", err)
			}
			if _, plan, err := OpenStoredMemberRange(path, locator, Password("password")); plan != nil || !errors.Is(err, ErrUnsupportedStoredRange) {
				t.Fatalf("fixture fallback plan=%v,err=%v", plan, err)
			}
		})
	}
}
