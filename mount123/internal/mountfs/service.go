package mountfs

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"syscall"
	"time"

	"github.com/binghezhouke/123/mount123/internal/iostats"
	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
)

// FileEntry is a single directory listing row for the web front end.
type FileEntry struct {
	ID        int64
	Name      string
	Size      int64
	IsDir     bool
	UpdatedAt time.Time
}

// ArchiveEntry is one member of an archive, for the web archive browser.
type ArchiveEntry struct {
	Name      string
	Size      uint64
	IsDir     bool
	Encrypted bool
}

// Service exposes the read-only cloud-and-archive tree to non-FUSE consumers
// (the HTTP front end). It shares the same disk cache and directory snapshots
// as a mounted tree when pointed at the same cache directory.
type Service struct {
	tree *Tree
}

// NewService prepares a cloud tree without mounting FUSE.
func NewService(ctx context.Context, api API, cache *storage.Cache, rootID int64, zipDirs bool, opts Options) (*Service, error) {
	root := NewWithOptions(ctx, api, cache, rootID, zipDirs, opts)
	if err := root.Prepare(ctx); err != nil {
		return nil, err
	}
	return &Service{tree: root.tree}, nil
}

// NewServiceFromNode wraps an already-prepared FUSE root so the web front end
// and the mount share one in-memory tree. The caller owns the root's lifetime.
func NewServiceFromNode(root *Node) *Service {
	if root == nil {
		return nil
	}
	return &Service{tree: root.tree}
}

// ListDirectory returns the files and folders directly under parentID, backed
// by the tree's cached directory snapshot.
func (s *Service) ListDirectory(ctx context.Context, parentID int64) ([]FileEntry, error) {
	directory, err := s.tree.cloudDirectory(ctx, parentID)
	if err != nil {
		return nil, err
	}
	entries := make([]FileEntry, 0, len(directory.files))
	for _, f := range directory.files {
		entries = append(entries, FileEntry{
			ID:        f.ID,
			Name:      f.Name,
			Size:      f.Size,
			IsDir:     f.IsDir,
			UpdatedAt: fileUpdatedAt(f),
		})
	}
	return entries, nil
}

// File returns the cloud metadata for one file ID.
func (s *Service) File(ctx context.Context, fileID int64) (*panapi.File, error) {
	meta, ok := s.tree.api.(MetadataAPI)
	if !ok {
		return nil, syscall.EOPNOTSUPP
	}
	key := fmt.Sprintf("detail:%d", fileID)
	value, err := s.tree.loadMeta(ctx, key, s.tree.opts.DirectoryTTL, func(ctx context.Context) (any, int64, error) {
		file, detailErr := meta.Detail(ctx, fileID)
		if detailErr != nil {
			return nil, 0, detailErr
		}
		return file, int64(512 + len(file.Name) + len(file.Version)), nil
	})
	if err != nil {
		return nil, err
	}
	file := value.(panapi.File)
	return &file, nil
}

// OpenFile returns a cached random-access reader for a plain cloud file and
// its size. Directories return EISDIR.
func (s *Service) OpenFile(ctx context.Context, fileID int64) (io.ReaderAt, int64, error) {
	file, err := s.File(ctx, fileID)
	if err != nil {
		return nil, 0, err
	}
	if file.IsDir {
		return nil, 0, syscall.EISDIR
	}
	source, err := s.tree.cloudFileSource(ctx, file, s.tree.opts.SourceTTL)
	if err != nil {
		return nil, 0, err
	}
	return source, file.Size, nil
}

// SaveArchivePassword uploads a sibling <name>.pwd sidecar for one archive.
func (s *Service) SaveArchivePassword(ctx context.Context, archive panapi.File, password []byte, overwrite bool) (panapi.File, bool, error) {
	saver, ok := s.tree.api.(ArchivePasswordAPI)
	if !ok {
		return panapi.File{}, false, syscall.EOPNOTSUPP
	}
	return saver.SaveArchivePassword(ctx, archive, password, overwrite)
}

// SaveSharedPassword uploads the directory-wide .mount123.pwd sidecar.
func (s *Service) SaveSharedPassword(ctx context.Context, directory panapi.File, password []byte, overwrite bool) (panapi.File, bool, error) {
	saver, ok := s.tree.api.(SharedPasswordAPI)
	if !ok {
		return panapi.File{}, false, syscall.EOPNOTSUPP
	}
	return saver.SaveSharedPassword(ctx, directory, password, overwrite)
}

// SharedPassword reads the nearest .mount123.pwd for a directory, or returns
// an empty string when none is set.
func (s *Service) SharedPassword(ctx context.Context, parentID int64) (string, error) {
	passwordAPI, ok := s.tree.api.(PasswordAPI)
	if !ok {
		return "", nil
	}
	// A synthetic archive whose sibling sidecar never matches forces the shared
	// password lookup (current directory, then ancestors).
	archive := &archiveDescriptor{id: -1, parentID: parentID, name: ".mount123-shared-probe.7z"}
	found, _, err := s.tree.findArchivePassword(ctx, archive)
	if err != nil {
		return "", err
	}
	if found == nil {
		return "", nil
	}
	raw, err := passwordAPI.ReadSmallFile(ctx, found.id, maxPasswordBytes)
	if err != nil {
		return "", err
	}
	defer clear(raw)
	password, err := normalizedPassword(raw)
	if err != nil {
		return "", err
	}
	return string(password), nil
}

// ProbeDirectory starts an explicit format probe for one cloud directory and
// returns the job snapshot. Detected archives are exposed as browsable
// archives by DetectedArchives.
func (s *Service) ProbeDirectory(ctx context.Context, parentID int64) (ArchiveProbeStatus, error) {
	return s.tree.startProbe(ctx, parentID)
}

// ProbeStatus returns the current probe job state for a directory.
func (s *Service) ProbeStatus(parentID int64) ArchiveProbeStatus {
	return s.tree.probeStatus(parentID)
}

// DetectedArchives returns the file IDs whose content was recognised as an
// archive, mapped to the detected format. Only successful detections whose
// content version still matches are returned.
func (s *Service) DetectedArchives(ctx context.Context, parentID int64) (map[int64]string, error) {
	record, err := s.tree.loadProbeRecord(ctx, parentID)
	if err != nil {
		return nil, err
	}
	detected := make(map[int64]string)
	for _, result := range record.Results {
		if result.Kind != "" && result.State == "detected" {
			detected[result.ID] = result.Kind
		}
	}
	return detected, nil
}

// ListArchive returns the members directly under memberPath inside an archive
// (empty path is the archive root). The archive format is derived from the
// cloud filename; split 7z is entered through its .001 volume.
func (s *Service) ListArchive(ctx context.Context, fileID int64, memberPath string) ([]ArchiveEntry, error) {
	idx, archive, source, err := s.outerArchiveIndex(ctx, fileID)
	if err != nil {
		return nil, err
	}
	first, rest := splitArchivePath(memberPath)
	if first != "" {
		idx.mu.RLock()
		m := idx.members[first]
		idx.mu.RUnlock()
		if m != nil && archiveKind(m.name) != "" {
			nested, nestedErr := s.nestedArchiveFrom(ctx, source, archive, idx, first, m)
			if nestedErr != nil {
				return nil, nestedErr
			}
			return archiveEntriesFromIndex(nested.index, rest)
		}
	}
	return archiveEntriesFromIndex(idx, memberPath)
}

// OpenArchiveMember returns a random-access reader over one archive member and
// its size. Members are materialized through the shared disk cache; large
// plain ZIP Store members map directly onto the remote file instead.
func (s *Service) OpenArchiveMember(ctx context.Context, fileID int64, memberPath string) (io.ReaderAt, int64, error) {
	idx, archive, source, err := s.outerArchiveIndex(ctx, fileID)
	if err != nil {
		return nil, 0, err
	}
	first, rest := splitArchivePath(memberPath)
	if rest != "" {
		idx.mu.RLock()
		m := idx.members[first]
		idx.mu.RUnlock()
		if m != nil && archiveKind(m.name) != "" {
			nested, nestedErr := s.nestedArchiveFrom(ctx, source, archive, idx, first, m)
			if nestedErr != nil {
				return nil, 0, nestedErr
			}
			nested.index.mu.RLock()
			inner := nested.index.members[rest]
			nested.index.mu.RUnlock()
			if inner == nil {
				return nil, 0, syscall.ENOENT
			}
			return s.openNestedMember(ctx, nested, inner)
		}
	}
	kind := archive.kind()
	if kind == "" {
		return nil, 0, syscall.ENOTSUP
	}
	if kind == ".zip" {
		return s.openZIPMember(ctx, source, archive, memberPath)
	}
	password, err := s.tree.otherPassword(ctx, archive)
	if err != nil {
		return nil, 0, err
	}
	defer clear(password)
	idx.mu.RLock()
	m := idx.members[memberPath]
	idx.mu.RUnlock()
	if m == nil {
		return nil, 0, syscall.ENOENT
	}
	_, _, identity, err := s.tree.archiveSource(ctx, source, archive)
	if err != nil {
		return nil, 0, err
	}
	key := s.tree.diskCacheScope() + ":" + identity + ":" + kind + ":archive-member:" + memberPath + fmt.Sprintf(":%d:%08x:", m.size, m.crc) + s.tree.passwordTag(archive, password)
	handle, err := s.tree.cache.Acquire(ctx, key, int64(m.size), func(fillCtx context.Context, w io.Writer) error {
		return s.tree.extractOtherMember(fillCtx, source, archive, m, password, w)
	})
	if err != nil {
		return nil, 0, err
	}
	return handle, int64(m.size), nil
}

func (s *Service) openZIPMember(ctx context.Context, source *storage.Remote, archive *archiveDescriptor, memberPath string) (io.ReaderAt, int64, error) {
	idx, err := s.tree.getZIP(ctx, source, archive.size, archive)
	if err != nil {
		return nil, 0, err
	}
	idx.mu.RLock()
	m := idx.members[memberPath]
	idx.mu.RUnlock()
	if m == nil {
		return nil, 0, syscall.ENOENT
	}
	if m.flags&1 != 0 {
		password, err := s.tree.archivePassword(ctx, archive)
		if err != nil {
			return nil, 0, err
		}
		defer clear(password)
		key := s.tree.diskCacheScope() + ":" + source.Key() + ":.zip:encrypted-member:" + memberPath + fmt.Sprintf(":%08x:%d:%s", m.crc, m.size, s.tree.passwordTag(archive, password))
		handle, err := s.tree.cache.Acquire(ctx, key, int64(m.size), func(fillCtx context.Context, w io.Writer) error {
			release, releaseErr := s.tree.acquireBuild(fillCtx)
			if releaseErr != nil {
				return releaseErr
			}
			defer release()
			started := time.Now()
			fillErr := encryptedMember(fillCtx, source, m, password, w)
			s.tree.observeStage(iostats.StageDecompression, started)
			return fillErr
		})
		if err != nil {
			if isPasswordError(err) {
				return nil, 0, syscall.EACCES
			}
			return nil, 0, err
		}
		return handle, int64(m.size), nil
	}
	if m.method == zip.Store {
		offset, err := m.dataOffset(ctx)
		if err != nil {
			return nil, 0, err
		}
		return io.NewSectionReader(source, offset, int64(m.size)), int64(m.size), nil
	}
	if m.method != zip.Deflate {
		return nil, 0, syscall.EOPNOTSUPP
	}
	key := s.tree.diskCacheScope() + ":" + source.Key() + ":.zip:member:" + memberPath + fmt.Sprintf(":%08x:%d", m.crc, m.size)
	handle, err := s.tree.cache.Acquire(ctx, key, int64(m.size), func(fillCtx context.Context, w io.Writer) error {
		release, releaseErr := s.tree.acquireBuild(fillCtx)
		if releaseErr != nil {
			return releaseErr
		}
		defer release()
		started := time.Now()
		fillErr := inflateMember(fillCtx, source, m, w)
		s.tree.observeStage(iostats.StageDecompression, started)
		return fillErr
	})
	if err != nil {
		return nil, 0, err
	}
	return handle, int64(m.size), nil
}

func fileUpdatedAt(f panapi.File) time.Time {
	if !f.UpdatedAt.IsZero() {
		return f.UpdatedAt
	}
	return f.CreatedAt
}
