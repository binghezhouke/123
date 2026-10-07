package mountfs

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"testing"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func TestLegacyZIPNamesThroughLookup(t *testing.T) {
	for _, test := range []struct {
		name, raw, want string
		extra           []byte
	}{
		{name: "GBK", raw: "\xd6\xd0\xce\xc4.jpg", want: "中文.jpg"},
		{name: "CP437", raw: "caf\x82.jpg", want: "café.jpg"},
		{name: "unicode-extra", raw: "old.jpg", want: "新.jpg", extra: unicodeExtra("old.jpg", "新.jpg")},
	} {
		t.Run(test.name, func(t *testing.T) {
			var buf bytes.Buffer
			writer := zip.NewWriter(&buf)
			member, err := writer.CreateHeader(&zip.FileHeader{Name: test.raw, NonUTF8: true, Extra: test.extra})
			if err != nil {
				t.Fatal(err)
			}
			member.Write([]byte("image"))
			writer.Close()
			root := fixtureArchive(t, buf.Bytes())
			fs.NewNodeFS(root, &fs.Options{})
			archive := lookup(t, root, "photos.zip")
			if _, errno := archive.Lookup(context.Background(), test.want, &fuse.EntryOut{}); errno != 0 {
				t.Fatalf("decoded filename unavailable: %v", errno)
			}
		})
	}
}
func unicodeExtra(raw, name string) []byte {
	b := make([]byte, 9+len(name))
	binary.LittleEndian.PutUint16(b, 0x7075)
	binary.LittleEndian.PutUint16(b[2:], uint16(5+len(name)))
	b[4] = 1
	binary.LittleEndian.PutUint32(b[5:], crc32.ChecksumIEEE([]byte(raw)))
	copy(b[9:], name)
	return b
}
