package rardecode

import (
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
