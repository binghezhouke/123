package mountfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"syscall"

	"github.com/binghezhouke/123/mount123/internal/storage"
)

// splitArchivePath splits a slash-separated member path into its first
// component and the remainder.
func splitArchivePath(path string) (string, string) {
	path = strings.Trim(path, "/")
	if index := strings.IndexByte(path, '/'); index >= 0 {
		return path[:index], strings.TrimPrefix(path[index+1:], "/")
	}
	return path, ""
}

func archiveEntriesFromIndex(idx *zipIndex, memberPath string) ([]ArchiveEntry, error) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	dir := idx.dirLocked(memberPath)
	if dir == nil {
		return nil, syscall.ENOENT
	}
	names := make([]string, 0, len(dir.dirs)+len(dir.files))
	for name := range dir.dirs {
		names = append(names, name)
	}
	for name := range dir.files {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]ArchiveEntry, 0, len(names))
	for _, name := range names {
		if dir.dirs[name] != nil {
			out = append(out, ArchiveEntry{Name: name, IsDir: true})
			continue
		}
		if m := dir.files[name]; m != nil {
			out = append(out, ArchiveEntry{Name: name, Size: m.size, Encrypted: m.encrypted})
		}
	}
	return out, nil
}

// outerArchiveIndex resolves one cloud archive into its parsed index.
func (s *Service) outerArchiveIndex(ctx context.Context, fileID int64) (*zipIndex, *archiveDescriptor, *storage.Remote, []byte, error) {
	file, err := s.File(ctx, fileID)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if file.IsDir {
		return nil, nil, nil, nil, syscall.EISDIR
	}
	archive := &archiveDescriptor{id: file.ID, parentID: file.ParentID, name: file.Name, version: file.Version, size: file.Size}
	kind := archive.kind()
	if kind == "" {
		// A renamed archive has no extension: fall back to the probe record.
		if record, probeErr := s.tree.loadProbeRecord(ctx, file.ParentID); probeErr == nil {
			for _, result := range record.Results {
				if result.ID == file.ID && result.State == "detected" && result.Size == file.Size && result.Kind != "" {
					archive.format = result.Kind
					kind = result.Kind
					break
				}
			}
		}
	}
	if kind == "" {
		return nil, nil, nil, nil, syscall.ENOTSUP
	}
	source, err := s.tree.cloudFileSource(ctx, file, s.tree.opts.SourceTTL)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	var idx *zipIndex
	var password []byte
	if kind == ".zip" {
		idx, err = s.tree.getZIP(ctx, source, archive.size, archive)
	} else {
		// Try without a password first so unencrypted archives skip password
		// discovery entirely. Only fetch a password when the decoder says one
		// is required.
		idx, err = s.tree.otherIndex(ctx, source, archive, nil)
		if err != nil && (needsPassword(err) || errors.Is(err, syscall.EACCES)) {
			password, err = s.tree.otherPassword(ctx, archive)
			if err != nil {
				return nil, nil, nil, nil, err
			}
			idx, err = s.tree.otherIndex(ctx, source, archive, password)
		}
	}
	if err != nil {
		clear(password)
		return nil, nil, nil, nil, err
	}
	return idx, archive, source, password, nil
}

// nestedArchiveFrom opens an archive member as a nested ZIP, 7z, or RAR
// (including split .7z.001 volumes). It delegates to the same lazy expansion
// path used by FUSE so the inner archive is never fully decompressed.
func (s *Service) nestedArchiveFrom(ctx context.Context, source *storage.Remote, outer *archiveDescriptor, idx *zipIndex, memberPath string, m *member) (*nestedArchive, error) {
	return s.tree.buildNestedArchive(ctx, source, outer, idx, memberPath, m)
}

func (s *Service) openNestedMember(ctx context.Context, na *nestedArchive, m *member) (io.ReaderAt, int64, error) {
	key := na.cachePrefix + ":" + m.name + fmt.Sprintf(":%d:%08x", m.size, m.crc)
	handle, err := s.acquireMemberReader(ctx, key, int64(m.size), m.format == ".zip", func(fillCtx context.Context, w io.Writer) error {
		return archiveReadError(fillCtx, extractArchiveMember(fillCtx, na.reader, na.size, m, na.password, w), na.password)
	})
	if err != nil {
		_ = na.Close()
		return nil, 0, err
	}
	if growing, ok := handle.(*serviceGrowingReader); ok {
		// The producer still owns these nested readers after this request
		// returns or closes its handle. Release only after the fill stops.
		go func() {
			<-growing.handle.Done()
			_ = na.Close()
		}()
	} else {
		_ = na.Close()
	}
	return handle, int64(m.size), nil
}
