package mountfs

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/simplifiedchinese"
)

// zipName changes only the displayed path, retaining the raw header for member
// reads and cache identity. Legacy encoding is ambiguous; prefer GB18030 for
// non-UTF8 Chinese archives, then CP437. Unicode flags/validated extras win.
func zipName(f *zip.File) (string, error) {
	raw := []byte(f.Name)
	if f.Flags&0x800 != 0 {
		if !utf8.Valid(raw) {
			return "", fmt.Errorf("invalid UTF-8 ZIP filename")
		}
		return f.Name, nil
	}
	for extra := f.Extra; len(extra) >= 4; {
		id := binary.LittleEndian.Uint16(extra)
		size := int(binary.LittleEndian.Uint16(extra[2:]))
		extra = extra[4:]
		if size > len(extra) {
			break
		}
		field := extra[:size]
		extra = extra[size:]
		if id == 0x7075 && len(field) >= 5 && field[0] == 1 && binary.LittleEndian.Uint32(field[1:]) == crc32.ChecksumIEEE(raw) && utf8.Valid(field[5:]) {
			return string(field[5:]), nil
		}
	}
	if utf8.Valid(raw) {
		return f.Name, nil
	}
	decoded, err := simplifiedchinese.GB18030.NewDecoder().Bytes(raw)
	if err == nil && !strings.ContainsRune(string(decoded), utf8.RuneError) {
		encoded, err := simplifiedchinese.GB18030.NewEncoder().Bytes(decoded)
		if err == nil && bytes.Equal(raw, encoded) {
			return string(decoded), nil
		}
	}
	decoded, err = charmap.CodePage437.NewDecoder().Bytes(raw)
	return string(decoded), err
}
