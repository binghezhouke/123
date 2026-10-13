package mountfs

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func archiveZIPTestMember(t *testing.T, source io.ReaderAt, size int64) *member {
	t.Helper()
	idx, err := buildZIP(context.Background(), &Tree{opts: defaults(Options{MetadataBytes: 64 << 20})}, zipStreamReaderAt{reader: source}, size)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range idx.members {
		return m
	}
	t.Fatal("ZIP fixture has no member")
	return nil
}

func TestZIPMemberStreamEncryptedFixtures(t *testing.T) {
	files, err := filepath.Glob("testdata/encrypted/*.zip")
	if err != nil {
		t.Fatal(err)
	}
	want := append(bytes.Repeat([]byte("mount123 encrypted fixture\n"), 256), byteSliceRange()...)
	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			input := &archiveStreamCountedInput{Reader: bytes.NewReader(data)}
			m := archiveZIPTestMember(t, input, int64(len(data)))
			if m.format != ".zip" || m.ordinal != 0 {
				t.Fatalf("indexed location: format=%q ordinal=%d", m.format, m.ordinal)
			}
			r, err := openArchiveMemberStream(context.Background(), input, int64(len(data)), m, []byte("mount-test-password"))
			if err != nil {
				t.Fatal(err)
			}
			prefix := make([]byte, 32)
			if _, err := io.ReadFull(r, prefix); err != nil || !bytes.Equal(prefix, want[:len(prefix)]) {
				t.Fatalf("partial bytes differ: %v", err)
			}
			beforeClose := input.readBytes
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			if input.readBytes != beforeClose {
				t.Fatal("partial close read or authenticated unread payload")
			}
			r, err = openArchiveMemberStream(context.Background(), input, int64(len(data)), m, []byte("mount-test-password"))
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			got, err := io.ReadAll(r)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("full decrypted stream: size=%d error=%v", len(got), err)
			}
			if _, err := openArchiveMemberStream(context.Background(), input, int64(len(data)), m, []byte("wrong")); !errors.Is(err, ErrWrongZIPPassword) {
				t.Fatalf("wrong password: %v", err)
			}
		})
	}
}

func TestZIPMemberStreamRetainsAESAuthenticationErrorAtEOF(t *testing.T) {
	for _, fixture := range []string{"aes256-ae2-store.zip", "aes256-ae2-deflate.zip"} {
		t.Run(fixture, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata/encrypted", fixture))
			if err != nil {
				t.Fatal(err)
			}
			m := archiveZIPTestMember(t, bytes.NewReader(data), int64(len(data)))
			offset, err := m.dataOffset(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			data[offset+int64(m.compressed)-1] ^= 1
			r, err := openArchiveMemberStream(context.Background(), bytes.NewReader(data), int64(len(data)), m, []byte("mount-test-password"))
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			// CopyN may swallow an error returned with the final bytes. The
			// required EOF probe must retain the authentication failure.
			_, _ = io.CopyN(io.Discard, r, int64(m.size))
			if _, err := r.Read(make([]byte, 1)); !errors.Is(err, ErrWrongZIPPassword) {
				t.Fatalf("EOF authentication: %v", err)
			}
			if _, err := r.Read(make([]byte, 1)); !errors.Is(err, ErrWrongZIPPassword) {
				t.Fatalf("retained EOF authentication: %v", err)
			}
		})
	}
}

func zipStreamPlainFixture(t *testing.T, method uint16, payload []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "payload", Method: method})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestZIPMemberStreamRebindsIndexedInput(t *testing.T) {
	payload := []byte("current input bytes")
	data := zipStreamPlainFixture(t, zip.Store, payload)
	m := archiveZIPTestMember(t, bytes.NewReader(data), int64(len(data)))
	current := append([]byte(nil), data...)
	offset, err := m.dataOffset(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	current[offset] ^= 1
	r, err := openArchiveMemberStream(context.Background(), bytes.NewReader(current), int64(len(current)), m, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if len(got) == 0 || got[0] != payload[0]^1 || !errors.Is(err, zip.ErrChecksum) {
		t.Fatalf("source was not rebound: got=%q error=%v", got, err)
	}
}

type zipStreamContextInput struct {
	*bytes.Reader
	contextReads int
	fail         error
}

func (r *zipStreamContextInput) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	r.contextReads++
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if r.fail != nil {
		return 0, r.fail
	}
	return r.Reader.ReadAt(p, off)
}

func TestZIPMemberStreamContextAndSourceErrors(t *testing.T) {
	data := zipStreamPlainFixture(t, zip.Deflate, bytes.Repeat([]byte("context payload"), 100))
	m := archiveZIPTestMember(t, bytes.NewReader(data), int64(len(data)))
	input := &zipStreamContextInput{Reader: bytes.NewReader(data)}
	ctx, cancel := context.WithCancel(context.Background())
	r, err := openArchiveMemberStream(ctx, input, int64(len(data)), m, nil)
	if err != nil {
		t.Fatal(err)
	}
	if input.contextReads == 0 {
		t.Fatal("local header did not use contextual ancestor input")
	}
	cancel()
	if _, err := r.Read(make([]byte, 10)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled ZIP read: %v", err)
	}
	_ = r.Close()
	// A cancelled stream does not poison its metadata or ancestor reader.
	r, err = openArchiveMemberStream(context.Background(), input, int64(len(data)), m, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	sentinel := errors.New("ancestor input failed")
	input.fail = sentinel
	if _, err := r.Read(make([]byte, 10)); !errors.Is(err, sentinel) {
		t.Fatalf("source error lost: %v", err)
	}
}

func TestZIPMemberStreamChecksSizeAndAESVersionCRC(t *testing.T) {
	for _, method := range []uint16{zip.Store, zip.Deflate} {
		data := zipStreamPlainFixture(t, method, []byte("length validation"))
		base := archiveZIPTestMember(t, bytes.NewReader(data), int64(len(data)))
		for _, delta := range []int64{-1, 1} {
			m := *base
			m.size = uint64(int64(m.size) + delta)
			r, err := openArchiveMemberStream(context.Background(), bytes.NewReader(data), int64(len(data)), &m, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, err = io.ReadAll(r)
			_ = r.Close()
			if !errors.Is(err, zip.ErrChecksum) {
				t.Fatalf("method=%d size delta=%d: %v", method, delta, err)
			}
		}
	}
	files, err := filepath.Glob("testdata/encrypted/*ae*.zip")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			m := archiveZIPTestMember(t, bytes.NewReader(data), int64(len(data)))
			m.crc ^= 1
			r, err := openArchiveMemberStream(context.Background(), bytes.NewReader(data), int64(len(data)), m, []byte("mount-test-password"))
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			_, err = io.ReadAll(r)
			if m.aes.version == 1 && !errors.Is(err, ErrWrongZIPPassword) {
				t.Fatalf("AE-1 CRC mismatch: %v", err)
			}
			if m.aes.version == 2 && err != nil {
				t.Fatalf("AE-2 uses authentication instead of CRC: %v", err)
			}
		})
	}
}
