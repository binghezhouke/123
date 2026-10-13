package mountfs

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
)

type unavailableZIPCacheInput struct{ reads atomic.Int64 }

func (r *unavailableZIPCacheInput) ReadAtContext(context.Context, []byte, int64) (int, error) {
	r.reads.Add(1)
	return 0, syscall.EIO
}

func TestRootZIPCachedMemberDoesNotReadRawHeaders(t *testing.T) {
	var plain bytes.Buffer
	zw := zip.NewWriter(&plain)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "payload.txt", Method: zip.Deflate})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Write(bytes.Repeat([]byte("synthetic payload\n"), 300)); err != nil {
		t.Fatal(err)
	}
	if err = zw.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		data     []byte
		password []byte
	}{
		{"deflate", plain.Bytes(), nil},
		{"zipcrypto", nil, []byte("mount-test-password")},
		{"aes", nil, []byte("mount-test-password")},
	} {
		if tc.data == nil {
			fixture := "zipcrypto-deflate.zip"
			if tc.name == "aes" {
				fixture = "aes256-deflate.zip"
			}
			tc.data, err = os.ReadFile("testdata/encrypted/" + fixture)
			if err != nil {
				t.Fatal(err)
			}
		}
		for _, frontend := range []string{"service", "fuse"} {
			t.Run(tc.name+"/"+frontend, func(t *testing.T) {
				s, root, _ := rootZIPSplitFixture(t, tc.data, 100, tc.password, Options{DisableStreamMembers: true, DisableReadAhead: true})
				idx, _, _, pw, err := s.outerArchiveIndex(context.Background(), 1)
				clear(pw)
				if err != nil {
					t.Fatal(err)
				}
				var name string
				for name = range idx.members {
					break
				}
				r, size, err := s.OpenArchiveMember(context.Background(), 1, name)
				if err != nil {
					t.Fatal(err)
				}
				want := make([]byte, size)
				if _, err = r.ReadAt(want, 0); err != nil {
					t.Fatal(err)
				}
				if c, ok := r.(io.Closer); ok {
					c.Close()
				}
				var leaf *Node
				if frontend == "fuse" {
					leaf = lookup(t, root, "sample.zip.001")
					for _, part := range bytes.Split([]byte(name), []byte("/")) {
						leaf = lookup(t, leaf, string(part))
					}
				}
				// Simulate evicted raw ranges and unavailable remote I/O while retaining
				// the validated index and complete member cache. Content is unchanged.
				unavailable := &unavailableZIPCacheInput{}
				idx.prepared.input = unavailable
				if frontend == "service" {
					r, _, err = s.OpenArchiveMember(context.Background(), 1, name)
					if err != nil {
						t.Fatalf("cached member open uses raw header: %v", err)
					}
					if c, ok := r.(io.Closer); ok {
						defer c.Close()
					}
					got := make([]byte, size)
					if _, err = r.ReadAt(got, 0); err != nil || !bytes.Equal(got, want) {
						t.Fatalf("cached read: %v", err)
					}
				} else {
					readOther(t, leaf, want)
				}
				if unavailable.reads.Load() != 0 {
					t.Fatalf("warm member read raw input %d times", unavailable.reads.Load())
				}
			})
		}
	}
}
