package mountfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"sync"

	"github.com/binghezhouke/123/mount123/internal/storage"
)

type zipDeflateCheckpoint struct {
	OutputOffset, InputOffset int64
	BitOffset                 uint8
	Window                    []byte
	CRC                       uint32
}

type zipDeflateCheckpointHooks struct {
	Want                  func(int64) bool
	Save                  func(zipDeflateCheckpoint)
	Valid, Invalid, Close func()
}

const (
	zipDeflateCheckpointWindowBytes = 32 << 10
	zipDeflateCheckpointHeaderBytes = 96
	zipDeflateCheckpointRecordBytes = zipDeflateCheckpointHeaderBytes + zipDeflateCheckpointWindowBytes + sha256.Size
	zipDeflateCheckpointMinStride   = int64(4 << 20)
)

// The store owns immutable records, not decoders or member generations. Each
// stream holds at most one pending window per slot until its complete CRC is
// accepted. Verified records share the ordinary cache's disk budget and LRU.
type zipDeflateCheckpoints struct {
	cache            *storage.Cache
	key              string
	identity         [sha256.Size]byte
	size, compressed int64
	stride           int64
	slots            int
}

func newZIPDeflateCheckpoints(cache *storage.Cache, key string, size, compressed int64) *zipDeflateCheckpoints {
	if cache == nil || key == "" || size < 8<<20 || compressed <= 0 {
		return nil
	}
	slots := int(min(int64(16), cache.Capacity()/(8*zipDeflateCheckpointRecordBytes)))
	if slots < 2 {
		return nil
	}
	// Round upward without adding to size, which may approach MaxInt64.
	const mib = int64(1 << 20)
	units := size / (int64(slots) * mib)
	if size%(int64(slots)*mib) != 0 {
		units++
	}
	stride := max(zipDeflateCheckpointMinStride, units*mib)
	policy := fmt.Sprintf("%s\x00%d\x00%d\x00%d\x00%d", key, size, compressed, stride, slots)
	key = "zip-deflate-checkpoint-v1:" + cache.StableDigest("zip-deflate-checkpoint-v1", policy)
	return &zipDeflateCheckpoints{cache: cache, key: key, identity: sha256.Sum256([]byte(key)), size: size, compressed: compressed, stride: stride, slots: slots}
}

func (s *zipDeflateCheckpoints) nearest(ctx context.Context, target, after int64) *zipDeflateCheckpoint {
	if s == nil || ctx.Err() != nil || target < s.stride || target <= after {
		return nil
	}
	bucket := int(min(int64(s.slots), target/s.stride))
	var encoded []byte
	for ; bucket > 0; bucket-- {
		if ctx.Err() != nil {
			return nil
		}
		key := s.recordKey(bucket)
		handle, err := s.cache.OpenCached(key, zipDeflateCheckpointRecordBytes)
		if err != nil || handle == nil {
			continue
		}
		if encoded == nil {
			encoded = make([]byte, zipDeflateCheckpointRecordBytes)
		}
		n, readErr := handle.ReadAt(encoded, 0)
		closeErr := handle.Close()
		if ctx.Err() != nil {
			return nil
		}
		var point *zipDeflateCheckpoint
		if n == len(encoded) && (readErr == nil || readErr == io.EOF) && closeErr == nil {
			point = s.decode(encoded, bucket)
		}
		if point == nil {
			// The handle is closed before removal. A concurrent pin/fill can
			// retain garbage, but its invalid record never becomes a hit.
			_ = s.cache.Remove(key)
			continue
		}
		if point.OutputOffset <= after {
			break
		}
		if point.OutputOffset <= target {
			return point
		}
	}
	return nil
}

func (s *zipDeflateCheckpoints) hooks(ctx context.Context) zipDeflateCheckpointHooks {
	if s == nil {
		return zipDeflateCheckpointHooks{}
	}
	var mu sync.Mutex
	finished := false
	pending := make(map[int]zipDeflateCheckpoint)
	discard := func() {
		mu.Lock()
		finished = true
		pending = nil
		mu.Unlock()
	}
	return zipDeflateCheckpointHooks{
		Want: func(output int64) bool {
			bucket := s.bucket(output)
			if bucket == 0 || ctx.Err() != nil {
				return false
			}
			mu.Lock()
			defer mu.Unlock()
			_, exists := pending[bucket]
			return !finished && !exists
		},
		Save: func(point zipDeflateCheckpoint) {
			bucket := s.bucket(point.OutputOffset)
			if bucket == 0 || !s.valid(point) || ctx.Err() != nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if _, exists := pending[bucket]; finished || exists {
				return
			}
			point.Window = append([]byte(nil), point.Window...)
			pending[bucket] = point
		},
		Valid: func() {
			mu.Lock()
			if finished {
				mu.Unlock()
				return
			}
			finished = true
			verified := pending
			pending = nil
			mu.Unlock()
			// Codec EOF plus the member's complete CRC authorizes publication.
			// Cache failure/cancellation only loses optional acceleration. Any
			// records already published here are independently CRC-verified.
			for bucket := 1; bucket <= s.slots; bucket++ {
				if ctx.Err() != nil {
					return
				}
				if point, exists := verified[bucket]; exists {
					_ = s.cache.Store(ctx, s.recordKey(bucket), s.encode(point))
					delete(verified, bucket)
				}
			}
		},
		Invalid: discard,
		Close:   discard,
	}
}

func (s *zipDeflateCheckpoints) bucket(output int64) int {
	if output < s.stride || output >= s.size {
		return 0
	}
	bucket := output / s.stride
	if bucket < 1 || bucket > int64(s.slots) {
		return 0
	}
	return int(bucket)
}

func (s *zipDeflateCheckpoints) valid(point zipDeflateCheckpoint) bool {
	return s.bucket(point.OutputOffset) != 0 && point.InputOffset >= 0 && point.InputOffset < s.compressed && point.BitOffset <= 7 && len(point.Window) == zipDeflateCheckpointWindowBytes
}

func (s *zipDeflateCheckpoints) recordKey(bucket int) string {
	return fmt.Sprintf("%s:slot:%d", s.key, bucket)
}

func (s *zipDeflateCheckpoints) encode(point zipDeflateCheckpoint) []byte {
	data := make([]byte, zipDeflateCheckpointRecordBytes)
	copy(data[:8], "ZIPDCP01")
	binary.BigEndian.PutUint32(data[8:12], 1)
	binary.BigEndian.PutUint32(data[12:16], uint32(len(point.Window)))
	binary.BigEndian.PutUint64(data[16:24], uint64(s.size))
	binary.BigEndian.PutUint64(data[24:32], uint64(s.compressed))
	binary.BigEndian.PutUint64(data[32:40], uint64(point.OutputOffset))
	binary.BigEndian.PutUint64(data[40:48], uint64(point.InputOffset))
	binary.BigEndian.PutUint32(data[48:52], point.CRC)
	data[52] = point.BitOffset
	copy(data[64:96], s.identity[:])
	copy(data[96:], point.Window)
	digest := sha256.Sum256(data[:len(data)-sha256.Size])
	copy(data[len(data)-sha256.Size:], digest[:])
	return data
}

func (s *zipDeflateCheckpoints) decode(data []byte, bucket int) *zipDeflateCheckpoint {
	if len(data) != zipDeflateCheckpointRecordBytes || string(data[:8]) != "ZIPDCP01" || binary.BigEndian.Uint32(data[8:12]) != 1 || binary.BigEndian.Uint32(data[12:16]) != zipDeflateCheckpointWindowBytes || binary.BigEndian.Uint64(data[16:24]) != uint64(s.size) || binary.BigEndian.Uint64(data[24:32]) != uint64(s.compressed) || !bytes.Equal(data[64:96], s.identity[:]) {
		return nil
	}
	digest := sha256.Sum256(data[:len(data)-sha256.Size])
	if !bytes.Equal(data[len(data)-sha256.Size:], digest[:]) {
		return nil
	}
	point := zipDeflateCheckpoint{OutputOffset: int64(binary.BigEndian.Uint64(data[32:40])), InputOffset: int64(binary.BigEndian.Uint64(data[40:48])), BitOffset: data[52], CRC: binary.BigEndian.Uint32(data[48:52])}
	if s.bucket(point.OutputOffset) != bucket {
		return nil
	}
	// Check lengths and bounds before allocating the returned owned window.
	point.Window = data[96 : 96+zipDeflateCheckpointWindowBytes]
	if !s.valid(point) {
		return nil
	}
	point.Window = append([]byte(nil), point.Window...)
	return &point
}
