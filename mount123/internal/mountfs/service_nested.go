package mountfs

import (
	"context"
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
func (s *Service) outerArchiveIndex(ctx context.Context, fileID int64) (*zipIndex, *archiveDescriptor, *storage.Remote, error) {
	file, err := s.File(ctx, fileID)
	if err != nil {
		return nil, nil, nil, err
	}
	if file.IsDir {
		return nil, nil, nil, syscall.EISDIR
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
		return nil, nil, nil, syscall.ENOTSUP
	}
	source, err := s.tree.cloudFileSource(ctx, file, s.tree.opts.SourceTTL)
	if err != nil {
		return nil, nil, nil, err
	}
	var idx *zipIndex
	if kind == ".zip" {
		idx, err = s.tree.getZIP(ctx, source, archive.size, archive)
	} else {
		password, passwordErr := s.tree.otherPassword(ctx, archive)
		if passwordErr != nil {
			return nil, nil, nil, passwordErr
		}
		idx, err = s.tree.otherIndex(ctx, source, archive, password)
		clear(password)
	}
	if err != nil {
		return nil, nil, nil, err
	}
	return idx, archive, source, nil
}

// nestedArchiveFrom opens an archive member as a nested 7z (including split
// .7z.001 volumes). It reuses the mount's lazy member mapping so the inner
// archive is never fully decompressed.
func (s *Service) nestedArchiveFrom(ctx context.Context, source *storage.Remote, outer *archiveDescriptor, idx *zipIndex, memberPath string, m *member) (*nestedArchive, error) {
	if archiveKind(m.name) != ".7z" {
		return nil, syscall.ENOTSUP
	}
	if err := defaultNestedArchivePolicy.allow(1, int64(m.size)); err != nil {
		return nil, err
	}
	reader, size, identity, err := s.tree.archiveSource(ctx, source, outer)
	if err != nil {
		return nil, err
	}
	password, err := s.tree.otherPassword(ctx, outer)
	if err != nil {
		return nil, err
	}
	defer clear(password)

	parts := []*member{m}
	partNames := []string{memberPath}
	if strings.HasSuffix(strings.ToLower(m.name), ".7z.001") {
		stem := m.name[:len(m.name)-4]
		idx.mu.RLock()
		for name, candidate := range idx.members {
			if strings.HasPrefix(name, stem+".") && len(name) == len(stem)+4 && name[len(name)-3:] >= "002" {
				partNames = append(partNames, name)
				parts = append(parts, candidate)
			}
		}
		idx.mu.RUnlock()
		order := make([]int, len(partNames))
		for i := range order {
			order[i] = i
		}
		sort.Slice(order, func(i, j int) bool { return partNames[order[i]] < partNames[order[j]] })
		sortedParts := make([]*member, len(parts))
		sortedNames := make([]string, len(partNames))
		for i, j := range order {
			sortedParts[i], sortedNames[i] = parts[j], partNames[j]
		}
		parts, partNames = sortedParts, sortedNames
		for i, name := range partNames {
			want := fmt.Sprintf("%s.%03d", stem, i+1)
			if name != want {
				return nil, syscall.EIO
			}
		}
	}

	readers := make([]io.ReaderAt, 0, len(parts))
	handles := make([]*storage.Handle, 0, len(parts))
	sizes := make([]int64, 0, len(parts))
	partSize := int64(0)
	for _, part := range parts {
		if direct, ok, directErr := nestedCopyMemberReader(reader, size, part, part.ordinal, password); directErr != nil {
			for _, old := range handles {
				_ = old.Close()
			}
			return nil, directErr
		} else if ok {
			readers = append(readers, direct)
			sizes = append(sizes, int64(part.size))
			partSize += int64(part.size)
			continue
		}
		key := s.tree.diskCacheScope() + ":" + identity + ":nested-member:" + part.name + fmt.Sprintf(":%d:%08x:%s", part.size, part.crc, s.tree.passwordTag(outer, password))
		handle, acquireErr := s.tree.cache.Acquire(ctx, key, int64(part.size), func(fillCtx context.Context, w io.Writer) error {
			return extractArchiveMember(fillCtx, reader, size, part, password, w)
		})
		if acquireErr != nil {
			for _, old := range handles {
				_ = old.Close()
			}
			return nil, acquireErr
		}
		handles = append(handles, handle)
		readers = append(readers, handle)
		sizes = append(sizes, int64(part.size))
		partSize += int64(part.size)
	}
	cached := readers[0]
	if len(readers) > 1 {
		cached = &nestedConcatReader{parts: readers, sizes: sizes, total: partSize}
	}
	innerMembers, err := indexNested7z(ctx, cached, partSize, password, defaultNestedArchivePolicy)
	if err != nil {
		for _, old := range handles {
			_ = old.Close()
		}
		return nil, err
	}
	innerIndex, err := nestedIndexFromMembers(innerMembers, defaultNestedArchivePolicy)
	if err != nil {
		for _, old := range handles {
			_ = old.Close()
		}
		return nil, err
	}
	return &nestedArchive{reader: cached, size: partSize, format: ".7z", index: innerIndex, password: append([]byte(nil), password...), handles: handles}, nil
}

func (s *Service) openNestedMember(ctx context.Context, na *nestedArchive, m *member) (io.ReaderAt, int64, error) {
	key := s.tree.diskCacheScope() + ":nested-content:" + m.name + fmt.Sprintf(":%d:%08x", m.size, m.crc)
	handle, err := s.tree.cache.Acquire(ctx, key, int64(m.size), func(fillCtx context.Context, w io.Writer) error {
		return archiveReadError(fillCtx, extractArchiveMember(fillCtx, na.reader, na.size, m, na.password, w), na.password)
	})
	if err != nil {
		return nil, 0, err
	}
	return handle, int64(m.size), nil
}
