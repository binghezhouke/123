package mountfs

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"syscall"
)

const maxArchiveVolumes = 1000

type archiveVolumeCandidate struct {
	name      string
	size      int64
	directory bool
}

// byteSplitStem recognizes only the entry volume; later numbered parts remain
// ordinary files. Case is significant in the stem and insensitive in the format.
func byteSplitStem(name string) (string, bool) {
	lower := strings.ToLower(name)
	if strings.HasSuffix(lower, ".7z.001") || strings.HasSuffix(lower, ".zip.001") {
		return name[:len(name)-4], true
	}
	return "", false
}

func archivePasswordName(name string) string {
	if stem, ok := byteSplitStem(name); ok {
		return stem + ".pwd"
	}
	return name + ".pwd"
}

func volumeNumber(token string, padding int) (int, bool, error) {
	if token == "" || strings.Trim(token, "0123456789") != "" {
		return 0, false, nil
	}
	number, err := strconv.Atoi(token)
	if err != nil || number > maxArchiveVolumes {
		return 0, true, syscall.EFBIG
	}
	if number < 1 || token != fmt.Sprintf("%0*d", padding, number) {
		return 0, true, syscall.EIO
	}
	return number, true, nil
}

// discoverArchiveVolumes is shared by cloud and nested inputs. It never reads
// payloads, and returns indices into the caller's frozen candidate snapshot.
func discoverArchiveVolumes(entry string, kind archiveSplitKind, lastDisk uint32, candidates []archiveVolumeCandidate) ([]int, []int64, int64, error) {
	stem, split := byteSplitStem(entry)
	if kind == archiveByteSplit && !split {
		return nil, nil, 0, syscall.EOPNOTSUPP
	}
	if kind == archiveZIPDisks {
		if !strings.HasSuffix(strings.ToLower(entry), ".zip") || lastDisk == 0 {
			return nil, nil, 0, syscall.EIO
		}
		if lastDisk >= maxArchiveVolumes {
			return nil, nil, 0, syscall.EFBIG
		}
		stem = entry[:len(entry)-4]
	}
	parts := map[int]int{}
	final := -1
	for i, candidate := range candidates {
		if kind == archiveZIPDisks && candidate.name == entry {
			if final >= 0 {
				return nil, nil, 0, syscall.EIO
			}
			final = i
			continue
		}
		token := ""
		padding := 3
		switch kind {
		case archiveByteSplit:
			if !strings.HasPrefix(candidate.name, stem+".") {
				continue
			}
			token = candidate.name[len(stem)+1:]
		case archiveZIPDisks:
			if !strings.HasPrefix(candidate.name, stem+".") {
				continue
			}
			suffix := candidate.name[len(stem)+1:]
			if len(suffix) < 2 || (suffix[0] != 'z' && suffix[0] != 'Z') {
				continue
			}
			token, padding = suffix[1:], 2
		default:
			if candidate.name != entry {
				continue
			}
			if final >= 0 {
				return nil, nil, 0, syscall.EIO
			}
			final = i
			continue
		}
		// Unreferenced disk suffixes do not belong to the declared ZIP set.
		if kind == archiveZIPDisks && token != "" && strings.Trim(token, "0123456789") == "" {
			n, e := strconv.ParseUint(token, 10, 64)
			if e != nil || n > uint64(lastDisk) {
				continue
			}
		}
		number, numeric, err := volumeNumber(token, padding)
		if err != nil {
			return nil, nil, 0, err
		}
		if !numeric {
			continue
		}
		if _, exists := parts[number]; exists {
			return nil, nil, 0, syscall.EIO
		}
		parts[number] = i
	}
	count := len(parts)
	if kind == archiveZIPDisks {
		count = int(lastDisk)
	}
	indices := make([]int, 0, count+1)
	for number := 1; number <= count; number++ {
		index, ok := parts[number]
		if !ok {
			return nil, nil, 0, syscall.EIO
		}
		indices = append(indices, index)
	}
	if kind != archiveByteSplit {
		if final < 0 {
			return nil, nil, 0, syscall.EIO
		}
		indices = append(indices, final)
	}
	if len(indices) == 0 {
		return nil, nil, 0, syscall.EIO
	}
	sizes := make([]int64, len(indices))
	var total int64
	for i, index := range indices {
		part := candidates[index]
		if part.directory || part.size <= 0 {
			return nil, nil, 0, syscall.EIO
		}
		if part.size > math.MaxInt64-total {
			return nil, nil, 0, syscall.EFBIG
		}
		total += part.size
		sizes[i] = part.size
	}
	return indices, sizes, total, nil
}
