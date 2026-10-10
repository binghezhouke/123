package mountfs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"
	"syscall"

	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/nwaples/rardecode/v2"
)

const archiveIndexFormatVersion = 1

type boundedJSONBuffer struct {
	bytes.Buffer
	limit int
}

func (b *boundedJSONBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, syscall.EFBIG
	}
	return b.Buffer.Write(p)
}

type archiveIndexDTO struct {
	Version        int                    `json:"version"`
	Kind           string                 `json:"kind"`
	ArchiveSize    int64                  `json:"archive_size"`
	IdentityDigest string                 `json:"identity_digest"`
	Entries        []archiveIndexEntryDTO `json:"entries"`
	Complete       bool                   `json:"complete"`
	ScanOffset     int64                  `json:"scan_offset,omitempty"`
}

type archiveIndexEntryDTO struct {
	Path         string                   `json:"path"`
	Name         string                   `json:"name,omitempty"`
	Directory    bool                     `json:"directory,omitempty"`
	Ordinal      int                      `json:"ordinal,omitempty"`
	Size         uint64                   `json:"size,omitempty"`
	Compressed   uint64                   `json:"compressed,omitempty"`
	CRC          uint32                   `json:"crc,omitempty"`
	Method       uint16                   `json:"method,omitempty"`
	Flags        uint16                   `json:"flags,omitempty"`
	ModifiedTime uint16                   `json:"modified_time,omitempty"`
	HeaderOffset int64                    `json:"header_offset,omitempty"`
	AES          *archiveAESDTO           `json:"aes,omitempty"`
	RARLocator   *rardecode.LocatorRecord `json:"rar_locator,omitempty"`
	SevenStream  *sevenStreamLocation     `json:"seven_stream,omitempty"`
}

type archiveAESDTO struct {
	Version  uint16 `json:"version"`
	Strength byte   `json:"strength"`
	Method   uint16 `json:"method"`
}

func (t *Tree) archiveIndexCacheKey(kind, identity string, a *archiveDescriptor, password []byte) string {
	if t.cache == nil {
		return ""
	}
	passwordIdentity := t.passwordTag(a, password)
	return "archive-index-dto-v1:" + t.cache.StableDigest("archive-index-key-v1", t.cacheScope+"\x00"+kind+"\x00"+identity+"\x00"+passwordIdentity)
}

func (t *Tree) loadPersistentArchiveIndex(ctx context.Context, cacheKey, kind string, size int64, identity string) (*zipIndex, bool) {
	if t.cache == nil || cacheKey == "" {
		return nil, false
	}
	h, err := t.cache.OpenArchiveIndex(cacheKey)
	if err != nil {
		return nil, false
	}
	if h.Size() <= 0 || h.Size() > t.opts.MetadataBytes || h.Size() > math.MaxInt {
		_ = h.Close()
		_ = t.cache.Remove(cacheKey)
		return nil, false
	}
	data := make([]byte, int(h.Size()))
	if _, err = h.ReadAt(data, 0); err != nil {
		_ = h.Close()
		_ = t.cache.Remove(cacheKey)
		return nil, false
	}
	_ = h.Close()
	var dto archiveIndexDTO
	if json.Unmarshal(data, &dto) != nil || dto.Version != archiveIndexFormatVersion || dto.Kind != kind || dto.ArchiveSize != size || dto.IdentityDigest != identity {
		_ = t.cache.Remove(cacheKey)
		return nil, false
	}
	idx, err := t.indexFromDTO(ctx, kind, dto, size)
	if err != nil {
		_ = t.cache.Remove(cacheKey)
		return nil, false
	}
	return idx, true
}

// loadArchiveCheckpoint restores a previously verified prefix.  It is kept
// separate from the completed index so an interrupted scan can never be
// mistaken for a complete directory.
func (t *Tree) loadArchiveCheckpoint(ctx context.Context, cacheKey, kind string, size int64, identity string) (*zipIndex, bool) {
	idx, ok := t.loadPersistentArchiveIndex(ctx, cacheKey+":checkpoint", kind, size, identity)
	if !ok || idx.complete {
		return nil, false
	}
	return idx, true
}

func (t *Tree) persistArchiveIndex(ctx context.Context, cacheKey, kind string, size int64, identity string, idx *zipIndex) {
	if t.cache == nil || cacheKey == "" || idx == nil {
		return
	}
	dto := archiveIndexDTO{Version: archiveIndexFormatVersion, Kind: kind, ArchiveSize: size, IdentityDigest: identity, Complete: true, ScanOffset: size}
	var walk func(*zipDir, string)
	walk = func(dir *zipDir, prefix string) {
		keys := make([]string, 0, len(dir.dirs))
		for name := range dir.dirs {
			keys = append(keys, name)
		}
		sort.Strings(keys)
		for _, name := range keys {
			path := name
			if prefix != "" {
				path = prefix + "/" + name
			}
			dto.Entries = append(dto.Entries, archiveIndexEntryDTO{Path: path, Directory: true})
			walk(dir.dirs[name], path)
		}
		keys = keys[:0]
		for name := range dir.files {
			keys = append(keys, name)
		}
		sort.Strings(keys)
		for _, key := range keys {
			m := dir.files[key]
			path := key
			if prefix != "" {
				path = prefix + "/" + key
			}
			item := archiveIndexEntryDTO{Path: path, Name: m.name, Ordinal: m.ordinal, Size: m.size, Compressed: m.compressed, CRC: m.crc, Method: m.method, Flags: m.flags, ModifiedTime: m.modifiedTime, HeaderOffset: m.headerOffset, SevenStream: m.sevenStream}
			if m.aes != nil {
				item.AES = &archiveAESDTO{Version: m.aes.version, Strength: m.aes.strength, Method: m.aes.method}
			}
			if m.rarLocator != nil {
				loc := rardecode.ExportLocator(*m.rarLocator)
				item.RARLocator = &loc
			}
			dto.Entries = append(dto.Entries, item)
		}
	}
	idx.mu.RLock()
	complete, scanErr := idx.complete, idx.scanErr
	if complete && scanErr == nil {
		walk(idx.root, "")
	}
	idx.mu.RUnlock()
	if !complete || scanErr != nil {
		return
	}
	var buffer boundedJSONBuffer
	buffer.limit = int(t.opts.MetadataBytes)
	if err := json.NewEncoder(&buffer).Encode(dto); err != nil {
		return
	}
	_ = t.cache.StoreArchiveIndex(ctx, cacheKey, buffer.Bytes())
}

func (t *Tree) persistArchiveCheckpoint(ctx context.Context, cacheKey, kind string, size int64, identity string, idx *zipIndex, offset int64) {
	if t.cache == nil || cacheKey == "" || idx == nil { return }
	idx.mu.RLock()
	dto := archiveIndexDTO{Version: archiveIndexFormatVersion, Kind: kind, ArchiveSize: size, IdentityDigest: identity, Complete: false, ScanOffset: offset}
	var walk func(*zipDir, string)
	walk = func(dir *zipDir, prefix string) { for name, child := range dir.dirs { p:=name; if prefix!="" {p=prefix+"/"+name}; dto.Entries=append(dto.Entries, archiveIndexEntryDTO{Path:p,Directory:true}); walk(child,p) }; for name,m := range dir.files { p:=name; if prefix!="" {p=prefix+"/"+name}; e:=archiveIndexEntryDTO{Path:p,Name:m.name,Ordinal:m.ordinal,Size:m.size,CRC:m.crc}; if m.rarLocator!=nil { l:=rardecode.ExportLocator(*m.rarLocator); e.RARLocator=&l }; dto.Entries=append(dto.Entries,e) } }
	walk(idx.root, ""); idx.mu.RUnlock()
	var b boundedJSONBuffer; b.limit=int(t.opts.MetadataBytes); if json.NewEncoder(&b).Encode(dto)==nil { _ = t.cache.StoreArchiveIndex(ctx, cacheKey+":checkpoint", b.Bytes()) }
}

func (t *Tree) indexFromDTO(ctx context.Context, kind string, dto archiveIndexDTO, archiveSize int64) (*zipIndex, error) {
	if len(dto.Entries) > t.opts.MaxExpandedNodes+t.opts.MaxZIPEntries {
		return nil, syscall.EFBIG
	}
	idx := &zipIndex{root: &zipDir{dirs: map[string]*zipDir{}, files: map[string]*member{}}, members: map[string]*member{}, bytes: 256, complete: dto.Complete, changed: make(chan struct{})}
	if dto.Complete {
		close(idx.changed)
	}
	seen := make(map[string]bool, len(dto.Entries))
	names, nodes := 0, 0
	for _, record := range dto.Entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := strings.TrimSuffix(record.Path, "/")
		if name == "" || seen[name] {
			return nil, errors.New("invalid persisted archive path")
		}
		seen[name] = true
		parts := strings.Split(name, "/")
		if len(parts) > t.opts.MaxDepth {
			return nil, syscall.EFBIG
		}
		dir := idx.root
		for i, part := range parts {
			if !validName(part) {
				return nil, errors.New("unsafe persisted archive path")
			}
			isLeaf := i == len(parts)-1
			names += len(part)
			if names > t.opts.MaxNameBytes {
				return nil, syscall.EFBIG
			}
			if !isLeaf || record.Directory {
				if dir.files[part] != nil {
					return nil, errors.New("conflicting persisted archive paths")
				}
				child := dir.dirs[part]
				if child == nil {
					dir.order = append(dir.order, part)
					child = &zipDir{dirs: map[string]*zipDir{}, files: map[string]*member{}}
					dir.dirs[part] = child
					nodes++
				}
				dir = child
				continue
			}
			if dir.dirs[part] != nil || record.Name == "" || record.Size >= math.MaxInt64 || record.Ordinal < 0 || record.Ordinal >= t.opts.MaxZIPEntries {
				return nil, errors.New("invalid persisted archive member")
			}
			memberFormat := kind
			if kind == ".zip" {
				memberFormat = ""
			}
			m := &member{name: record.Name, format: memberFormat, ordinal: record.Ordinal, headerOffset: record.HeaderOffset, modifiedTime: record.ModifiedTime, method: record.Method, flags: record.Flags, crc: record.CRC, compressed: record.Compressed, size: record.Size, encrypted: record.Flags&1 != 0}
			if kind == ".7z" {
				// Rebuild only old 7z indexes that lack stream locations. ZIP/RAR
				// keep their existing persistent index schema and cache keys.
				if record.Size > 0 && (record.SevenStream == nil || !record.SevenStream.valid(record.Size) || record.SevenStream.Stream >= t.opts.MaxZIPEntries) {
					return nil, errors.New("persisted 7z stream location is missing or invalid")
				}
				m.sevenStream = record.SevenStream
			}
			if kind == ".zip" {
				if record.HeaderOffset < 0 || record.HeaderOffset >= archiveSize || (record.Method != 0 && record.Method != 8 && record.Method != 99) {
					return nil, errors.New("invalid persisted ZIP member location")
				}
				if record.AES != nil {
					m.aes = &aesMemberInfo{version: record.AES.Version, strength: record.AES.Strength, method: record.AES.Method}
				}
			} else if kind == ".rar" {
				if record.RARLocator == nil {
					return nil, errors.New("persisted RAR locator is missing")
				}
				locator, err := rardecode.ImportLocator(*record.RARLocator, archiveSize)
				if err != nil {
					return nil, err
				}
				m.rarLocator = &locator
			}
			dir.order = append(dir.order, part)
			dir.files[part] = m
			idx.members[m.name] = m
			nodes++
			idx.bytes += int64(256 + len(part))
		}
		idx.bytes += int64(384 + len(name))
		if nodes > t.opts.MaxExpandedNodes || idx.bytes > t.opts.MetadataBytes {
			return nil, syscall.EFBIG
		}
	}
	if len(idx.members) > t.opts.MaxZIPEntries {
		return nil, syscall.EFBIG
	}
	return idx, nil
}

func (z *zipIndex) attachZIPReader(source *storage.Remote) {
	reader := &contextZIPReaderAt{source: source, gate: make(chan struct{}, 1)}
	for _, m := range z.members {
		m.reader = reader
	}
}
