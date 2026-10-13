package mountfs

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nwaples/rardecode/v2"
)

func rarGroupPayload() []byte {
	state := uint32(0x86)
	block := make([]byte, 49152)
	for i := range block {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		block[i] = byte(state)
	}
	return append(block, block...)
}

func TestServiceSolidRARCurrentGroupFailureDoesNotReplayPredecessor(t *testing.T) {
	for _, name := range []string{"rar3-two-groups.rar", "rar5-two-groups.rar"} {
		t.Run(name, func(t *testing.T) {
			s := multilevelService(t, "rar", rarGroupData(t, name, 2), Options{DisableReadAhead: true, MaxConcurrentBuilds: 1}, 16<<20)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := s.ListArchive(ctx, 77, ""); err != nil {
				t.Fatal(err)
			}
			before := s.tree.cache.Stats().Foreground.RequestedBytes
			r, _, err := s.OpenArchiveMember(ctx, 77, "d.bin")
			if err == nil {
				_, err = r.ReadAt(make([]byte, len(rarGroupPayload())), 0)
				r.(io.Closer).Close()
			}
			if !errors.Is(err, syscall.EIO) {
				t.Fatalf("selected group checksum: %v", err)
			}
			requested := s.tree.cache.Stats().Foreground.RequestedBytes - before
			t.Logf("failed group source requested %d bytes", requested)
			// A fallback replays both the preceding group and the bad current
			// group (~153 KiB); one failed current-group attempt is ~54 KiB.
			if requested >= 64<<10 {
				t.Fatalf("failed producer reopened archive prefix: %d source bytes", requested)
			}
		})
	}
}

func TestServiceSolidRARGroupConcurrentReadsAndBudgetFallback(t *testing.T) {
	for _, capacity := range []int64{16 << 20, 512 << 10} {
		t.Run(fmt.Sprint(capacity), func(t *testing.T) {
			s := multilevelService(t, "rar", rarGroupData(t, "rar5-two-groups.rar", -1), Options{DisableReadAhead: true, MaxConcurrentBuilds: 1}, capacity)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := s.ListArchive(ctx, 77, ""); err != nil {
				t.Fatal(err)
			}
			before := s.tree.cache.Stats().Foreground.RequestedBytes
			var wg sync.WaitGroup
			errs := make(chan error, 2)
			names := []string{"c.bin", "d.bin"}
			for _, name := range names {
				wg.Add(1)
				go func() {
					defer wg.Done()
					r, size, err := s.OpenArchiveMember(ctx, 77, name)
					if err == nil {
						got := make([]byte, size)
						n, readErr := r.ReadAt(got, 0)
						r.(io.Closer).Close()
						err = readErr
						if err == nil && (n != len(rarGroupPayload()) || !bytes.Equal(got, rarGroupPayload())) {
							err = fmt.Errorf("member content mismatch")
						}
					}
					errs <- err
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			requested := s.tree.cache.Stats().Foreground.RequestedBytes - before
			if capacity == 16<<20 && requested >= 64<<10 {
				t.Fatalf("same-group readers did not share one producer: %d source bytes", requested)
			}
		})
	}
}

func TestServiceSolidRARStoredAndEmptyAnchorsRemainReadable(t *testing.T) {
	data := storedRARImages([]byte("stored group bytes\n"), 2)
	ordinal := 0
	err := rardecode.WalkMembers("archive.rar", func(_ *rardecode.FileHeader, loc rardecode.MemberLocator) error {
		if ordinal == 1 {
			off := rardecode.ExportLocator(loc).Offset
			flags := binary.LittleEndian.Uint16(data[off+3:]) | 0x10
			binary.LittleEndian.PutUint16(data[off+3:], flags)
			size := binary.LittleEndian.Uint16(data[off+5:])
			binary.LittleEndian.PutUint16(data[off:], uint16(crc32.ChecksumIEEE(data[off+2:off+int64(size)])))
		}
		ordinal++
		return nil
	}, rardecode.FileSystem(singleArchiveFS{bytes.NewReader(data), int64(len(data))}))
	if err != nil {
		t.Fatal(err)
	}
	s := multilevelService(t, "rar", data, Options{MaxConcurrentBuilds: 1}, 16<<20)
	idx, _, _, _, err := s.outerArchiveIndex(context.Background(), 77)
	if err != nil {
		t.Fatal(err)
	}
	if m := idx.members["0002.jpg"]; m.rarGroupSize == 0 || m.rarGroupStartable {
		t.Fatal("stored group must retain legacy session path")
	}
	multilevelRead(t, s, "0002.jpg", []byte("stored group bytes\n"))
	s = multilevelService(t, "rar", rarGroupData(t, "rar5-group-boundaries.rar", -1), Options{MaxConcurrentBuilds: 1}, 16<<20)
	multilevelRead(t, s, "empty.bin", nil)
}

func TestServiceSolidRARGroupEligibilitySurvivesDTOAndOldIndex(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint(legacy), func(t *testing.T) {
			s := multilevelService(t, "rar", rarGroupData(t, "rar5-two-groups.rar", -1), Options{MaxConcurrentBuilds: 1}, 16<<20)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			idx, a, source, password, err := s.outerArchiveIndex(ctx, 77)
			if err != nil {
				t.Fatal(err)
			}
			defer clear(password)
			if !idx.members["d.bin"].rarGroupStartable {
				t.Fatal("fresh group eligibility missing")
			}
			if legacy {
				key := s.tree.archiveIndexCacheKey(".rar", archiveIdentity(source, a), a, password)
				h, err := s.tree.cache.OpenArchiveIndex(key)
				if err != nil {
					t.Fatal(err)
				}
				blob := make([]byte, h.Size())
				_, err = h.ReadAt(blob, 0)
				h.Close()
				if err != nil {
					t.Fatal(err)
				}
				var dto map[string]any
				if err := json.Unmarshal(blob, &dto); err != nil {
					t.Fatal(err)
				}
				for _, entry := range dto["entries"].([]any) {
					delete(entry.(map[string]any), "rar_group_startable")
				}
				blob, err = json.Marshal(dto)
				if err != nil {
					t.Fatal(err)
				}
				if err := s.tree.cache.ReplaceArchiveIndex(ctx, key, blob); err != nil {
					t.Fatal(err)
				}
			}
			restored, err := NewService(ctx, s.tree.api, s.tree.cache, 0, true, Options{MaxConcurrentBuilds: 1})
			if err != nil {
				t.Fatal(err)
			}
			idx, _, _, _, err = restored.outerArchiveIndex(ctx, 77)
			if err != nil {
				t.Fatal(err)
			}
			if idx.members["d.bin"].rarGroupStartable != !legacy {
				t.Fatal("restored eligibility disagrees with persisted field")
			}
			multilevelRead(t, restored, "d.bin", rarGroupPayload())
		})
	}
}

func rarGroupData(t *testing.T, name string, corruptOrdinal int) []byte {
	t.Helper()
	data, err := os.ReadFile("../../../tests/fixtures/rar/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if corruptOrdinal >= 0 {
		ordinal := 0
		err := rardecode.WalkMembersFrom("archive.rar", 0, func(h *rardecode.FileHeader, _ rardecode.MemberLocator, next int64) error {
			if ordinal == corruptOrdinal {
				data[next-h.PackedSize+h.PackedSize/2] ^= 0x80
			}
			ordinal++
			return nil
		}, rardecode.FileSystem(singleArchiveFS{bytes.NewReader(data), int64(len(data))}))
		if err != nil {
			t.Fatal(err)
		}
	}
	return data
}

func TestServiceSolidRARLaterGroupSkipsCorruptPredecessor(t *testing.T) {
	for _, name := range []string{"rar3-two-groups.rar", "rar5-two-groups.rar"} {
		t.Run(name, func(t *testing.T) {
			data := rarGroupData(t, name, 0)
			s := multilevelService(t, "rar", data, Options{DisableReadAhead: true, MaxConcurrentBuilds: 1}, 16<<20)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			rows, err := s.ListArchive(ctx, 77, "")
			if err != nil || len(rows) != 4 {
				t.Fatalf("header-only listing = %d, %v", len(rows), err)
			}
			r, size, err := s.OpenArchiveMember(ctx, 77, "d.bin")
			if err != nil {
				t.Fatalf("later independent solid group: %v", err)
			}
			defer r.(io.Closer).Close()
			want := rarGroupPayload()
			got := make([]byte, size)
			if n, err := r.ReadAt(got, 0); err != nil || n != len(want) || !bytes.Equal(got, want) {
				t.Fatalf("later group bytes n=%d err=%v", n, err)
			}
		})
	}
}
