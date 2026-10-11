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

// buildNestedArchive contains the shared expansion path used by the web and
// FUSE front ends. It only performs work after an archive member has been
// explicitly opened; callers must keep the returned archive alive.
func (t *Tree) buildNestedArchive(ctx context.Context, source *storage.Remote, outer *archiveDescriptor, outerIndex *zipIndex, memberPath string, m *member) (*nestedArchive, error) {
	kind := archiveKind(m.name)
	if kind != ".7z" && kind != ".rar" && kind != ".zip" {
		return nil, syscall.ENOTSUP
	}
	if err := defaultNestedArchivePolicy.allow(1, int64(m.size)); err != nil {
		return nil, err
	}
	reader, size, identity, err := t.archiveSource(ctx, source, outer)
	if err != nil {
		return nil, err
	}
	outerPassword, err := t.otherPassword(ctx, outer)
	if err != nil {
		return nil, err
	}
	defer clear(outerPassword)
	innerPassword := append([]byte(nil), outerPassword...)
	defer clear(innerPassword)
	innerArchive := &archiveDescriptor{id: outer.id, parentID: outer.parentID, name: m.name, version: outer.version, size: int64(m.size), format: kind}
	if candidate, candidateErr := t.otherPassword(ctx, innerArchive); candidateErr == nil && len(candidate) > 0 {
		clear(innerPassword)
		innerPassword = candidate
	} else {
		clear(candidate)
	}

	parts := []*member{m}
	partNames := []string{memberPath}
	if strings.HasSuffix(strings.ToLower(m.name), ".7z.001") {
		if outerIndex == nil {
			outerIndex, err = t.otherIndex(ctx, source, outer, outerPassword)
			if err != nil {
				return nil, err
			}
		}
		stem := m.name[:len(m.name)-4]
		outerIndex.mu.RLock()
		for name, candidate := range outerIndex.members {
			if strings.HasPrefix(name, stem+".") && len(name) == len(stem)+4 && name[len(name)-3:] >= "002" {
				partNames = append(partNames, name)
				parts = append(parts, candidate)
			}
		}
		outerIndex.mu.RUnlock()
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
			if want := fmt.Sprintf("%s.%03d", stem, i+1); name != want {
				return nil, syscall.EIO
			}
		}
	}

	readers := make([]io.ReaderAt, 0, len(parts))
	lazyReaders := make([]*nestedMemberReader, 0, len(parts))
	sizes := make([]int64, 0, len(parts))
	partSize := int64(0)
	for _, part := range parts {
		if direct, ok, directErr := nestedCopyMemberReader(reader, size, part, part.ordinal, outerPassword); directErr != nil {
			return nil, directErr
		} else if ok {
			readers = append(readers, direct)
			sizes = append(sizes, int64(part.size))
			partSize += int64(part.size)
			continue
		}
		lr, err := t.newNestedMemberReader(ctx, source, outer, identity, part, outerPassword)
		if err != nil {
			for _, r := range lazyReaders {
				_ = r.Close()
			}
			return nil, err
		}
		lazyReaders = append(lazyReaders, lr)
		readers = append(readers, lr)
		sizes = append(sizes, int64(part.size))
		partSize += int64(part.size)
	}
	var cached io.ReaderAt = readers[0]
	if len(readers) > 1 {
		cached = &nestedConcatReader{parts: readers, sizes: sizes, total: partSize}
	}
	var innerMembers []archiveMember
	switch kind {
	case ".rar":
		innerMembers, err = indexNestedRAR(ctx, cached, partSize, innerPassword, defaultNestedArchivePolicy)
	case ".zip":
		innerMembers, err = indexNestedZIP(cached, partSize, defaultNestedArchivePolicy)
	default:
		innerMembers, err = indexNested7z(ctx, cached, partSize, innerPassword, defaultNestedArchivePolicy)
	}
	if err != nil {
		for _, r := range lazyReaders {
			_ = r.Close()
		}
		return nil, err
	}
	innerIndex, err := nestedIndexFromMembers(innerMembers, defaultNestedArchivePolicy)
	if err != nil {
		for _, r := range lazyReaders {
			_ = r.Close()
		}
		return nil, err
	}
	return &nestedArchive{
		reader: cached, size: partSize, format: kind, index: innerIndex,
		password:    append([]byte(nil), innerPassword...),
		cachePrefix: t.diskCacheScope() + ":" + identity + ":nested-content:" + t.passwordTag(innerArchive, innerPassword),
		lazyReaders: lazyReaders,
	}, nil
}
