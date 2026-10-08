package mountfs

import (
	"context"
	"errors"
	"io"
	"net"
	"path"
	"strings"
	"syscall"

	"github.com/binghezhouke/123/mount123/internal/faults"
)

// PathDiagnosis is a bounded, safe explanation of what the mount can verify
// about one path. Reason and Action are stable machine-readable codes; no
// upstream error text, URL, credential, password, or file bytes are returned.
type PathDiagnosis struct {
	Path          string              `json:"path"`
	Stage         string              `json:"stage"`
	State         string              `json:"state"`
	Reason        string              `json:"reason"`
	Action        string              `json:"action"`
	FileID        *int64              `json:"file_id,omitempty"`
	MetadataOnly  bool                `json:"metadata_only,omitempty"`
	ArchiveStatus *ArchiveIndexStatus `json:"archive_index,omitempty"`
}

// DiagnosePath resolves a path inside the mounted tree and performs bounded
// checks. retry only resets a failed archive-index status; active work remains
// coalesced. Archive members are never decompressed by this method.
func (n *Node) DiagnosePath(ctx context.Context, mountRelativePath string, retry bool) (PathDiagnosis, error) {
	report := PathDiagnosis{Path: boundedPath(mountRelativePath)}
	parts, err := diagnosisPathParts(mountRelativePath)
	if err != nil {
		return failedDiagnosis(report, "path_resolution", err), nil
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	parent := n
	var currentPath []string
	var activeArchiveStatus *ArchiveIndexStatus
	if len(parts) == 0 {
		if _, err := n.list(ctx); err != nil {
			return failedDiagnosis(report, "directory_metadata", err), nil
		}
		return okDiagnosis(report, "directory_metadata", "directory_listed", "none"), nil
	}
	for i, component := range parts {
		if parent.isDisc() {
			report.MetadataOnly = true
			report.ArchiveStatus = activeArchiveStatus
			report.Stage, report.State = "disc_member_metadata", "unchecked"
			report.Reason = "disc_path_not_verified"
			report.Action = "doctor does not inspect ISO/UDF member data"
			return report, nil
		}
		entries, pending, err := parent.lookupEntries(ctx, component)
		if err != nil {
			return failedDiagnosis(report, "directory_metadata", err), nil
		}
		if pending {
			report.Stage, report.State, report.Reason = "directory_metadata", "pending", "directory_lookup_pending"
			report.Action = "retry the diagnosis after the directory lookup completes"
			return report, nil
		}
		item := entries[component]
		if item == nil {
			return failedDiagnosis(report, "path_resolution", syscall.ENOENT), nil
		}
		currentPath = append(currentPath, component)
		target := &Node{tree: n.tree, item: item, parent: parent}
		if target.isArchiveDirectory() {
			if item.source != nil || item.cloud == nil {
				report.MetadataOnly = true
				report.ArchiveStatus = activeArchiveStatus
				report.Stage, report.State = "nested_archive", "unchecked"
				report.Reason = "nested_archive_content_not_read"
				report.Action = "nested archive content was not opened; diagnose it separately"
				return report, nil
			}
			archive := item.archive
			if archive == nil && item.cloud != nil {
				f := item.cloud
				archive = &archiveDescriptor{id: f.ID, parentID: f.ParentID, name: f.Name, version: f.Version, size: f.Size}
			}
			report.FileID = int64Pointer(archive.id)
			if _, err := target.source(ctx); err != nil {
				return failedDiagnosis(report, "archive_source_probe", err), nil
			}
			archivePath := strings.Join(currentPath, "/")
			status, statusErr := n.archiveDiagnosisStatus(ctx, archivePath, retry)
			if statusErr != nil {
				return failedDiagnosis(report, "archive_index", statusErr), nil
			}
			report.ArchiveStatus, report.MetadataOnly = &status, true
			activeArchiveStatus = &status
			if i < len(parts)-1 {
				report.Stage = "archive_member_metadata"
				report.Reason = "member_path_pending_verification"
				report.Action = "checking member names from the archive index only"
			} else {
				report.Stage = "archive_index"
			}
			switch status.State {
			case "complete":
				if i == len(parts)-1 {
					report.State = "ok"
					report.Stage = "archive_index"
					report.MetadataOnly = true
					report.Reason, report.Action = "archive_index_complete", "none"
				} else {
					parent = target
					continue
				}
			case "failed":
				report.State, report.Reason, report.Action = "failed", status.FailureReason, status.RecommendedAction
			case "queued", "scanning":
				report.State = "pending"
				if i == len(parts)-1 {
					report.Reason, report.Action = "archive_index_pending", "run wait-index on the archive path"
				} else {
					report.Reason, report.Action = "archive_index_pending", "wait for the containing archive index, then retry doctor"
				}
			default:
				report.State, report.Reason, report.Action = "failed", "archive_index_unavailable", "rerun doctor; inspect mount logs if the problem persists"
			}
			return report, nil
		}
		if i != len(parts)-1 {
			if item.member != nil && archiveKind(item.member.name) != "" {
				report.MetadataOnly = true
				report.ArchiveStatus = activeArchiveStatus
				report.Stage, report.State = "nested_archive", "unchecked"
				report.Reason = "nested_archive_content_not_read"
				report.Action = "nested archive content was not opened; diagnose it separately"
				return report, nil
			}
			if !item.directory {
				return failedDiagnosis(report, "path_resolution", syscall.ENOTDIR), nil
			}
			parent = target
			continue
		}
		if id := cloudID(item); id != nil {
			report.FileID = id
		}
		if item.member != nil {
			report.MetadataOnly = true
			report.ArchiveStatus = activeArchiveStatus
			report.Stage, report.State = "archive_member_metadata", "ok"
			report.Reason = "member_metadata_found"
			report.Action = "member content was not decompressed or verified"
			return report, nil
		}
		if item.directory {
			if parent.item.source != nil && parent.item.archive != nil {
				if _, err := target.list(ctx); err != nil {
					return failedDiagnosis(report, "archive_member_metadata", err), nil
				}
				report.MetadataOnly = true
				report.ArchiveStatus = activeArchiveStatus
				report.Stage, report.State = "archive_member_metadata", "ok"
				report.Reason = "member_metadata_found"
				report.Action = "member content was not decompressed or verified"
				return report, nil
			}
			if _, err := target.list(ctx); err != nil {
				return failedDiagnosis(report, "directory_metadata", err), nil
			}
			return okDiagnosis(report, "directory_metadata", "directory_listed", "none"), nil
		}
		if item.cloud == nil {
			report.MetadataOnly = true
			return failedDiagnosis(report, "path_resolution", syscall.EOPNOTSUPP), nil
		}
		source, err := target.source(ctx)
		if err != nil {
			return failedDiagnosis(report, "source_probe", err), nil
		}
		if item.cloud.Size == 0 {
			return okDiagnosis(report, "source_probe", "empty_file", "none"), nil
		}
		var sample [1]byte
		got, err := source.ReadRangeAtContext(ctx, sample[:], 0)
		if err != nil || got != 1 {
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			return failedDiagnosis(report, "sample_read", err), nil
		}
		return okDiagnosis(report, "sample_read", "one_byte_read", "none"), nil
	}
	return report, nil
}

func (n *Node) archiveDiagnosisStatus(ctx context.Context, archivePath string, retry bool) (ArchiveIndexStatus, error) {
	if retry {
		return n.RetryArchiveIndex(ctx, archivePath)
	}
	return n.StatusArchiveIndex(ctx, archivePath)
}

func (n *Node) isArchiveDirectory() bool {
	if n == nil || n.item == nil || !n.item.directory || n.isDisc() || n.item.cloud == nil || n.item.cloud.IsDir {
		return false
	}
	return n.tree.zipDirs && archiveKind(n.item.cloud.Name) != ""
}

func diagnosisPathParts(value string) ([]string, error) {
	if len(value) > 4096 || strings.ContainsRune(value, '\x00') {
		return nil, syscall.EINVAL
	}
	for _, component := range strings.Split(value, "/") {
		if component == ".." {
			return nil, syscall.EINVAL
		}
	}
	clean := path.Clean("/" + strings.TrimPrefix(value, "/"))
	if clean == "/" || clean == "/." {
		return nil, nil
	}
	parts := strings.Split(strings.TrimPrefix(clean, "/"), "/")
	if len(parts) > 256 {
		return nil, syscall.EINVAL
	}
	return parts, nil
}

func boundedPath(value string) string {
	if len(value) <= 4096 {
		return value
	}
	return value[:4096]
}

func cloudID(item *entry) *int64 {
	if item == nil || item.cloud == nil {
		return nil
	}
	return int64Pointer(item.cloud.ID)
}

func int64Pointer(v int64) *int64 { return &v }

func okDiagnosis(report PathDiagnosis, stage, reason, action string) PathDiagnosis {
	report.Stage, report.State, report.Reason, report.Action = stage, "ok", reason, action
	return report
}

func failedDiagnosis(report PathDiagnosis, stage string, err error) PathDiagnosis {
	report.Stage, report.State = stage, "failed"
	report.Reason, report.Action = safeDiagnosisFailure(err, stage)
	return report
}

func failedArchiveStatus(size int64, err error) ArchiveIndexStatus {
	reason, action := safeDiagnosisFailure(err, "archive_index")
	kind := "unknown"
	var classified *faults.Error
	var networkError net.Error
	switch {
	case errors.As(err, &classified):
		kind = string(classified.Kind)
	case errors.As(err, &networkError):
		kind = string(faults.Network)
	case errors.Is(err, context.DeadlineExceeded):
		kind = "timeout"
	case errors.Is(err, context.Canceled):
		kind = "canceled"
	case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EOPNOTSUPP), errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EFBIG), errors.Is(err, syscall.EAGAIN), errors.Is(err, syscall.EIO):
		kind = "local"
	}
	return ArchiveIndexStatus{State: "failed", ArchiveSize: size, FailureKind: kind, FailureReason: reason, RecommendedAction: action}
}

func safeDiagnosisFailure(err error, stage string) (string, string) {
	switch {
	case errors.Is(err, syscall.ENOENT):
		return "path_not_found", "check the path and directory listing"
	case errors.Is(err, syscall.ENOTDIR):
		return "not_a_directory", "check each parent path component"
	case errors.Is(err, syscall.EINVAL):
		return "invalid_path", "use a path relative to the mount root"
	case errors.Is(err, syscall.EACCES):
		if stage == "archive_index" || stage == "archive_source_probe" {
			return "password_required_or_invalid", "refresh the password sidecar or correct its contents, then run doctor --retry"
		}
		return "permission_denied", "check account access to the item"
	case errors.Is(err, syscall.EIO):
		return "read_failed", "run doctor --retry; check the remote file and cache"
	case errors.Is(err, syscall.EOPNOTSUPP):
		return "unsupported_format", "use another tool or a supported archive format"
	case errors.Is(err, syscall.ENOSPC):
		return "cache_full", "free cache space or increase the cache limit"
	case errors.Is(err, syscall.EFBIG), errors.Is(err, syscall.EAGAIN):
		return "resource_limit", "reduce the archive or raise the configured resource limit"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout", "retry when the service or network is responsive"
	case errors.Is(err, context.Canceled):
		return "canceled", "rerun the diagnosis"
	}
	var classified *faults.Error
	var networkError net.Error
	if !errors.As(err, &classified) && !errors.As(err, &networkError) {
		if stage == "directory_metadata" {
			return "unclassified_error", "run doctor --retry; check account access if it persists"
		}
		if stage == "archive_index" {
			return "unclassified_error", "run doctor --retry; check archive format and size"
		}
		return "unclassified_error", "run doctor --retry; check the remote file and cache"
	}
	switch faults.KindOf(err) {
	case faults.Network:
		return "network_error", "check connectivity and run doctor --retry"
	case faults.Throttled:
		return "rate_limited", "wait briefly, then run doctor --retry"
	case faults.Unavailable:
		return "remote_unavailable", "wait for the remote service, then run doctor --retry"
	case faults.Unauthorized:
		return "unauthorized", "check the account session and item permissions"
	case faults.NotFound:
		return "remote_file_missing", "refresh the parent directory and verify the remote item"
	case faults.RemoteChanged:
		return "remote_changed", "refresh the directory and reopen the item"
	case faults.InvalidResponse:
		return "invalid_remote_response", "retry once; if it persists, check the remote service"
	default:
		if stage == "directory_metadata" {
			return "directory_metadata_failed", "run doctor --retry; check account access if it persists"
		}
		if stage == "archive_index" {
			return "archive_index_failed", "run doctor --retry; check archive format and size"
		}
		return "read_failed", "run doctor --retry; check the remote file and cache"
	}
}
