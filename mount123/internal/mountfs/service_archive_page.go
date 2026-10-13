package mountfs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// ArchivePage is one bounded, consistent snapshot of a directory. Complete
// describes the index scan; NextCursor independently describes remaining pages.
type ArchivePage struct {
	Items      []ArchiveEntry
	NextCursor string
	Total      int
	Complete   bool
}

type archiveCursor struct {
	Index  string `json:"index"`
	Scope  string `json:"scope"`
	Offset int    `json:"offset"`
	Scan   bool   `json:"scan"`
}

var archiveListingSequence atomic.Uint64
var archiveListingEpoch = strconv.FormatInt(time.Now().UnixNano(), 36)

func readArchiveCursor(value, index, scope string, complete bool, total int) archiveCursor {
	c := archiveCursor{Index: index, Scope: scope, Scan: !complete}
	if complete {
		c.Index = "complete"
	}
	var decoded archiveCursor
	if len(value) <= 8192 {
		data, err := base64.RawURLEncoding.DecodeString(value)
		if err == nil && json.Unmarshal(data, &decoded) == nil && decoded.Scope == scope && decoded.Offset >= 0 && decoded.Offset <= total && ((decoded.Scan && decoded.Index == index) || (!decoded.Scan && complete && decoded.Index == "complete")) {
			return decoded
		}
	}
	// A rebuilt index may have a different discovery order. Restart enumeration
	// rather than skip data; the client deduplicates entries already displayed.
	return c
}

func nextArchiveCursor(c archiveCursor, end, total int, complete bool) string {
	if complete && end >= total {
		return ""
	}
	c.Offset = end
	data, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(data)
}

// ListArchivePage keeps discovery order stable for the life of a progressive
// listing. Completed listings opened afresh retain alphabetical ordering.
func (s *Service) ListArchivePage(ctx context.Context, fileID int64, memberPath, cursor string, limit int) (ArchivePage, error) {
	if limit <= 0 || limit > 1000 {
		return ArchivePage{}, syscall.EINVAL
	}
	return s.listArchivePage(ctx, fileID, memberPath, cursor, limit, true)
}

func (s *Service) listArchivePage(ctx context.Context, fileID int64, memberPath, cursor string, limit int, progressive bool) (ArchivePage, error) {
	file, err := s.File(ctx, fileID)
	if err != nil {
		return ArchivePage{}, err
	}
	scope := fmt.Sprintf("%d:%s:%d:%s", fileID, file.Version, file.Size, memberPath)
	if strings.EqualFold(path.Ext(file.Name), ".iso") {
		entries, err := s.ListISO(ctx, fileID, memberPath)
		if err != nil {
			return ArchivePage{}, err
		}
		c := readArchiveCursor(cursor, fmt.Sprintf("iso:%s:%d", file.Version, file.Size), scope, true, len(entries))
		end := len(entries)
		if limit > 0 {
			end = min(end, c.Offset+limit)
		}
		return ArchivePage{Items: entries[c.Offset:end], Total: len(entries), Complete: true, NextCursor: nextArchiveCursor(c, end, len(entries), true)}, nil
	}
	idx, archive, source, password, err := s.outerArchiveIndexMode(ctx, fileID, progressive)
	defer clear(password)
	if err != nil {
		return ArchivePage{}, err
	}
	// Bind the cursor to the metadata used to build this index, even if the
	// detail cache changed since the initial ISO format check above.
	scope = fmt.Sprintf("%d:%s:%d:%s", fileID, archive.version, archive.size, memberPath)
	first, rest := splitArchivePath(memberPath)
	if first != "" {
		idx.mu.RLock()
		m := idx.members[first]
		idx.mu.RUnlock()
		if m != nil && archiveKind(m.name) != "" {
			nested, err := s.nestedArchiveFrom(ctx, source, archive, idx, first, m)
			if err != nil {
				return ArchivePage{}, err
			}
			defer nested.Close()
			idx, memberPath = nested.index, rest
		}
	}
	return archivePageFromIndex(idx, memberPath, scope, cursor, limit)
}

func archivePageFromIndex(idx *zipIndex, memberPath, scope, cursor string, limit int) (ArchivePage, error) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.scanErr != nil {
		return ArchivePage{}, idx.scanErr
	}
	if idx.listingID == "" {
		idx.listingID = fmt.Sprintf("%s:%d", archiveListingEpoch, archiveListingSequence.Add(1))
	}
	dir := idx.dirLocked(memberPath)
	if dir == nil {
		if idx.complete {
			return ArchivePage{}, syscall.ENOENT
		}
		c := readArchiveCursor("", idx.listingID, scope, false, 0)
		return ArchivePage{Items: []ArchiveEntry{}, NextCursor: nextArchiveCursor(c, 0, 0, false)}, nil
	}
	total := len(dir.dirs) + len(dir.files)
	c := readArchiveCursor(cursor, idx.listingID, scope, idx.complete, total)
	names := dir.order
	if !c.Scan {
		if dir.sortedNames == nil {
			dir.sortedNames = make([]string, 0, total)
			for name := range dir.dirs {
				dir.sortedNames = append(dir.sortedNames, name)
			}
			for name := range dir.files {
				dir.sortedNames = append(dir.sortedNames, name)
			}
			sort.Strings(dir.sortedNames)
		}
		names = dir.sortedNames
	}
	end := len(names)
	if limit > 0 {
		end = min(end, c.Offset+limit)
	}
	items := make([]ArchiveEntry, 0, end-c.Offset)
	for _, name := range names[c.Offset:end] {
		if dir.dirs[name] != nil {
			items = append(items, ArchiveEntry{Name: name, IsDir: true})
		} else if m := dir.files[name]; m != nil {
			items = append(items, ArchiveEntry{Name: name, Size: m.size, Encrypted: m.encrypted})
		}
	}
	return ArchivePage{Items: items, NextCursor: nextArchiveCursor(c, end, total, idx.complete), Total: total, Complete: idx.complete}, nil
}
