package mountfs

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/bodgit/sevenzip"
	"github.com/nwaples/rardecode/v2"
)

func archiveStreamTestMember(t *testing.T, data []byte, format string, ordinal int, password []byte) *member {
	t.Helper()
	var found *member
	if err := scanArchive(context.Background(), format, bytes.NewReader(data), int64(len(data)), password, func(f archiveMember) error {
		if !f.directory && f.ordinal == ordinal {
			found = &member{format: format, name: f.name, ordinal: f.ordinal, size: f.size, crc: f.crc, sevenCRCDefined: f.sevenCRCDefined, rarLocator: f.rarLocator}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if found == nil {
		t.Fatalf("fixture has no %s member ordinal %d", format, ordinal)
	}
	return found
}

func TestArchiveMemberStream7zFixtures(t *testing.T) {
	for _, fixture := range []string{"plain.7z", "password.7z", "headers.7z", "copy.7z", "solid.7z"} {
		t.Run(fixture, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata/encrypted", fixture))
			if err != nil {
				t.Fatal(err)
			}
			password := []byte("mount-test-password")
			if fixture == "plain.7z" {
				password = nil
			}
			// Find the regular payload even if directory headers precede it.
			ordinal := -1
			if err := scanArchive(context.Background(), ".7z", bytes.NewReader(data), int64(len(data)), password, func(f archiveMember) error {
				if f.name == "folder/hello.txt" {
					ordinal = f.ordinal
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			m := archiveStreamTestMember(t, data, ".7z", ordinal, password)
			r, err := openArchiveMemberStream(context.Background(), bytes.NewReader(data), int64(len(data)), m, password)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			got, err := io.ReadAll(r)
			if err != nil {
				t.Fatal(err)
			}
			if want := bytes.Repeat([]byte("mount 7z fixture contents\n"), 200); !bytes.Equal(got, want) {
				t.Fatalf("payload differs: got %d bytes", len(got))
			}
		})
	}
}

func TestArchiveMemberStreamRejectsStale7zMetadata(t *testing.T) {
	data := copy7zFixture([]byte("member contents"))
	base := archiveStreamTestMember(t, data, ".7z", 0, nil)
	for _, mismatch := range []string{"negative-ordinal", "ordinal", "name", "size", "crc"} {
		t.Run(mismatch, func(t *testing.T) {
			m := *base
			switch mismatch {
			case "negative-ordinal":
				m.ordinal = -1
			case "ordinal":
				m.ordinal++
			case "name":
				m.name += "-stale"
			case "size":
				m.size++
			case "crc":
				m.crc++
			}
			if _, err := openArchiveMemberStream(context.Background(), bytes.NewReader(data), int64(len(data)), &m, nil); !errors.Is(err, syscall.ESTALE) {
				t.Fatalf("metadata mismatch: %v", err)
			}
		})
	}
}

func TestArchiveMemberStreamRARFixtures(t *testing.T) {
	for _, fixture := range []string{"rar5-crc.rar", "rar3-solid.rar", "rar5-solid.rar", "rar5-psw.rar", "rar5-hpsw.rar"} {
		t.Run(fixture, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("../../../tests/fixtures/rar", fixture))
			if err != nil {
				t.Fatal(err)
			}
			var password []byte
			if fixture == "rar5-psw.rar" || fixture == "rar5-hpsw.rar" {
				password = []byte("password")
			}
			ordinal := 0
			if fixture == "rar3-solid.rar" || fixture == "rar5-solid.rar" {
				ordinal = 1 // Must decode the preceding member before this one.
			}
			m := archiveStreamTestMember(t, data, ".rar", ordinal, password)
			r, err := openArchiveMemberStream(context.Background(), bytes.NewReader(data), int64(len(data)), m, password)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			got, err := io.ReadAll(r)
			if err != nil {
				t.Fatal(err)
			}
			// Independently walk with the ordinary RAR decoder as the oracle.
			rr, err := rardecode.NewReader(bytes.NewReader(data), rarOptions(password)...)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i <= ordinal; i++ {
				if _, err := rr.Next(); err != nil {
					t.Fatal(err)
				}
			}
			want, err := io.ReadAll(rr)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) || uint64(len(got)) != m.size {
				t.Fatalf("RAR member differs: got %d bytes, want %d", len(got), len(want))
			}
		})
	}
}

type archiveStreamCountedInput struct {
	*bytes.Reader
	readBytes int64
}

func (r *archiveStreamCountedInput) ReadAt(p []byte, off int64) (int, error) {
	n, err := r.Reader.ReadAt(p, off)
	r.readBytes += int64(n)
	return n, err
}

func archiveStreamLargeSolidRAR(t *testing.T, payload []byte) ([]byte, *member) {
	t.Helper()
	data := storedRARImages(payload, 2)
	m := archiveStreamTestMember(t, data, ".rar", 1, nil)
	offset := rardecode.ExportLocator(*m.rarLocator).Offset
	headerSize := int(binary.LittleEndian.Uint16(data[offset+5:]))
	header := data[offset : offset+int64(headerSize)]
	binary.LittleEndian.PutUint16(header[3:], binary.LittleEndian.Uint16(header[3:])|0x10)
	binary.LittleEndian.PutUint16(header, uint16(crc32.ChecksumIEEE(header[2:])))
	m = archiveStreamTestMember(t, data, ".rar", 1, nil)
	if !m.rarLocator.Solid() {
		t.Fatal("generated second RAR member is not solid")
	}
	return data, m
}

func TestArchiveMemberStreamPartialCloseDoesNotDrain(t *testing.T) {
	payload := bytes.Repeat([]byte("small ranges must stay small\n"), 160000)
	for _, kind := range []string{"7z", "rar-direct", "rar-solid", "zip"} {
		t.Run(kind, func(t *testing.T) {
			var data []byte
			var m *member
			switch kind {
			case "7z":
				data = copy7zFixture(payload)
				m = archiveStreamTestMember(t, data, ".7z", 0, nil)
			case "rar-direct":
				data = storedRARImages(payload, 1)
				m = archiveStreamTestMember(t, data, ".rar", 0, nil)
			case "rar-solid":
				data, m = archiveStreamLargeSolidRAR(t, payload)
			case "zip":
				var buf bytes.Buffer
				zw := zip.NewWriter(&buf)
				w, err := zw.CreateHeader(&zip.FileHeader{Name: "payload", Method: zip.Store})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := w.Write(payload); err != nil {
					t.Fatal(err)
				}
				if err := zw.Close(); err != nil {
					t.Fatal(err)
				}
				data = buf.Bytes()
			}
			input := &archiveStreamCountedInput{Reader: bytes.NewReader(data)}
			if kind == "zip" {
				m = archiveZIPTestMember(t, input, int64(len(data)))
			}
			r, err := openArchiveMemberStream(context.Background(), input, int64(len(data)), m, nil)
			if err != nil {
				t.Fatal(err)
			}
			beforeRead := input.readBytes
			head := make([]byte, 32)
			if _, err := io.ReadFull(r, head); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(head, payload[:len(head)]) {
				t.Fatal("partial bytes differ")
			}
			beforeClose := input.readBytes
			if beforeClose-beforeRead >= int64(len(payload)) {
				t.Fatalf("32-byte read fetched entire %d-byte member", len(payload))
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			if input.readBytes != beforeClose {
				t.Fatalf("partial Close fetched %d additional archive bytes", input.readBytes-beforeClose)
			}
			if err := r.Close(); err != nil {
				t.Fatalf("second Close: %v", err)
			}
			if _, err := r.Read(head); !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("read after Close: %v", err)
			}
		})
	}
}

func TestArchiveMemberStreamFullReadRejectsChecksumFailure(t *testing.T) {
	payload := bytes.Repeat([]byte("checksum protected data\n"), 1000)
	for _, kind := range []string{"7z", "rar-direct", "rar-solid", "zip"} {
		t.Run(kind, func(t *testing.T) {
			var data []byte
			var m *member
			switch kind {
			case "7z":
				data = copy7zFixture(payload)
				m = archiveStreamTestMember(t, data, ".7z", 0, nil)
				data[32] ^= 1 // Packed Copy payload; metadata CRC remains valid.
			case "rar-direct":
				data = storedRARImages(payload, 1)
				m = archiveStreamTestMember(t, data, ".rar", 0, nil)
			case "rar-solid":
				data, m = archiveStreamLargeSolidRAR(t, payload)
			case "zip":
				var buf bytes.Buffer
				zw := zip.NewWriter(&buf)
				w, err := zw.CreateHeader(&zip.FileHeader{Name: "payload", Method: zip.Store})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := w.Write(payload); err != nil {
					t.Fatal(err)
				}
				if err := zw.Close(); err != nil {
					t.Fatal(err)
				}
				data = buf.Bytes()
				zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
				if err != nil {
					t.Fatal(err)
				}
				f := zr.File[0]
				offset, err := f.DataOffset()
				if err != nil {
					t.Fatal(err)
				}
				data[offset] ^= 1
				m = archiveZIPTestMember(t, bytes.NewReader(data), int64(len(data)))
			}
			if m.format == ".rar" {
				offset := rardecode.ExportLocator(*m.rarLocator).Offset
				headerSize := int64(binary.LittleEndian.Uint16(data[offset+5:]))
				data[offset+headerSize] ^= 1
			}
			r, err := openArchiveMemberStream(context.Background(), bytes.NewReader(data), int64(len(data)), m, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			_, err = io.ReadAll(r)
			var want error = errArchiveIntegrity
			if m.format == ".7z" {
				// A whole-group read can reject its group CRC before the
				// factory's own per-member checksum wrapper is reached.
				zr, openErr := sevenzip.NewReader(bytes.NewReader(data), int64(len(data)))
				if openErr != nil {
					t.Fatal(openErr)
				}
				group, openErr := zr.OpenStream(zr.File[m.ordinal].Stream)
				if openErr != nil {
					t.Fatal(openErr)
				}
				_, groupErr := io.ReadAll(group)
				_ = group.Close()
				var checksumErr *sevenzip.ReadError
				if !errors.As(groupErr, &checksumErr) {
					t.Fatalf("corrupted group has no decoder checksum error: %v", groupErr)
				}
				want = checksumErr.Err
			} else if m.format == ".rar" {
				want = rardecode.ErrBadFileChecksum
			} else if m.format == ".zip" {
				want = zip.ErrChecksum
			}
			if !errors.Is(err, want) {
				t.Fatalf("full member checksum failure: got %v, want %v", err, want)
			}
		})
	}
}

func TestArchiveMemberStreamCancellation(t *testing.T) {
	data := copy7zFixture([]byte("cancelled member"))
	m := archiveStreamTestMember(t, data, ".7z", 0, nil)
	ctx, cancel := context.WithCancel(context.Background())
	r, err := openArchiveMemberStream(ctx, bytes.NewReader(data), int64(len(data)), m, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	cancel()
	if _, err := r.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read: %v", err)
	}
	if _, err := openArchiveMemberStream(ctx, bytes.NewReader(data), int64(len(data)), m, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled open: %v", err)
	}
}

type archiveStreamOneShotChecksumError struct {
	data  []byte
	reads int
}

func (r *archiveStreamOneShotChecksumError) Read(p []byte) (int, error) {
	r.reads++
	if r.reads > 1 {
		return 0, io.EOF
	}
	return copy(p, r.data), rardecode.ErrBadFileChecksum
}

func TestArchiveMemberStreamRetainsChecksumErrorAfterCopyN(t *testing.T) {
	want := []byte("bytes returned with a terminal checksum failure")
	decoder := &archiveStreamOneShotChecksumError{data: want}
	stream := &archiveMemberStream{ctx: context.Background(), reader: decoder, closer: io.NopCloser(decoder)}
	defer stream.Close()
	var output bytes.Buffer
	// CopyN reports success when n equals the requested size, even though the
	// decoder returned these bytes together with a non-EOF integrity error.
	if n, err := io.CopyN(&output, stream, int64(len(want))); n != int64(len(want)) || err != nil {
		t.Fatalf("CopyN = %d, %v", n, err)
	}
	if !bytes.Equal(output.Bytes(), want) {
		t.Fatal("CopyN bytes differ")
	}
	for i := 0; i < 2; i++ {
		if n, err := stream.Read(make([]byte, 1)); n != 0 || !errors.Is(err, rardecode.ErrBadFileChecksum) {
			t.Fatalf("post-CopyN EOF check = %d, %v", n, err)
		}
	}
	if decoder.reads != 1 {
		t.Fatalf("terminal decoder was read %d times", decoder.reads)
	}
}

func TestArchiveMemberStream7zWithOptionalCRCOmitted(t *testing.T) {
	payload := []byte("valid optional-CRC 7z payload")
	data := copy7zFixture(payload)
	header := append([]byte(nil), data[32+len(payload):]...)
	removed := false
	for i := 0; i+6 <= len(header); i++ {
		if header[i] == 10 && header[i+1] == 1 && binary.LittleEndian.Uint32(header[i+2:i+6]) == crc32.ChecksumIEEE(payload) {
			header = append(header[:i], header[i+6:]...)
			removed = true
			break
		}
	}
	if !removed {
		t.Fatal("fixture has no optional digest to remove")
	}
	data = append(data[:32+len(payload)], header...)
	binary.LittleEndian.PutUint64(data[20:28], uint64(len(header)))
	binary.LittleEndian.PutUint32(data[28:32], crc32.ChecksumIEEE(header))
	binary.LittleEndian.PutUint32(data[8:12], crc32.ChecksumIEEE(data[12:32]))
	// Independently confirm that removing this optional field left a valid
	// archive, rather than weakening the opener to accept a broken fixture.
	zr, err := sevenzip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	ordinary, err := zr.File[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(ordinary)
	closeErr := ordinary.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(got, payload) {
		t.Fatalf("library rejected optional-CRC fixture: read=%v close=%v", readErr, closeErr)
	}
	if zr.File[0].CRC32 != 0 {
		t.Fatal("optional CRC was not removed")
	}
	m := archiveStreamTestMember(t, data, ".7z", 0, nil)
	r, err := openArchiveMemberStream(context.Background(), bytes.NewReader(data), int64(len(data)), m, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err = io.ReadAll(r)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("valid CRC-omitted member rejected: %v", err)
	}
}

func TestArchiveMemberStreamMissingCRCStillChecksLength(t *testing.T) {
	decoder := &archiveMember7zChecksum{reader: bytes.NewReader([]byte("short")), hash: crc32.NewIEEE(), left: 10}
	stream := &archiveMemberStream{ctx: context.Background(), reader: decoder, closer: io.NopCloser(decoder)}
	defer stream.Close()
	if _, err := io.ReadAll(stream); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated member without CRC: %v", err)
	}
}

func TestArchiveMemberStream7zRetainsGroupCRCFailureAfterCopyN(t *testing.T) {
	payload := []byte("a whole stream with a corrupt payload and valid metadata")
	data := copy7zFixture(payload)
	m := archiveStreamTestMember(t, data, ".7z", 0, nil)
	data[32] ^= 1
	stream, err := openArchiveMemberStream(context.Background(), bytes.NewReader(data), int64(len(data)), m, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if n, err := io.CopyN(io.Discard, stream, int64(len(payload))); n != int64(len(payload)) || err != nil {
		t.Fatalf("CopyN = %d, %v", n, err)
	}
	for i := 0; i < 2; i++ {
		n, err := stream.Read(make([]byte, 1))
		var checksumErr *sevenzip.ReadError
		if n != 0 || !errors.As(err, &checksumErr) {
			t.Fatalf("post-CopyN group checksum error = %d, %v", n, err)
		}
	}
}

func TestArchiveMemberStream7zMemberCRCIsCheckedBeforeGroupEnd(t *testing.T) {
	payload := []byte("member bytes")
	group := append(append([]byte(nil), payload...), []byte("later solid member")...)
	group[0] ^= 1
	decoder := &archiveMember7zChecksum{reader: bytes.NewReader(group), hash: crc32.NewIEEE(), left: int64(len(payload)), want: crc32.ChecksumIEEE(payload), defined: true}
	stream := &archiveMemberStream{ctx: context.Background(), reader: decoder, closer: io.NopCloser(decoder)}
	defer stream.Close()
	if _, err := io.ReadAll(stream); !errors.Is(err, errArchiveIntegrity) {
		t.Fatalf("member CRC failure before group EOF: %v", err)
	}
}
