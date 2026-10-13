package rardecode

import (
	"bytes"
	stdcontext "context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"testing"
	"testing/fstest"
)

func TestSolidSessionFromRejectsInvalidStarts(t *testing.T) {
	for _, name := range []string{"rar3-two-groups.rar", "rar5-two-groups.rar"} {
		for _, tc := range []struct {
			ordinal int
			want    error
		}{{-1, fs.ErrInvalid}, {1, ErrSolidOpen}, {3, ErrSolidOpen}, {4, io.EOF}, {99, io.EOF}} {
			t.Run(fmt.Sprintf("%s/%d", name, tc.ordinal), func(t *testing.T) {
				recorded := &solidRecordingFS{FS: solidFixtureFS()}
				s, err := OpenSolidSessionFrom(name, tc.ordinal, FileSystem(recorded))
				if s != nil || !errors.Is(err, tc.want) {
					t.Fatalf("start = %v, %v; want %v", s, err, tc.want)
				}
				count := 1
				if tc.ordinal < 0 {
					count = 0
					if len(recorded.attempts) != 0 {
						t.Fatal("negative ordinal opened archive")
					}
				}
				recorded.assertClosed(t, count)
			})
		}
	}
	// Neither an empty regular file nor a directory resets a compression dictionary.
	var ordinal int
	err := Walk("rar5-group-boundaries.rar", func(h *FileHeader) error {
		if h.IsDir || h.UnPackedSize == 0 {
			s, err := OpenSolidSessionFrom("rar5-group-boundaries.rar", ordinal, FileSystem(solidFixtureFS()))
			if s != nil || !errors.Is(err, fs.ErrInvalid) {
				t.Fatalf("non-data anchor = %v, %v", s, err)
			}
		}
		ordinal++
		return nil
	}, FileSystem(solidFixtureFS()))
	if err != nil || ordinal != 3 {
		t.Fatalf("boundary fixtures: count %d, %v", ordinal, err)
	}
	// A stored independent file does not initialize the compression dictionary.
	s, err := OpenSolidSessionFrom("rar5-vols.part1.rar", 1, FileSystem(solidFixtureFS()))
	if s != nil || !errors.Is(err, fs.ErrInvalid) {
		t.Fatalf("stored anchor = %v, %v", s, err)
	}
}

func TestSolidSessionFromKeepsTargetChecks(t *testing.T) {
	for _, name := range []string{"rar3-two-groups.rar", "rar5-two-groups.rar"} {
		t.Run(name+"/checksum", func(t *testing.T) {
			recorded := &solidRecordingFS{FS: groupFixture(t, name, 2)}
			s, err := OpenSolidSessionFrom(name, 2, FileSystem(recorded), SkipCheck)
			if err != nil {
				t.Fatal(err)
			}
			member, err := s.Next()
			if err == nil {
				_, err = io.Copy(io.Discard, member)
			}
			if !errors.Is(err, ErrBadFileChecksum) {
				t.Fatalf("target checksum: %v", err)
			}
			assertSolidTerminal(t, s, ErrBadFileChecksum)
			recorded.assertClosed(t, 1)
		})
		t.Run(name+"/dictionary", func(t *testing.T) {
			recorded := &solidRecordingFS{FS: solidFixtureFS()}
			s, err := OpenSolidSessionFrom(name, 2, FileSystem(recorded), MaxDictionarySize(4096))
			if err != nil {
				t.Fatal(err)
			}
			if member, err := s.Next(); member != nil || !errors.Is(err, ErrDictionaryTooLarge) {
				t.Fatalf("target dictionary: %v, %v", member, err)
			}
			assertSolidTerminal(t, s, ErrDictionaryTooLarge)
			recorded.assertClosed(t, 1)
		})
	}
	for _, name := range []string{"rar3-two-groups-password.rar", "rar5-two-groups-password.rar"} {
		t.Run(name, func(t *testing.T) {
			for _, password := range []string{"password", "wrong"} {
				recorded := &solidRecordingFS{FS: solidFixtureFS()}
				s, err := OpenSolidSessionFrom(name, 2, FileSystem(recorded), Password(password))
				if err == nil {
					member, nextErr := s.Next()
					err = nextErr
					if err == nil {
						var got []byte
						got, err = io.ReadAll(member)
						if password == "password" && !bytes.Equal(got, solidFixturePayload()) {
							t.Fatal("correct password bytes mismatch")
						}
					}
					s.Close()
				}
				if (password == "password") != (err == nil) {
					t.Fatalf("password %q: %v", password, err)
				}
				recorded.assertClosed(t, 1)
			}
		})
	}
}

func multipartGroupFixture(t *testing.T, version int, corrupt bool) (string, fstest.MapFS) {
	t.Helper()
	fixture := fstest.MapFS{}
	for part := 1; part <= 7; part++ {
		name := fmt.Sprintf("rar%d-two-groups-multipart.part%02d.rar", version, part)
		data, err := os.ReadFile("../../../tests/fixtures/rar/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if corrupt && part == 2 {
			data[200] ^= 0x80
		} // Payload only, headers remain valid.
		fixture[name] = &fstest.MapFile{Data: data}
	}
	return fmt.Sprintf("rar%d-two-groups-multipart.part01.rar", version), fixture
}

func TestSolidSessionFromSkipsPreviousGroupAcrossVolumes(t *testing.T) {
	for _, version := range []int{3, 5} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			name, fixture := multipartGroupFixture(t, version, true)
			old, err := OpenSolidSession(name, FileSystem(fixture))
			if err != nil {
				t.Fatal(err)
			}
			first, err := old.Next()
			if err == nil {
				_, err = io.Copy(io.Discard, first)
			}
			old.Close()
			if err == nil {
				t.Fatal("corrupt previous split member unexpectedly succeeded")
			}
			recorded := &solidRecordingFS{FS: fixture}
			s, err := OpenSolidSessionFrom(name, 2, FileSystem(recorded))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			for ordinal := 2; ordinal < 4; ordinal++ {
				m, err := s.Next()
				if err != nil {
					t.Fatal(err)
				}
				got, err := io.ReadAll(m)
				if err != nil || !bytes.Equal(got, solidFixturePayload()) || m.Ordinal != ordinal || m.Group != 0 {
					t.Fatalf("split target %d: %v", ordinal, err)
				}
			}
			if len(s.Volumes()) != 7 {
				t.Fatalf("opened volume snapshot: %v", s.Volumes())
			}
			if m, err := s.Next(); m != nil || err != io.EOF {
				t.Fatalf("end %v, %v", m, err)
			}
			if s.Volumes() != nil {
				t.Fatal("terminal Volumes must be nil")
			}
			recorded.assertClosed(t, 7)
		})
	}
}

type groupCancelFS struct {
	fs.FS
	cancel stdcontext.CancelFunc
	opens  int
}

func (f *groupCancelFS) Open(name string) (fs.File, error) {
	file, err := f.FS.Open(name)
	if err == nil {
		f.opens++
		if f.opens == 2 {
			f.cancel()
		}
	}
	return file, err
}

func TestSolidSessionFromCancellationAndMissingVolume(t *testing.T) {
	ctx, cancel := stdcontext.WithCancel(stdcontext.Background())
	cancel()
	recorded := &solidRecordingFS{FS: solidFixtureFS()}
	if s, err := OpenSolidSessionFromContext(ctx, "rar5-two-groups.rar", 2, FileSystem(recorded)); s != nil || !errors.Is(err, stdcontext.Canceled) {
		t.Fatalf("pre-cancel: %v, %v", s, err)
	}
	if len(recorded.attempts) != 0 {
		t.Fatal("pre-canceled start opened archive")
	}
	for _, version := range []int{3, 5} {
		name, fixture := multipartGroupFixture(t, version, false)
		ctx, cancel := stdcontext.WithCancel(stdcontext.Background())
		recorded := &solidRecordingFS{FS: fixture}
		cancelFS := &groupCancelFS{FS: recorded, cancel: cancel}
		s, err := OpenSolidSessionFromContext(ctx, name, 2, FileSystem(cancelFS))
		cancel()
		if s != nil || !errors.Is(err, stdcontext.Canceled) {
			t.Fatalf("cancel skip: %v, %v", s, err)
		}
		recorded.assertClosed(t, 2)
		delete(fixture, fmt.Sprintf("rar%d-two-groups-multipart.part02.rar", version))
		recorded = &solidRecordingFS{FS: fixture}
		if s, err := OpenSolidSessionFrom(name, 2, FileSystem(recorded)); s != nil || !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("missing preceding volume: %v, %v", s, err)
		}
		recorded.assertClosed(t, 1)
	}
	ctx, cancel = stdcontext.WithCancel(stdcontext.Background())
	recorded = &solidRecordingFS{FS: solidFixtureFS()}
	s, err := OpenSolidSessionFromContext(ctx, "rar5-two-groups.rar", 2, FileSystem(recorded))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	defer cancel()
	m, err := s.Next()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Read(make([]byte, 100)); err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := m.Read(make([]byte, 1)); !errors.Is(err, stdcontext.Canceled) {
		t.Fatalf("active cancel: %v", err)
	}
	assertSolidTerminal(t, s, stdcontext.Canceled)
	recorded.assertClosed(t, 1)
}

func TestSolidSessionFromReleasesBeforeFirstNext(t *testing.T) {
	for _, version := range []int{3, 5} {
		for _, cancelFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/cancel=%t", version, cancelFirst), func(t *testing.T) {
				name, fixture := multipartGroupFixture(t, version, false)
				recorded := &solidRecordingFS{FS: fixture}
				ctx, cancel := stdcontext.WithCancel(stdcontext.Background())
				defer cancel()
				s, err := OpenSolidSessionFromContext(ctx, name, 2, FileSystem(recorded))
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				if len(recorded.files) != 4 || len(s.Volumes()) != 4 {
					t.Fatalf("group start should open only first four volumes: %d", len(recorded.files))
				}
				if cancelFirst {
					cancel()
					if m, err := s.Next(); m != nil || !errors.Is(err, stdcontext.Canceled) {
						t.Fatalf("cancel primed target: %v, %v", m, err)
					}
					assertSolidTerminal(t, s, stdcontext.Canceled)
				} else {
					if err := s.Close(); err != nil {
						t.Fatal(err)
					}
					if err := s.Close(); err != nil {
						t.Fatal(err)
					}
					if m, err := s.Next(); m != nil || !errors.Is(err, io.ErrClosedPipe) {
						t.Fatalf("closed primed target: %v, %v", m, err)
					}
				}
				recorded.assertClosed(t, 4)
			})
		}
	}
}

func groupFixture(t *testing.T, name string, corruptOrdinal int) fs.FS {
	t.Helper()
	data, err := os.ReadFile("../../../tests/fixtures/rar/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if corruptOrdinal >= 0 {
		// Only mutate packed payload; preserve all headers and their checksums.
		v, err := openVolume(name, getOptions([]Option{FileSystem(solidFixtureFS())}))
		if err != nil {
			t.Fatal(err)
		}
		pr := newPackedFileReader(v, v.opt)
		var blocks *fileBlockList
		for i := 0; i <= corruptOrdinal; i++ {
			blocks, err = pr.nextFile()
			if err != nil {
				break
			}
		}
		if err != nil {
			v.Close()
			t.Fatal(err)
		}
		h := blocks.firstBlock()
		data[h.dataOff+h.PackedSize/2] ^= 0x80
		if err := v.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return fstest.MapFS{name: &fstest.MapFile{Data: data}}
}

func TestSolidSessionFromSkipsCorruptPreviousGroup(t *testing.T) {
	for _, name := range []string{"rar3-two-groups.rar", "rar5-two-groups.rar"} {
		t.Run(name, func(t *testing.T) {
			fixture := groupFixture(t, name, 0)
			old, err := OpenSolidSession(name, FileSystem(fixture))
			if err != nil {
				t.Fatal(err)
			}
			first, err := old.Next()
			if err == nil {
				_, err = io.Copy(io.Discard, first)
			}
			old.Close()
			if err == nil {
				t.Fatal("corrupt previous group unexpectedly decoded successfully")
			}
			recorded := &solidRecordingFS{FS: fixture}
			s, err := OpenSolidSessionFrom(name, 2, FileSystem(recorded), SkipCheck)
			if err != nil {
				t.Fatalf("independent group after corrupt payload: %v", err)
			}
			defer s.Close()
			for ordinal := 2; ordinal < 4; ordinal++ {
				member, err := s.Next()
				if err != nil {
					t.Fatal(err)
				}
				if member.Ordinal != ordinal || member.Group != 0 || member.Header.Solid != (ordinal == 3) {
					t.Fatalf("incorrect group member: %+v", member)
				}
				got, err := io.ReadAll(member)
				if err != nil || !bytes.Equal(got, solidFixturePayload()) {
					t.Fatalf("member %d mismatch: %v", ordinal, err)
				}
			}
			if member, err := s.Next(); member != nil || err != io.EOF {
				t.Fatalf("end: %v, %v", member, err)
			}
			recorded.assertClosed(t, 1)
		})
	}
}

func TestSolidSessionFromDoesNotInitializePreviousDictionary(t *testing.T) {
	for _, name := range []string{"rar3-group-budget.rar", "rar5-group-budget.rar"} {
		t.Run(name, func(t *testing.T) {
			opts := []Option{FileSystem(solidFixtureFS()), MaxDictionarySize(4096)}
			old, err := OpenSolidSession(name, opts...)
			if err != nil {
				t.Fatal(err)
			}
			_, err = old.Next()
			old.Close()
			if !errors.Is(err, ErrDictionaryTooLarge) {
				t.Fatalf("prior 98304-byte member must reject 4096-byte budget: %v", err)
			}
			s, err := OpenSolidSessionFrom(name, 2, opts...)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			for ordinal := 2; ordinal < 4; ordinal++ {
				m, err := s.Next()
				if err != nil {
					t.Fatal(err)
				}
				got, err := io.ReadAll(m)
				if err != nil || !bytes.Equal(got, solidFixturePayload()[:4096]) || m.Ordinal != ordinal {
					t.Fatalf("4096-byte target member %d: %v", ordinal, err)
				}
			}
		})
	}
}
