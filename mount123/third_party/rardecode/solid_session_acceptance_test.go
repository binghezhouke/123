package rardecode

import (
	"bytes"
	stdcontext "context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"testing/fstest"
)

type solidRecordingFS struct {
	fs.FS
	files    []*solidRecordingFile
	attempts []string
	closeErr error
}

type solidRecordingFile struct {
	fs.File
	name     string
	closes   int
	closeErr error
}

func (f *solidRecordingFS) Open(name string) (fs.File, error) {
	f.attempts = append(f.attempts, name)
	file, err := f.FS.Open(name)
	if err != nil {
		return nil, err
	}
	recorded := &solidRecordingFile{File: file, name: name, closeErr: f.closeErr}
	f.files = append(f.files, recorded)
	return recorded, nil
}

func (f *solidRecordingFile) Close() error {
	f.closes++
	err := f.File.Close()
	if f.closeErr != nil {
		return f.closeErr
	}
	return err
}

func (f *solidRecordingFS) assertClosed(t *testing.T, count int) {
	t.Helper()
	if len(f.files) != count {
		t.Fatalf("opened files = %d, want %d (attempts %v)", len(f.files), count, f.attempts)
	}
	for _, file := range f.files {
		if file.closes != 1 {
			t.Errorf("volume %s Close calls = %d, want 1", file.name, file.closes)
		}
	}
}

func solidFixtureFS() fs.FS {
	return os.DirFS(filepath.Join("..", "..", "..", "tests", "fixtures", "rar"))
}

func assertSolidTerminal(t *testing.T, session *SolidSession, want error) {
	t.Helper()
	for i := 0; i < 2; i++ {
		if member, err := session.Next(); member != nil || !errors.Is(err, want) {
			t.Fatalf("terminal Next = %v, %v; want %v", member, err, want)
		}
		if n, err := session.Read(make([]byte, 1)); n != 0 || !errors.Is(err, want) {
			t.Fatalf("terminal Read = %d, %v; want %v", n, err, want)
		}
		if n, err := session.WriteTo(io.Discard); n != 0 || !errors.Is(err, want) {
			t.Fatalf("terminal WriteTo = %d, %v; want %v", n, err, want)
		}
		if err := session.Close(); err != nil {
			t.Fatalf("repeat session Close = %v", err)
		}
	}
}

func TestSolidSessionReleasesVolumeAndRetainsTerminalError(t *testing.T) {
	for _, fixture := range []string{"rar3-solid.rar", "rar5-solid.rar"} {
		for _, outcome := range []string{"eof", "explicit-close", "cancel", "writer-failure", "dictionary-limit"} {
			t.Run(fixture+"/"+outcome, func(t *testing.T) {
				recorded := &solidRecordingFS{FS: solidFixtureFS()}
				ctx, cancel := stdcontext.WithCancel(stdcontext.Background())
				defer cancel()
				opts := []Option{FileSystem(recorded)}
				if outcome == "dictionary-limit" {
					// Fixtures have 2048-byte members: the decoder caps the
					// declared dictionary to that size before applying the budget.
					opts = append(opts, MaxDictionarySize(2047))
				}
				session, err := OpenSolidSessionContext(ctx, fixture, opts...)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = session.Close() })
				member, err := session.Next()
				if outcome == "dictionary-limit" {
					if member != nil || !errors.Is(err, ErrDictionaryTooLarge) {
						t.Fatalf("dictionary budget = %v, %v", member, err)
					}
					assertSolidTerminal(t, session, ErrDictionaryTooLarge)
					recorded.assertClosed(t, 1)
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				var terminal error
				switch outcome {
				case "eof":
					for err == nil {
						if _, err = io.Copy(io.Discard, member); err != nil {
							t.Fatal(err)
						}
						member, err = session.Next()
					}
					terminal = io.EOF
				case "explicit-close":
					if err = session.Close(); err != nil {
						t.Fatal(err)
					}
					terminal = io.ErrClosedPipe
				case "cancel":
					cancel()
					_, err = member.Read(make([]byte, 1))
					terminal = stdcontext.Canceled
				case "writer-failure":
					_, err = member.WriteTo(failingSolidWriter{})
					terminal = io.ErrClosedPipe
				}
				if outcome != "explicit-close" && !errors.Is(err, terminal) {
					t.Fatalf("operation error = %v, want %v", err, terminal)
				}
				assertSolidTerminal(t, session, terminal)
				recorded.assertClosed(t, 1)
			})
		}
	}
}

// Corrupt the second member's declared data CRC, preserving its compressed
// stream and solid dictionary dependency. Recompute the header CRC so the
// failure is specifically ErrBadFileChecksum, not a malformed-header failure.
func solidBadCRCFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := fs.ReadFile(solidFixtureFS(), name)
	if err != nil {
		t.Fatal(err)
	}
	var offsets []int64
	err = WalkMembers(name, func(_ *FileHeader, locator MemberLocator) error {
		offsets = append(offsets, locator.offset)
		return nil
	}, FileSystem(solidFixtureFS()))
	if err != nil || len(offsets) != 2 {
		t.Fatalf("fixture headers = %v, error = %v", offsets, err)
	}
	off := int(offsets[1])
	if name == "rar3-solid.rar" {
		end := off + int(binary.LittleEndian.Uint16(data[off+5:]))
		data[off+16] ^= 1
		binary.LittleEndian.PutUint16(data[off:], uint16(crc32.ChecksumIEEE(data[off+2:end])))
	} else {
		headerSize, n := binary.Uvarint(data[off+4:])
		pos := off + 4 + n
		readVarint := func() uint64 {
			v, n := binary.Uvarint(data[pos:])
			if n <= 0 {
				t.Fatal("invalid fixture header varint")
			}
			pos += n
			return v
		}
		if readVarint() != block5File {
			t.Fatal("fixture locator does not point at file header")
		}
		flags := readVarint()
		if flags&block5HasExtra != 0 {
			readVarint()
		}
		if flags&block5HasData != 0 {
			readVarint()
		}
		fileFlags := readVarint()
		readVarint() // unpacked size
		readVarint() // attributes
		if fileFlags&file5HasUnixMtime != 0 {
			pos += 4
		}
		if fileFlags&file5HasCRC32 == 0 {
			t.Fatal("fixture lacks CRC32")
		}
		data[pos] ^= 1
		end := off + 4 + n + int(headerSize)
		binary.LittleEndian.PutUint32(data[off:], crc32.ChecksumIEEE(data[off+4:end]))
	}
	return data
}

func TestSolidSessionCRCFailureReleasesVolumeDespiteSkipCheck(t *testing.T) {
	for _, fixture := range []string{"rar3-solid.rar", "rar5-solid.rar"} {
		for _, advance := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/Next=%v", fixture, advance), func(t *testing.T) {
				recorded := &solidRecordingFS{FS: fstest.MapFS{fixture: &fstest.MapFile{Data: solidBadCRCFixture(t, fixture)}}}
				session, err := OpenSolidSession(fixture, FileSystem(recorded), SkipCheck)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = session.Close() })
				first, err := session.Next()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := io.Copy(io.Discard, first); err != nil {
					t.Fatal(err)
				}
				second, err := session.Next()
				if err != nil || !second.Header.Solid || second.Group != first.Group {
					t.Fatalf("second solid member = %v, %v", second, err)
				}
				if advance {
					if _, err := second.Read(make([]byte, 17)); err != nil {
						t.Fatal(err)
					}
					_, err = session.Next() // drain must verify before advancing
				} else {
					_, err = io.ReadAll(second)
				}
				if !errors.Is(err, ErrBadFileChecksum) {
					t.Fatalf("corrupt member = %v, want data CRC error", err)
				}
				assertSolidTerminal(t, session, ErrBadFileChecksum)
				recorded.assertClosed(t, 1)
			})
		}
	}
}

func TestSolidSessionDictionaryBudgetBoundary(t *testing.T) {
	for _, fixture := range []string{"rar3-solid.rar", "rar5-solid.rar"} {
		t.Run(fixture, func(t *testing.T) {
			session, err := OpenSolidSession(fixture, FileSystem(solidFixtureFS()), MaxDictionarySize(2048))
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			for i := 0; i < 2; i++ {
				member, err := session.Next()
				if err != nil {
					t.Fatal(err)
				}
				if data, err := io.ReadAll(member); err != nil || len(data) != 2048 {
					t.Fatalf("budget boundary bytes = %d, error = %v", len(data), err)
				}
			}
		})
	}
}

// Upstream volume fixtures are stored, non-solid archives. These tests prove
// session volume handling and release, not solid dictionary reuse across volumes.
func TestSolidSessionRealVolumes(t *testing.T) {
	for _, version := range []int{3, 5} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			recorded := &solidRecordingFS{FS: solidFixtureFS()}
			name := fmt.Sprintf("rar%d-vols.part1.rar", version)
			session, err := OpenSolidSession(name, FileSystem(recorded))
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			for i, lines := range []int{5000, 50} {
				member, err := session.Next()
				if err != nil {
					t.Fatal(err)
				}
				var expected bytes.Buffer
				for line := 1; line <= lines; line++ {
					fmt.Fprintf(&expected, "%40d\n", line)
				}
				data, err := io.ReadAll(member)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(data, expected.Bytes()) {
					t.Fatalf("member %d bytes differ: got %d, expected %d", i, len(data), expected.Len())
				}
			}
			wantVolumes := []string{name, fmt.Sprintf("rar%d-vols.part2.rar", version), fmt.Sprintf("rar%d-vols.part3.rar", version)}
			if got := session.Volumes(); !reflect.DeepEqual(got, wantVolumes) {
				t.Fatalf("opened volumes = %v, want %v", got, wantVolumes)
			}
			if _, err := session.Next(); err != io.EOF {
				t.Fatalf("end of volumes = %v", err)
			}
			assertSolidTerminal(t, session, io.EOF)
			recorded.assertClosed(t, 3)
		})
	}
}

func TestSolidSessionVolumeFailureReleasesOpenedFiles(t *testing.T) {
	for _, version := range []int{3, 5} {
		for _, outcome := range []string{"missing", "bad-header", "cancel", "crc"} {
			t.Run(fmt.Sprintf("%d/%s", version, outcome), func(t *testing.T) {
				volumes := fstest.MapFS{}
				for i := 1; i <= 3; i++ {
					if outcome == "missing" && i > 1 {
						continue
					}
					name := fmt.Sprintf("rar%d-vols.part%d.rar", version, i)
					data, err := fs.ReadFile(solidFixtureFS(), name)
					if err != nil {
						t.Fatal(err)
					}
					if outcome == "crc" && i == 2 {
						// Offset 200 is within the stored split-member payload
						// in both upstream fixtures, clear of all headers.
						data[200] ^= 1
					}
					if outcome == "bad-header" && i == 2 {
						// Preserve the signature but corrupt the main header CRC,
						// so failure occurs initializing an already opened volume.
						if version == 3 {
							data[7] ^= 1
						} else {
							data[8] ^= 1
						}
					}
					volumes[name] = &fstest.MapFile{Data: data}
				}
				recorded := &solidRecordingFS{FS: volumes}
				ctx, cancel := stdcontext.WithCancel(stdcontext.Background())
				defer cancel()
				session, err := OpenSolidSessionContext(ctx, fmt.Sprintf("rar%d-vols.part1.rar", version), FileSystem(recorded))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = session.Close() })
				member, err := session.Next()
				if err != nil {
					t.Fatal(err)
				}
				var want error
				count := 3
				switch outcome {
				case "missing":
					_, err = io.ReadAll(member)
					want, count = fs.ErrNotExist, 1
				case "bad-header":
					_, err = io.ReadAll(member)
					want, count = ErrBadHeaderCRC, 2
				case "cancel":
					if _, err = io.ReadFull(member, make([]byte, 120000)); err != nil {
						t.Fatal(err)
					}
					if len(recorded.files) != 2 {
						t.Fatalf("prefix opened %d volumes, want 2", len(recorded.files))
					}
					cancel()
					_, err = member.Read(make([]byte, 1))
					want, count = stdcontext.Canceled, 2
				case "crc":
					_, err = io.ReadAll(member)
					want = ErrBadFileChecksum
				}
				if !errors.Is(err, want) {
					t.Fatalf("volume failure = %v, want %v", err, want)
				}
				assertSolidTerminal(t, session, want)
				recorded.assertClosed(t, count)
			})
		}
	}
}

func TestSolidSessionCloseFailureDoesNotReplaceTerminalError(t *testing.T) {
	closeErr := errors.New("fixture Close failure")
	for _, outcome := range []string{"cancel", "crc", "explicit-close"} {
		t.Run(outcome, func(t *testing.T) {
			var fixtureFS fs.FS = solidFixtureFS()
			if outcome == "crc" {
				fixtureFS = fstest.MapFS{"rar5-solid.rar": &fstest.MapFile{Data: solidBadCRCFixture(t, "rar5-solid.rar")}}
			}
			recorded := &solidRecordingFS{FS: fixtureFS, closeErr: closeErr}
			ctx, cancel := stdcontext.WithCancel(stdcontext.Background())
			defer cancel()
			session, err := OpenSolidSessionContext(ctx, "rar5-solid.rar", FileSystem(recorded))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = session.Close() })
			member, err := session.Next()
			if err != nil {
				t.Fatal(err)
			}
			var want error
			switch outcome {
			case "cancel":
				cancel()
				_, err = member.Read(make([]byte, 1))
				want = stdcontext.Canceled
			case "crc":
				if _, err = io.ReadAll(member); err != nil {
					t.Fatal(err)
				}
				member, err = session.Next()
				if err != nil {
					t.Fatal(err)
				}
				_, err = io.ReadAll(member)
				want = ErrBadFileChecksum
			case "explicit-close":
				err = session.Close()
				if !errors.Is(err, closeErr) {
					t.Fatalf("explicit Close = %v, want %v", err, closeErr)
				}
				want = io.ErrClosedPipe
			}
			if outcome != "explicit-close" && !errors.Is(err, want) {
				t.Fatalf("terminal error = %v, want %v", err, want)
			}
			assertSolidTerminal(t, session, want)
			recorded.assertClosed(t, 1)
		})
	}
}
