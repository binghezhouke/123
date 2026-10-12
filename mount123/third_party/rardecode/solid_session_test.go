package rardecode

import (
	"bytes"
	stdcontext "context"
	"errors"
	"io"
	"path/filepath"
	"testing"
)

func TestSolidSessionReadsMembersInOrder(t *testing.T) {
	path := filepath.Join("..", "..", "..", "tests", "fixtures", "rar", "rar5-crc.rar")
	session, err := OpenSolidSession(path)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	member, err := session.Next()
	if err != nil {
		t.Fatal(err)
	}
	if member.Ordinal != 0 || member.Group != 0 || member.Header == nil {
		t.Fatalf("first member metadata = %#v", member)
	}
	data, err := io.ReadAll(member)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(data)) != member.Header.UnPackedSize {
		t.Fatalf("first member size = %d, want %d", len(data), member.Header.UnPackedSize)
	}
	if _, err := session.Next(); err != nil && err != io.EOF {
		t.Fatal(err)
	}
}

func TestSolidSessionRetainsDecoderAndExpiresPreviousMember(t *testing.T) {
	for _, fixture := range []string{"rar3-solid.rar", "rar5-solid.rar"} {
		t.Run(fixture, func(t *testing.T) { checkSolidSession(t, fixture) })
	}
}

func checkSolidSession(t *testing.T, fixture string) {
	path := filepath.Join("..", "..", "..", "tests", "fixtures", "rar", fixture)
	s, err := OpenSolidSession(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	first, err := s.Next()
	if err != nil {
		t.Fatal(err)
	}
	prefix := make([]byte, 17)
	if _, err = io.ReadFull(first, prefix); err != nil {
		t.Fatal(err)
	}
	// Next must drain and verify the remaining first member before advancing.
	second, err := s.Next()
	if err != nil {
		t.Fatal(err)
	}
	if !second.Header.Solid || second.Group != first.Group || second.Ordinal != 1 {
		t.Fatalf("not same solid group: %#v", second)
	}
	if _, err = first.Read(prefix); err != io.ErrClosedPipe {
		t.Fatalf("stale member: %v", err)
	}
	got, err := io.ReadAll(second)
	if err != nil {
		t.Fatal(err)
	}
	// Compare against a separate sequential decoder, including the full bytes.
	reference, err := OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reference.Close()
	if _, err = reference.Next(); err != nil {
		t.Fatal(err)
	}
	if _, err = reference.Next(); err != nil {
		t.Fatal(err)
	}
	want, err := io.ReadAll(reference)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) || len(got) != 2048 {
		t.Fatal("solid member content differs")
	}
	if len(s.Volumes()) != 1 {
		t.Fatalf("volumes: %v", s.Volumes())
	}
	if _, err = s.Next(); err != io.EOF {
		t.Fatalf("end: %v", err)
	}
}

func TestSolidSessionCancellationAndWriterFailure(t *testing.T) {
	path := filepath.Join("..", "..", "..", "tests", "fixtures", "rar", "rar3-solid.rar")
	ctx, cancel := stdcontext.WithCancel(stdcontext.Background())
	s, err := OpenSolidSessionContext(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	member, err := s.Next()
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err = member.Read(make([]byte, 1)); !errors.Is(err, stdcontext.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	if s.archive != nil {
		t.Fatal("cancel retained archive")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenSolidSession(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	member, err = s.Next()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = member.WriteTo(failingSolidWriter{}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("writer: %v", err)
	}
	if _, err = s.Next(); err == nil {
		t.Fatal("advanced after incomplete failed write")
	}
}

func TestSolidSessionVerifiesEncryptedMembers(t *testing.T) {
	path := filepath.Join("..", "..", "..", "tests", "fixtures", "rar", "rar5-psw.rar")
	session, err := OpenSolidSession(path, Password("password"), SkipCheck)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	member, err := session.Next()
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(member)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 || int64(len(data)) != member.Header.UnPackedSize {
		t.Fatalf("encrypted member size = %d, want %d", len(data), member.Header.UnPackedSize)
	}

	wrong, err := OpenSolidSession(path, Password("wrong"))
	if err != nil {
		// Header-encrypted archives are allowed to reject the session at open.
		return
	}
	defer wrong.Close()
	badMember, err := wrong.Next()
	if err != nil {
		return // Header-encrypted archives may reject on the first header.
	}
	if _, err = io.ReadAll(badMember); err == nil {
		t.Fatal("wrong password unexpectedly decoded encrypted member")
	}
}

type failingSolidWriter struct{}

func (failingSolidWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
