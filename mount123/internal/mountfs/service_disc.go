package mountfs

import (
	"context"
	"io"
	"path"
	"syscall"

	"github.com/binghezhouke/123/mount123/internal/discimage"
	"github.com/binghezhouke/123/mount123/internal/storage"
)

func (s *Service) isoSource(ctx context.Context, fileID int64) (*storage.Remote, int64, error) {
	file, err := s.tree.archiveFileMetadata(ctx, fileID)
	if err != nil {
		return nil, 0, err
	}
	if file.IsDir {
		return nil, 0, syscall.EISDIR
	}
	source, err := s.tree.cloudArchiveSource(ctx, &file)
	if err != nil {
		return nil, 0, err
	}
	return source, file.Size, nil
}

func (s *Service) openISO(ctx context.Context, fileID int64) (*discimage.FS, *storage.Remote, int64, error) {
	source, size, err := s.isoSource(ctx, fileID)
	if err != nil {
		return nil, nil, 0, err
	}
	image, err := discimage.Open(newDiscContextReaderAt(ctx, source, size), size)
	if err != nil {
		return nil, nil, 0, err
	}
	return image, source, size, nil
}

func (s *Service) ListISO(ctx context.Context, fileID int64, memberPath string) ([]ArchiveEntry, error) {
	if memberPath == "" {
		memberPath = "."
	}
	if !validDiscPath(memberPath, s.tree.opts.MaxDepth) {
		return nil, syscall.EINVAL
	}
	file, err := s.tree.archiveFileMetadata(ctx, fileID)
	if err != nil {
		return nil, err
	}
	if file.IsDir {
		return nil, syscall.EISDIR
	}
	source, size, err := s.isoSource(ctx, fileID)
	if err != nil {
		return nil, err
	}
	node := &Node{tree: s.tree, item: &entry{source: source, archiveSize: size,
		archive: &archiveDescriptor{id: file.ID, parentID: file.ParentID, name: file.Name, version: file.Version, size: size},
		disc:    &discEntry{path: memberPath}, directory: true}}
	directory, err := node.discDirectory(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]ArchiveEntry, 0, len(directory.names))
	for _, name := range directory.names {
		child := directory.entries[name]
		result = append(result, ArchiveEntry{Name: name, Size: child.disc.size, IsDir: child.directory})
	}
	return result, nil
}

func (s *Service) OpenISOMember(ctx context.Context, fileID int64, memberPath string) (io.ReaderAt, int64, error) {
	if memberPath != "" && !validDiscPath(memberPath, s.tree.opts.MaxDepth) {
		return nil, 0, syscall.EINVAL
	}
	if memberPath == "." || memberPath == "" {
		return nil, 0, syscall.EISDIR
	}
	image, _, _, err := s.openISO(ctx, fileID)
	if err != nil {
		return nil, 0, err
	}
	reader, err := image.OpenFile(memberPath)
	if err != nil {
		return nil, 0, err
	}
	parent, base := path.Dir(memberPath), path.Base(memberPath)
	entries, err := image.ReadDir(parent)
	if err != nil {
		return nil, 0, err
	}
	for _, entry := range entries {
		if entry.Name == base {
			if entry.Dir {
				return nil, 0, syscall.EISDIR
			}
			return reader, entry.Size, nil
		}
	}
	return nil, 0, syscall.ENOENT
}
