package mountfs

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"
	"time"
)

// The independent 7-Zip CLI accepts this tiny Copy fixture. Its first group
// inherits one folder CRC; the second group's eight member CRCs use one bitmap
// byte. Counting nine compact CRC slots instead rejects this valid inner file.
func TestServiceNested7zMixedCRC(t *testing.T) {
	fixture, err := os.ReadFile("testdata/solid7z/mixed-crc.7z")
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []uint16{zip.Store, zip.Deflate} {
		for _, corrupt := range []bool{false, true} {
			t.Run(fmt.Sprintf("method=%d/corrupt=%t", method, corrupt), func(t *testing.T) {
				data := append([]byte(nil), fixture...)
				if corrupt {
					data[32+8] ^= 1
				} // Last member; no folder CRC to mask a lost file CRC.
				outer := nestedDeflateLeaseZIP(t, "inner.7z", data, method)
				service, _, _ := nestedDeflateLeaseSource(t, outer)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				entries, err := service.ListArchive(ctx, 77, "inner.7z")
				if err != nil || len(entries) != 9 {
					t.Fatalf("valid inner layout: count=%d err=%v", len(entries), err)
				}
				for i := 0; i < 9; i++ {
					name := fmt.Sprintf("member-%02d.bin", i)
					if i == 0 {
						name = "standalone.bin"
					}
					reader, size, err := service.OpenArchiveMember(ctx, 77, "inner.7z/"+name)
					if err != nil || size != 1 {
						t.Fatalf("member%d open: size=%d err=%v", i, size, err)
					}
					got := make([]byte, 1)
					n, readErr := reader.ReadAt(got, 0)
					closeErr := reader.(io.Closer).Close()
					if corrupt && i == 8 {
						if !errors.Is(readErr, errArchiveIntegrity) {
							t.Fatalf("corrupt member CRC not enforced: n=%d err=%v", n, readErr)
						}
					} else if n != 1 || readErr != nil || got[0] != byte('A'+i) || closeErr != nil {
						t.Fatalf("member%d bytes=%x n=%d read=%v close=%v", i, got, n, readErr, closeErr)
					}
				}
			})
		}
	}
}
