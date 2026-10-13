package mountfs

import (
	"context"
	"errors"
	"syscall"

	"github.com/binghezhouke/123/mount123/internal/storage"
)

// outerArchiveIndex resolves one cloud archive into its parsed index.
func (s *Service) outerArchiveIndex(ctx context.Context, fileID int64) (*zipIndex, *archiveDescriptor, *storage.Remote, []byte, error) {
	return s.outerArchiveIndexMode(ctx, fileID, false)
}

func (s *Service) outerArchiveIndexMode(ctx context.Context, fileID int64, progressive bool) (*zipIndex, *archiveDescriptor, *storage.Remote, []byte, error) {
	file, err := s.tree.archiveFileMetadata(ctx, fileID)
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
	source, err := s.tree.cloudArchiveSource(ctx, &file)
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
		idx, err = s.tree.otherIndexMode(ctx, source, archive, nil, progressive)
		if err != nil && (needsPassword(err) || errors.Is(err, syscall.EACCES)) {
			password, err = s.tree.otherPassword(ctx, archive)
			if err != nil {
				return nil, nil, nil, nil, err
			}
			idx, err = s.tree.otherIndexMode(ctx, source, archive, password, progressive)
		}
	}
	if err != nil {
		clear(password)
		return nil, nil, nil, nil, err
	}
	return idx, archive, source, password, nil
}
