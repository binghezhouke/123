package rardecode

import (
	"bytes"
	stdctx "context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// The buggy scanner never returns or reads again. Isolate each public API
// replay so a regression fails without leaving a CPU-spinning test goroutine.
func TestRARSignatureScan(t *testing.T) {
	const caseEnv = "RAR_SIGNATURE_SCAN_CASE"
	cases := []struct {
		name   string
		prefix int
		valid  bool
	}{
		{"plain", 0, true},
		{"executable_prefix", 1536, true},
		{"false_candidate", 1536, true},
		{"split_signature", 509, true},
		{"missing_signature", 1536, false},
		{"past_scan_limit", maxSfxSize + 1024, false},
	}
	selected := os.Getenv(caseEnv)
	for _, version := range []int{3, 5} {
		for _, tc := range cases {
			name := fmt.Sprintf("v%d/%s", version, tc.name)
			if selected == "" {
				t.Run(name, func(t *testing.T) {
					ctx, cancel := stdctx.WithTimeout(stdctx.Background(), 3*time.Second)
					defer cancel()
					cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRARSignatureScan$", "-test.count=1")
					cmd.Env = append(os.Environ(), caseEnv+"="+name)
					out, err := cmd.CombinedOutput()
					if ctx.Err() != nil {
						t.Fatalf("signature scan made no progress: %v", ctx.Err())
					}
					if err != nil {
						t.Fatalf("signature scan failed: %v\n%s", err, out)
					}
				})
				continue
			}
			if selected != name {
				continue
			}
			payload := []byte("synthetic RAR payload")
			archive, _, _ := storedRangeFixture(t, version, payload, crc32.ChecksumIEEE(payload), false)
			prefix := make([]byte, tc.prefix)
			if len(prefix) > 0 {
				copy(prefix, "MZ")
			}
			if tc.name == "false_candidate" {
				copy(prefix[256:], "R-not-a-signature")
			}
			data := append(prefix, archive...)
			if tc.name == "missing_signature" {
				data = prefix
			}
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "sample.rar"), data, 0600); err != nil {
				t.Fatal(err)
			}
			options := []Option{FileSystem(os.DirFS(dir)), BufferSize(512)}
			var locator MemberLocator
			members := 0
			err := WalkMembers("sample.rar", func(h *FileHeader, l MemberLocator) error {
				members++
				locator = l
				return nil
			}, options...)
			if !tc.valid {
				if !errors.Is(err, ErrNoSig) || members != 0 {
					t.Fatalf("invalid signature: members=%d err=%v", members, err)
				}
				return
			}
			if err != nil || members != 1 {
				t.Fatalf("walk: members=%d err=%v", members, err)
			}
			_, reader, err := OpenMember("sample.rar", locator, options...)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			got, err := io.ReadAll(reader)
			if err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("direct member read differs: bytes=%d err=%v", len(got), err)
			}
			return
		}
	}
	if selected != "" {
		t.Fatalf("unknown subprocess case %q", selected)
	}
}
