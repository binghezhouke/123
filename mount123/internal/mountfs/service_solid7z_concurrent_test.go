package mountfs

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bodgit/sevenzip"
	"github.com/hanwen/go-fuse/v2/fs"
)

type solidConcurrentProbeGate struct {
	io.ReadCloser
	ctx             context.Context
	entered, resume chan struct{}
	once            sync.Once
	damageFirstByte bool
	position        int64
}

func (g *solidConcurrentProbeGate) Read(p []byte) (int, error) {
	g.once.Do(func() { close(g.entered) })
	select {
	case <-g.resume:
	case <-g.ctx.Done():
		return 0, g.ctx.Err()
	}
	n, err := g.ReadCloser.Read(p)
	if g.damageFirstByte && g.position == 0 && n > 0 {
		p[0] ^= 1 // The group/member CRC must reject captured output as well.
	}
	g.position += int64(n)
	return n, err
}

// Hold the first codec read until both earlier demands are registered. This
// proves the output was available to help them; late arrivals may still replay.
func TestServiceSolid7zConcurrentOutOfOrderDemand(t *testing.T) {
	const flag = "MOUNT123_SOLID_CONCURRENT_PROBE"
	if os.Getenv(flag) != "1" {
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, exe, "-test.run=^TestServiceSolid7zConcurrentOutOfOrderDemand$", "-test.count=1", "-test.v")
		cmd.Env = append(os.Environ(), flag+"=1")
		out, err := cmd.CombinedOutput()
		t.Logf("isolated probe:\n%s", out)
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	inner, err := os.ReadFile("testdata/solid7z/inner.7z")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, format                               string
		method                                     uint16
		encrypted, budgetFallback, corruptProducer bool
	}{
		{name: "root", format: "7z"},
		{name: "store", format: "zip", method: zip.Store},
		{name: "deflate", format: "zip", method: zip.Deflate},
		{name: "root-aes", format: "7z", encrypted: true},
		{name: "store-aes", format: "zip", method: zip.Store, encrypted: true},
		{name: "deflate-aes", format: "zip", method: zip.Deflate, encrypted: true},
		{name: "pending-budget-fallback", format: "7z", budgetFallback: true},
		{name: "root-failed-producer", format: "7z", corruptProducer: true},
		{name: "nested-failed-producer", format: "zip", method: zip.Store, corruptProducer: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			entered, resume := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(resume) }) }
			defer unblock()
			counts := new(solid7zReuseCounts)
			sevenzip.RegisterDecompressor([]byte{0x21}, func(p []byte, size uint64, inputs []io.ReadCloser) (io.ReadCloser, error) {
				r, err := counts.factory(p, size, inputs)
				if err != nil {
					return nil, err
				}
				if counts.initializations.Load() == 1 {
					return &solidConcurrentProbeGate{ReadCloser: r, ctx: ctx, entered: entered, resume: resume, damageFirstByte: tc.corruptProducer}, nil
				}
				return r, nil
			})
			fixture, password := inner, []byte(nil)
			if tc.encrypted {
				fixture, err = os.ReadFile("testdata/solid7z/encrypted.7z")
				if err != nil {
					t.Fatal(err)
				}
				password = []byte("synthetic-solid-password")
			}
			body, prefix := fixture, ""
			if tc.format == "zip" {
				body = nestedDeflateLeaseZIP(t, "inner.7z", fixture, tc.method)
				prefix = "inner.7z/"
			}
			original, api := otherArchiveFixture(t, "outer."+tc.format, body, password, false)
			root := NewWithOptions(ctx, api, original.tree.cache, 0, true, Options{DisableReadAhead: true, DisableStreamMembers: true, MaxConcurrentBuilds: 4})
			fs.NewNodeFS(root, &fs.Options{})
			if err := root.Prepare(ctx); err != nil {
				t.Fatal(err)
			}
			s := NewServiceFromNode(root)
			if tc.budgetFallback {
				s.tree.nestedSolid7zPool.maxPendingBytes = 0
			}
			defer s.tree.nestedSolid7zPool.Close()
			if _, err := s.ListArchive(ctx, 77, strings.TrimSuffix(prefix, "/")); err != nil {
				t.Fatal(err)
			}
			handles := make([]io.ReaderAt, 3)
			for i := range handles {
				r, _, err := s.OpenArchiveMember(ctx, 77, prefix+fmt.Sprintf("member-%c.bin", 'a'+i))
				if err != nil {
					t.Fatal(err)
				}
				handles[i] = r
				defer r.(io.Closer).Close()
			}
			middle := lookup(t, root, "outer."+tc.format)
			for _, part := range strings.Split(prefix+"member-b.bin", "/") {
				middle = lookup(t, middle, part)
			}
			fuseHandle, _, errno := middle.Open(ctx, syscall.O_RDONLY)
			if errno != 0 {
				t.Fatal(errno)
			}
			defer fuseHandle.(fs.FileReleaser).Release(ctx)
			if counts.initializations.Load() != 0 {
				t.Fatal("metadata unexpectedly decoded body")
			}
			type readResult struct {
				index int
				err   error
			}
			done := make(chan readResult, 3)
			read := func(i int, viaFUSE bool) {
				buf := make([]byte, 64<<10)
				var n int
				var err error
				if viaFUSE {
					result, errno := fuseHandle.(fs.FileReader).Read(ctx, buf, 0)
					if errno != 0 {
						err = errno
					} else {
						got, status := result.Bytes(nil)
						if status != 0 {
							err = syscall.Errno(status)
						} else {
							n = copy(buf, got)
						}
						result.Done()
					}
				} else {
					n, err = solidReuseRead(ctx, handles[i], buf, 0)
				}
				if err == nil && (n != len(buf) || !bytes.Equal(buf, solid7zReusePayload(i)[:len(buf)])) {
					err = fmt.Errorf("member %d bytes differ", i)
				}
				done <- readResult{i, err}
			}
			go read(2, false)
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("late-member decoder did not start")
			}
			go read(0, false)
			go read(1, true)
			for !tc.budgetFallback {
				pool := s.tree.nestedSolid7zPool
				pool.mu.Lock()
				pending := 0
				for _, entry := range pool.entries {
					pending += len(entry.demands)
				}
				pool.mu.Unlock()
				if pending == 2 {
					break
				}
				select {
				case <-ctx.Done():
					unblock()
					t.Fatal("earlier demands did not reach the shared group")
				case <-time.After(time.Millisecond):
				}
			}
			unblock()
			for range 3 {
				result := <-done
				if tc.corruptProducer && result.index == 2 {
					if result.err == nil {
						t.Fatal("corrupt producer passed its member CRC")
					}
				} else if result.err != nil {
					t.Fatal(result.err)
				}
			}
			if tc.corruptProducer {
				// The failed producer and its captured prefix must not poison a
				// later retry. A/B already compared every returned byte above.
				read(2, false)
				if result := <-done; result.err != nil {
					t.Fatal(result.err)
				}
			}
			initial, output := counts.initializations.Load(), counts.returnedOutput.Load()
			t.Logf("concurrent C/A/B: decoder_initializations=%d returned_output=%d", initial, output)
			for _, i := range []int{0, 1, 2} {
				read(i, false)
				if result := <-done; result.err != nil {
					t.Fatal(result.err)
				}
			}
			if counts.initializations.Load() != initial || counts.returnedOutput.Load() != output {
				t.Fatal("cached repeat decoded again")
			}
			wantInitial, wantOutput := int64(1), int64(9437210)
			if tc.budgetFallback {
				wantInitial, wantOutput = 3, 15728679
			}
			if !tc.corruptProducer && (initial != wantInitial || output != wantOutput) {
				t.Fatalf("pending earlier ranges replayed prefix: initializations=%d returned=%d", initial, output)
			}
			if s.tree.nestedSolid7zPool.pendingBytes != 0 {
				t.Fatal("completed waiters retained output reservations")
			}
		})
	}
}
