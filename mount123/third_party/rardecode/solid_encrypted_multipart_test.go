package rardecode

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"testing"
)

func solidEncryptedMultipartName(version int) string {
	return fmt.Sprintf("rar%d-solid-encrypted-multipart.part01.rar", version)
}

func TestSolidSessionEncryptedMultipartKnownBytes(t *testing.T) {
	want := solidMultipartPayload()
	for _, version := range []int{3, 5} {
		for _, partial := range []bool{false, true} {
			t.Run(fmt.Sprintf("RAR%d/partial=%v", version, partial), func(t *testing.T) {
				recorded := &solidRecordingFS{FS: solidFixtureFS()}
				// This password belongs only to the synthetic public fixture.
				session, err := OpenSolidSession(solidEncryptedMultipartName(version), FileSystem(recorded), Password("password"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = session.Close() })
				first, err := session.Next()
				if err != nil {
					t.Fatal(err)
				}
				if first.Header.Name != "first.bin" || !first.Header.Encrypted || first.Header.Solid {
					t.Fatalf("encrypted first member = %#v", first)
				}
				if partial {
					prefix := make([]byte, 257)
					if _, err := io.ReadFull(first, prefix); err != nil || !bytes.Equal(prefix, want[:len(prefix)]) {
						t.Fatalf("encrypted prefix matches=%v, error=%v", bytes.Equal(prefix, want[:len(prefix)]), err)
					}
				} else if got, err := io.ReadAll(first); err != nil || !bytes.Equal(got, want) {
					t.Fatalf("encrypted first matches=%v, error=%v", bytes.Equal(got, want), err)
				}
				second, err := session.Next() // decrypt and verify the remaining split first member
				if err != nil {
					t.Fatal(err)
				}
				if second.Header.Name != "second.bin" || !second.Header.Encrypted || !second.Header.Solid || second.Group != first.Group || second.Ordinal != 1 {
					t.Fatalf("encrypted dependent member = %#v", second)
				}
				if len(recorded.files) != 4 {
					t.Fatalf("encrypted dependent member begins after %d volumes, want 4", len(recorded.files))
				}
				if got, err := io.ReadAll(second); err != nil || !bytes.Equal(got, want) {
					t.Fatalf("encrypted second matches=%v, error=%v", bytes.Equal(got, want), err)
				}
				if _, err := session.Next(); err != io.EOF {
					t.Fatalf("encrypted session end=%v", err)
				}
				assertSolidTerminal(t, session, io.EOF)
				recorded.assertClosed(t, 4)
			})
		}
	}
}

func TestSolidSessionEncryptedMultipartWrongPasswordReleasesFiles(t *testing.T) {
	for _, version := range []int{3, 5} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			recorded := &solidRecordingFS{FS: solidFixtureFS()}
			session, err := OpenSolidSession(solidEncryptedMultipartName(version), FileSystem(recorded), Password("wrong"))
			if err != nil {
				t.Fatal(err) // these fixtures deliberately have readable, unencrypted headers
			}
			t.Cleanup(func() { _ = session.Close() })
			member, err := session.Next()
			if err == nil {
				var data []byte
				data, err = io.ReadAll(member)
				if bytes.Equal(data, solidMultipartPayload()) {
					t.Fatal("wrong password produced the known fixture bytes")
				}
			}
			if err == nil {
				t.Fatal("wrong password unexpectedly passed member verification")
			}
			want := ErrBadFileChecksum
			if version == 5 {
				want = ErrBadPassword
			}
			if !errors.Is(err, want) {
				t.Fatalf("wrong password error=%v, want %v", err, want)
			}
			t.Logf("wrong password rejected: %v", err)
			assertSolidTerminal(t, session, err)
			recorded.assertClosed(t, len(recorded.files))
		})
	}
}
