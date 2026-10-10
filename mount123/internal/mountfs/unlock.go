package mountfs

import (
	"context"
	"errors"
	"hash/crc32"
	"io"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/bodgit/sevenzip"
	"github.com/nwaples/rardecode/v2"
)

const passwordValidationBytes = 256 << 20

// ArchivePasswordAPI is optional: ordinary filesystem requests remain read-only.
// Only an explicit UnlockArchives control request can upload password sidecars.
type ArchivePasswordAPI interface {
	SaveArchivePassword(context.Context, panapi.File, []byte, bool) (panapi.File, bool, error)
}

// SharedPasswordAPI stores the one hidden password file for a directory.
// It is separate from ArchivePasswordAPI so older test/fake clients remain
// valid for per-archive unlocks.
type SharedPasswordAPI interface {
	SaveSharedPassword(context.Context, panapi.File, []byte, bool) (panapi.File, bool, error)
}

type UnlockOptions struct {
	All            bool `json:"all,omitempty"`
	Shared         bool `json:"shared,omitempty"`
	SkipValidation bool `json:"skip_validation,omitempty"`
	Overwrite      bool `json:"overwrite,omitempty"`
}

type UnlockProgress struct {
	FileID int64  `json:"file_id"`
	Name   string `json:"name"`
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

type UnlockSummary struct {
	Total     int  `json:"total"`
	Saved     int  `json:"saved"`
	Skipped   int  `json:"skipped"`
	Failed    int  `json:"failed"`
	Refreshed bool `json:"refreshed"`
	Cancelled bool `json:"cancelled"`
}

type unlockTarget struct {
	file    panapi.File
	archive *archiveDescriptor
}

func (t *Tree) passwordTarget(ctx context.Context, item *entry) (*unlockTarget, error) {
	if item == nil || item.cloud == nil || item.cloud.IsDir || item.member != nil || item.disc != nil || item.source != nil {
		return nil, syscall.EINVAL
	}
	f := *item.cloud
	kind := archiveKind(f.Name)
	if item.archive != nil {
		kind = item.archive.kind()
	}
	if kind == "" {
		record, err := t.loadProbeRecord(ctx, f.ParentID)
		if err != nil {
			return nil, err
		}
		for _, result := range record.Results {
			if result.matches(f) && result.State == "detected" {
				kind = result.Kind
				break
			}
		}
	}
	if kind == "" {
		return nil, syscall.EINVAL
	}
	if _, err := panapi.PasswordSidecarName(f); err != nil {
		return nil, syscall.EINVAL
	}
	return &unlockTarget{file: f, archive: &archiveDescriptor{id: f.ID, parentID: f.ParentID, name: f.Name, version: f.Version, size: f.Size, format: kind}}, nil
}

// UnlockArchives explicitly saves sidecars in the cloud, then refreshes their
// parent. It does not write through FUSE or install a private password override.
func (n *Node) UnlockArchives(ctx context.Context, value string, password []byte, options UnlockOptions, progress func(UnlockProgress) error) (summary UnlockSummary, err error) {
	if len(password) == 0 || len(password) > maxPasswordBytes || !utf8.Valid(password) || strings.ContainsAny(string(password), "\x00\r\n") {
		return summary, syscall.EINVAL
	}
	saver, ok := n.tree.api.(ArchivePasswordAPI)
	if !ok {
		return summary, syscall.EOPNOTSUPP
	}
	parts, err := diagnosisPathParts(value)
	if err != nil {
		return summary, err
	}
	parentPath, targetName := ".", ""
	if options.All || options.Shared {
		parentPath = strings.Join(parts, "/")
		if parentPath == "" {
			parentPath = "."
		}
	} else {
		if len(parts) == 0 {
			return summary, syscall.EINVAL
		}
		targetName = parts[len(parts)-1]
		if len(parts) > 1 {
			parentPath = strings.Join(parts[:len(parts)-1], "/")
		}
	}
	t := n.tree
	t.passwordSaveOnce.Do(func() { t.passwordSaveGate = make(chan struct{}, 1) })
	select {
	case t.passwordSaveGate <- struct{}{}:
	case <-ctx.Done():
		return summary, ctx.Err()
	}
	defer func() { <-t.passwordSaveGate }()
	parent, err := n.cloudDirectoryAt(ctx, parentPath)
	if err != nil {
		return summary, err
	}
	parentID := parent.item.cloud.ID
	if _, err = t.requestDirectoryRefresh(ctx, parentID, n, parentPath, false); err != nil {
		return summary, err
	}
	directory, err := t.cloudDirectory(ctx, parentID)
	if err != nil {
		return summary, err
	}
	if options.Shared {
		sharedSaver, ok := n.tree.api.(SharedPasswordAPI)
		if !ok {
			return summary, syscall.EOPNOTSUPP
		}
		if options.All {
			return summary, syscall.EINVAL
		}
		defer func() {
			summary.Cancelled = ctx.Err() != nil
			refreshCtx, stop := context.WithTimeout(t.ctx, 25*time.Second)
			defer stop()
			_, refreshErr := t.requestDirectoryRefresh(refreshCtx, parentID, n, parentPath, false)
			summary.Refreshed = refreshErr == nil
		}()
		summary.Total = 1
		state := "saved"
		reason := ""
		for _, f := range directory.files {
			if f.Name == panapi.SharedPasswordFileName && !f.IsDir {
				if !options.Overwrite {
					state = "skipped_existing"
					summary.Skipped = 1
				}
				break
			}
		}
		if state == "saved" {
			directoryFile := panapi.File{ID: parentID, IsDir: true, ParentID: 0}
			if _, skipped, e := sharedSaver.SaveSharedPassword(ctx, directoryFile, password, options.Overwrite); e != nil {
				state, reason = "save_failed", "shared_password_error"
				summary.Failed = 1
			} else if skipped {
				state = "skipped_existing"
				summary.Skipped = 1
			} else {
				summary.Saved = 1
			}
		}
		if progress != nil {
			if err := progress(UnlockProgress{FileID: parentID, Name: panapi.SharedPasswordFileName, State: state, Reason: reason}); err != nil {
				return summary, err
			}
		}
		return summary, nil
	}
	var targets []*unlockTarget
	if options.All {
		seen := make(map[int64]bool)
		for _, f := range directory.files {
			if f.IsDir || seen[f.ID] {
				continue
			}
			file := f
			file.ParentID = parentID
			target, e := t.passwordTarget(ctx, &entry{cloud: &file})
			if e != nil {
				if errors.Is(e, syscall.EINVAL) {
					continue
				}
				return summary, e
			}
			seen[f.ID] = true
			targets = append(targets, target)
		}
	} else {
		item := directory.entries[targetName]
		if item == nil || item.cloud == nil {
			return summary, syscall.ENOENT
		}
		copy := *item
		file := *item.cloud
		file.ParentID = parentID
		copy.cloud = &file
		target, e := t.passwordTarget(ctx, &copy)
		if e != nil {
			return summary, e
		}
		targets = append(targets, target)
	}
	if len(targets) == 0 {
		return summary, syscall.EINVAL
	}
	if len(targets) > 1000 {
		return summary, syscall.EFBIG
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].file.Name < targets[j].file.Name })
	summary.Total = len(targets)
	// Refresh even after a canceled/partially completed batch, using the mount
	// lifetime rather than the disconnected client's canceled context.
	defer func() {
		summary.Cancelled = ctx.Err() != nil
		refreshCtx, stop := context.WithTimeout(t.ctx, 25*time.Second)
		defer stop()
		_, refreshErr := t.requestDirectoryRefresh(refreshCtx, parentID, n, parentPath, false)
		summary.Refreshed = refreshErr == nil
	}()
	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		name, _ := panapi.PasswordSidecarName(target.file)
		state := "saved"
		reason := ""
		exists := false
		for _, f := range directory.files {
			if f.Name == name {
				exists = true
				break
			}
		}
		if exists && !options.Overwrite {
			summary.Skipped++
			state = "skipped_existing"
		} else {
			if !options.SkipValidation {
				validationCtx, stop := context.WithTimeout(ctx, 2*time.Minute)
				check := &Node{tree: t, item: &entry{cloud: &target.file, archive: target.archive, directory: true}}
				source, e := check.source(validationCtx)
				if e == nil {
					e = t.validateArchivePassword(validationCtx, source, target.archive, password)
				}
				stop()
				if e != nil {
					state = "validation_failed"
					reason = "password_or_archive_error"
					if errors.Is(e, syscall.EFBIG) {
						reason = "validation_limit"
					}
					if errors.Is(e, context.DeadlineExceeded) {
						reason = "validation_timeout"
					}
					summary.Failed++
				}
			}
			if state == "saved" {
				if err := ctx.Err(); err != nil {
					return summary, err
				}
				_, skipped, e := saver.SaveArchivePassword(ctx, target.file, password, options.Overwrite)
				if e != nil {
					state = "save_failed"
					summary.Failed++
				} else if skipped {
					state = "skipped_existing"
					summary.Skipped++
				} else {
					summary.Saved++
				}
			}
		}
		if progress != nil {
			if err := progress(UnlockProgress{FileID: target.file.ID, Name: name, State: state, Reason: reason}); err != nil {
				return summary, err
			}
		}
	}
	return summary, nil
}

func (t *Tree) validateArchivePassword(ctx context.Context, source *storage.Remote, archive *archiveDescriptor, password []byte) error {
	if archive.kind() == ".zip" {
		return validateZIPPassword(ctx, source, archive.size, password, passwordValidationBytes)
	}
	reader, size, _, err := t.archiveSource(ctx, source, archive)
	if err != nil {
		return err
	}
	if archive.kind() == ".rar" {
		reader = source.NewMetadataReader(ctx)
	}
	header := &budgetReaderAt{r: reader, left: 64 << 20}
	stop := errors.New("validated first archive header")
	noPasswordErr := scanArchive(ctx, archive.kind(), header, size, nil, func(archiveMember) error { return stop })
	var encrypted *sevenzip.ReadError
	if errors.Is(noPasswordErr, rardecode.ErrArchiveEncrypted) || (errors.As(noPasswordErr, &encrypted) && encrypted.Encrypted) {
		err := scanArchive(ctx, archive.kind(), header, size, password, func(archiveMember) error { return stop })
		if err == nil || errors.Is(err, stop) {
			return nil
		}
		return err
	}
	if noPasswordErr != nil && !errors.Is(noPasswordErr, stop) {
		return noPasswordErr
	}
	if archive.kind() == ".7z" {
		return validate7zStreams(ctx, reader, size, password)
	}
	var selected *member
	var prefix uint64
	encryptedSeen := false
	err = scanArchive(ctx, ".rar", header, size, password, func(f archiveMember) error {
		// Solid RAR fallback decodes every preceding member. Bound that work,
		// not merely the final member's size; independent members open directly.
		if prefix > passwordValidationBytes || f.size > passwordValidationBytes-prefix {
			prefix = passwordValidationBytes + 1
		} else {
			prefix += f.size
		}
		if f.directory || !f.encrypted {
			return nil
		}
		encryptedSeen = true
		cost := prefix
		if f.rarLocator != nil && f.rarLocator.Direct() {
			cost = f.size
		}
		if cost <= passwordValidationBytes && (selected == nil || f.size < selected.size) {
			selected = &member{format: ".rar", name: f.name, size: f.size, ordinal: f.ordinal, rarLocator: f.rarLocator}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if selected == nil {
		if encryptedSeen {
			return syscall.EFBIG
		}
		return syscall.EACCES
	}
	if selected.size > passwordValidationBytes {
		return syscall.EFBIG
	}
	return extractArchiveMember(ctx, &budgetReaderAt{r: reader, left: passwordValidationBytes}, size, selected, password, io.Discard)
}

// Plaintext 7z headers do not expose member encryption. Validate each stream
// once and each member CRC, under total packed/unpacked budgets, rather than
// assuming the smallest member is encrypted or repeatedly decoding solid data.
func validate7zStreams(ctx context.Context, reader io.ReaderAt, size int64, password []byte) error {
	bounded := &budgetReaderAt{r: reader, left: passwordValidationBytes}
	zr, err := sevenzip.NewReaderWithPassword(bounded, size, string(password))
	if err != nil {
		return err
	}
	var total uint64
	for _, stream := range zr.Streams() {
		if stream.UncompressedSize > passwordValidationBytes-total {
			return syscall.EFBIG
		}
		total += stream.UncompressedSize
	}
	if total == 0 {
		return syscall.EACCES
	}
	for _, stream := range zr.Streams() {
		var files []*sevenzip.File
		for _, f := range zr.File {
			if f.Stream == stream.Index {
				if _, ok := f.StreamOffset(); ok {
					files = append(files, f)
				}
			}
		}
		sort.Slice(files, func(i, j int) bool { a, _ := files[i].StreamOffset(); b, _ := files[j].StreamOffset(); return a < b })
		rc, e := zr.OpenStreamWithReader(stream.Index, bounded)
		if e != nil {
			return e
		}
		r := &contextReader{ctx: ctx, r: rc}
		pos := int64(0)
		for _, f := range files {
			offset, _ := f.StreamOffset()
			if offset < pos || uint64(offset) > stream.UncompressedSize || f.UncompressedSize > stream.UncompressedSize-uint64(offset) {
				e = syscall.EIO
				break
			}
			if _, e = io.CopyN(io.Discard, r, offset-pos); e != nil {
				break
			}
			hash := crc32.NewIEEE()
			if _, e = io.CopyN(hash, r, int64(f.UncompressedSize)); e != nil {
				break
			}
			if hash.Sum32() != f.CRC32 {
				e = errArchiveIntegrity
				break
			}
			pos = offset + int64(f.UncompressedSize)
		}
		if e == nil {
			_, e = io.CopyN(io.Discard, r, int64(stream.UncompressedSize)-pos)
		}
		if e == nil {
			var tail [1]byte
			_, end := r.Read(tail[:])
			if !errors.Is(end, io.EOF) {
				e = syscall.EIO
			}
		}
		e = errors.Join(e, rc.Close())
		if e != nil {
			return e
		}
	}
	return ctx.Err()
}
