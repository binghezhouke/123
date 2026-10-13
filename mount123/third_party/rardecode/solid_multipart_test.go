package rardecode

import (
	"bytes"
	stdcontext "context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"testing"
	"testing/fstest"
)

func solidFixturePayload() []byte {
	state := uint32(0x86)
	block := make([]byte, 49152)
	for i := range block {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		block[i] = byte(state)
	}
	return append(block, block...)
}

func solidMultipartName(version, part int) string {
	return fmt.Sprintf("rar%d-solid-multipart.part%02d.rar", version, part)
}

func TestSolidSessionCompressedSolidMultipart(t *testing.T) {
	want := solidFixturePayload()
	if got := fmt.Sprintf("%x", sha256.Sum256(want)); got != "04bdbf62c09c8fe4899c780f22302811a45d787bbfea06df80f7ab642876099e" {
		t.Fatalf("generated oracle hash = %s", got)
	}
	for _, version := range []int{3, 5} {
		for _, partial := range []bool{false, true} {
			t.Run(fmt.Sprintf("RAR%d/partial=%v", version, partial), func(t *testing.T) {
				recorded := &solidRecordingFS{FS: solidFixtureFS()}
				session, err := OpenSolidSession(solidMultipartName(version, 1), FileSystem(recorded))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = session.Close() })
				first, err := session.Next()
				if err != nil {
					t.Fatal(err)
				}
				if first.Header.Name != "first.bin" || first.Ordinal != 0 || first.Header.Solid {
					t.Fatalf("first member = %#v", first)
				}
				if partial {
					prefix := make([]byte, 257)
					if _, err := io.ReadFull(first, prefix); err != nil || !bytes.Equal(prefix, want[:len(prefix)]) {
						t.Fatalf("partial first bytes match = %v, error = %v", bytes.Equal(prefix, want[:len(prefix)]), err)
					}
				} else if got, err := io.ReadAll(first); err != nil || !bytes.Equal(got, want) {
					t.Fatalf("full first bytes match = %v, error = %v", bytes.Equal(got, want), err)
				}
				second, err := session.Next() // partial mode must drain across volumes
				if err != nil {
					t.Fatal(err)
				}
				if second.Header.Name != "second.bin" || second.Ordinal != 1 || !second.Header.Solid || second.Group != first.Group {
					t.Fatalf("second member = %#v", second)
				}
				if len(recorded.files) != 4 {
					t.Fatalf("second member started after %d volumes, want 4", len(recorded.files))
				}
				if _, err := first.Read(make([]byte, 1)); err != io.ErrClosedPipe {
					t.Fatalf("stale first member = %v", err)
				}
				if got, err := io.ReadAll(second); err != nil || !bytes.Equal(got, want) {
					t.Fatalf("dependent second bytes match = %v, error = %v", bytes.Equal(got, want), err)
				}
				if _, err := session.Next(); err != io.EOF {
					t.Fatalf("end of solid volumes = %v", err)
				}
				assertSolidTerminal(t, session, io.EOF)
				recorded.assertClosed(t, 4)
			})
		}
	}
}

func TestSolidMultipartRequiresPrecedingDecoderState(t *testing.T) {
	for _, version := range []int{3, 5} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			opt := getOptions([]Option{FileSystem(solidFixtureFS())})
			volume, err := openVolume(solidMultipartName(version, 1), opt)
			if err != nil {
				t.Fatal(err)
			}
			defer volume.Close()
			packed := newPackedFileReader(volume, opt)
			first, err := packed.nextFile()
			if err != nil || first.firstBlock().decVer == 0 {
				t.Fatalf("first member is not compressed: %v", err)
			}
			// Discard the first member's packed blocks without decoding them.
			// This is deliberately a negative decoder probe, not a session API.
			second, err := packed.nextFile()
			if err != nil {
				t.Fatal(err)
			}
			h := second.firstBlock()
			if !h.Solid || h.decVer == 0 || h.volnum != 3 || h.PackedSize >= h.UnPackedSize/100 {
				t.Fatalf("dependent compressed member = %#v", h)
			}
			reader, err := packed.newArchiveFile(second)
			if err != nil {
				t.Logf("independent decoder rejected dependent stream: %v", err)
				return
			}
			got, err := io.ReadAll(reader)
			if err == nil || bytes.Equal(got, solidFixturePayload()) {
				t.Fatalf("second member decoded without preceding dictionary: bytes=%d, error=%v", len(got), err)
			}
			t.Logf("independent decoder failed as expected: %v", err)
		})
	}
}

func solidMultipartBadCRC(t *testing.T, version int, data []byte) []byte {
	t.Helper()
	volume, err := newVolume(bytes.NewReader(data), getOptions(nil), 3)
	if err != nil {
		t.Fatal(err)
	}
	for {
		offset := int(volume.br.off + volume.n)
		header, err := volume.nextBlock()
		if err != nil {
			t.Fatal(err)
		}
		if header.Name != "second.bin" {
			continue
		}
		end := int(header.dataOff)
		body := data[offset:end]
		if len(header.sum) != 4 || bytes.Count(body, header.sum) != 1 {
			t.Fatal("second member's CRC does not have a unique header location")
		}
		pos := offset + bytes.Index(body, header.sum)
		data[pos] ^= 1 // compressed bytes and all split flags remain intact
		if version == 3 {
			binary.LittleEndian.PutUint16(data[offset:], uint16(crc32.ChecksumIEEE(data[offset+2:end])))
		} else {
			binary.LittleEndian.PutUint32(data[offset:], crc32.ChecksumIEEE(data[offset+4:end]))
		}
		return data
	}
}

func TestSolidSessionCompressedMultipartFailures(t *testing.T) {
	for _, version := range []int{3, 5} {
		for _, failure := range []string{"missing-middle", "crc", "cancel"} {
			t.Run(fmt.Sprintf("RAR%d/%s", version, failure), func(t *testing.T) {
				volumes := fstest.MapFS{}
				for i := 1; i <= 4; i++ {
					if failure == "missing-middle" && i == 2 {
						continue
					}
					name := solidMultipartName(version, i)
					data, err := fs.ReadFile(solidFixtureFS(), name)
					if err != nil {
						t.Fatal(err)
					}
					if failure == "crc" && i == 4 {
						data = solidMultipartBadCRC(t, version, data)
					}
					volumes[name] = &fstest.MapFile{Data: data}
				}
				recorded := &solidRecordingFS{FS: volumes}
				ctx, cancel := stdcontext.WithCancel(stdcontext.Background())
				defer cancel()
				session, err := OpenSolidSessionContext(ctx, solidMultipartName(version, 1), FileSystem(recorded), SkipCheck)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = session.Close() })
				first, err := session.Next()
				if err != nil {
					t.Fatal(err)
				}
				var want error
				count := 4
				switch failure {
				case "missing-middle":
					_, err = io.ReadAll(first)
					want, count = fs.ErrNotExist, 1
				case "crc":
					second, nextErr := session.Next() // drains valid first across all four volumes
					if nextErr != nil {
						t.Fatal(nextErr)
					}
					_, err = io.ReadAll(second)
					want = ErrBadFileChecksum
				case "cancel":
					prefix := make([]byte, 20000)
					if _, err = io.ReadFull(first, prefix); err != nil {
						t.Fatal(err)
					}
					count = len(recorded.files)
					if count < 2 || !bytes.Equal(prefix, solidFixturePayload()[:len(prefix)]) {
						t.Fatalf("prefix crossed %d volumes, bytes match=%v", count, bytes.Equal(prefix, solidFixturePayload()[:len(prefix)]))
					}
					cancel()
					_, err = first.Read(make([]byte, 1))
					want = stdcontext.Canceled
				}
				if !errors.Is(err, want) {
					t.Fatalf("solid multipart failure = %v, want %v", err, want)
				}
				assertSolidTerminal(t, session, want)
				recorded.assertClosed(t, count)
			})
		}
	}
}
