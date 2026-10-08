package mountfs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/binghezhouke/123/mount123/internal/iostats"
	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/workqueue"
)

// cloudDirectory is an immutable parent listing shared by cloud lookup,
// password-sidecar discovery and split-volume discovery.
type cloudDirectory struct {
	generation uint64
	fetchedAt  time.Time
	files      []panapi.File
	names      []string
	byName     map[string]panapi.File
	entries    map[string]*entry
	bytes      int64
}

var cloudDirectoryGeneration atomic.Uint64

func (n *Node) listCloud(ctx context.Context) (map[string]*entry, error) {
	directory, err := n.tree.cloudDirectory(ctx, n.item.cloud.ID)
	if err != nil {
		return nil, err
	}
	return directory.entries, nil
}

func (n *Node) lookupCloud(ctx context.Context, name string) (map[string]*entry, error) {
	directory, err := n.tree.cloudDirectory(ctx, n.item.cloud.ID)
	if err != nil {
		return nil, err
	}
	f, ok := directory.byName[name]
	if !ok {
		return nil, nil
	}
	copy := f
	return map[string]*entry{name: {name: name, cloud: &copy, directory: n.tree.cloudIsDirectory(f)}}, nil
}

func (t *Tree) cloudDirectory(ctx context.Context, parentID int64) (*cloudDirectory, error) {
	started := time.Now()
	defer t.observeStage(iostats.StageDirectoryLookup, started)
	key := fmt.Sprintf("dir:%d", parentID)
	value, err := t.loadRefreshingDirectoryMeta(ctx, key, t.opts.DirectoryTTL, func(ctx context.Context) (any, int64, error) {
		if !workqueue.IsBackground(ctx) {
			if restored, size, ok := t.restoreCloudDirectory(ctx, parentID); ok {
				return restored, size, nil
			}
		}
		return t.fetchPersistCloudDirectory(ctx, parentID)
	})
	if err != nil {
		return nil, err
	}
	t.startExpiredDirectoryRefresh(key, t.opts.DirectoryTTL, func(ctx context.Context) (any, int64, error) {
		return t.fetchPersistCloudDirectory(ctx, parentID)
	})
	return value.(*cloudDirectory), nil
}

func (t *Tree) fetchPersistCloudDirectory(ctx context.Context, parentID int64) (*cloudDirectory, int64, error) {
	directory, size, err := t.buildCloudDirectory(ctx, parentID)
	if err != nil {
		return nil, 0, err
	}
	directory.fetchedAt = time.Now()
	if err := t.persistCloudDirectory(ctx, parentID, directory); err != nil {
		t.directoryStats.persistenceFailures.Add(1)
	}
	return directory, size, nil
}

func (t *Tree) buildCloudDirectory(ctx context.Context, parentID int64) (*cloudDirectory, int64, error) {
	t.directoryStats.listCalls.Add(1)
	files, err := t.api.List(ctx, parentID)
	if err != nil {
		t.directoryStats.listErrors.Add(1)
		return nil, 0, err
	}
	return t.buildCloudDirectoryFromFiles(ctx, files)
}

func (t *Tree) buildCloudDirectoryFromFiles(ctx context.Context, files []panapi.File) (*cloudDirectory, int64, error) {
	if len(files) > t.opts.MaxEntries {
		return nil, 0, fmt.Errorf("directory exceeds %d entries", t.opts.MaxEntries)
	}
	// Reserve real names before assigning aliases so that an alias can never
	// hide another cloud item. ID order makes the mapping independent of API
	// pagination/order; the smallest ID keeps each original name.
	files = append([]panapi.File(nil), files...)
	sort.SliceStable(files, func(i, j int) bool { return files[i].ID < files[j].ID })
	reserved := make(map[string]bool, len(files))
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		if !validName(f.Name) {
			return nil, 0, errors.New("cloud directory contains an invalid filename")
		}
		reserved[f.Name] = true
	}
	directory := &cloudDirectory{generation: cloudDirectoryGeneration.Add(1), files: files, byName: make(map[string]panapi.File, len(files)), entries: make(map[string]*entry, len(files))}
	size := int64(128)
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		name := f.Name
		if _, exists := directory.byName[name]; exists {
			name = cloudDuplicateName(f, reserved)
			size += int64(len(name))
		}
		size += int64(400 + len(f.Name) + len(f.Version))
		if size > t.opts.MetadataBytes {
			return nil, 0, fmt.Errorf("directory exceeds metadata budget; increase -metadata-mib")
		}
		file := f
		directory.byName[name] = f
		directory.entries[name] = &entry{name: name, cloud: &file, directory: t.cloudIsDirectory(f)}
		directory.names = append(directory.names, name)
	}
	sort.Strings(directory.names)
	directory.bytes = size
	return directory, size, nil
}

type directoryDiskSnapshot struct {
	Version   int           `json:"version"`
	ParentID  int64         `json:"parent_id"`
	FetchedAt time.Time     `json:"fetched_at"`
	Files     []panapi.File `json:"files"`
}

func (t *Tree) directorySnapshotKey(parentID int64) string {
	if t.cache == nil || !t.cacheScopeStable {
		return ""
	}
	identity := fmt.Sprintf("%s\x00%d", t.cacheScope, parentID)
	return "directory-snapshot:" + t.cache.StableDigest("cloud-directory-v1", identity)
}

func (t *Tree) persistCloudDirectory(ctx context.Context, parentID int64, directory *cloudDirectory) error {
	if t.cache == nil || t.cache.IsEphemeral() || !t.cacheScopeStable {
		return nil
	}
	encoded, err := json.Marshal(directoryDiskSnapshot{Version: 1, ParentID: parentID, FetchedAt: directory.fetchedAt, Files: directory.files})
	if err != nil {
		return err
	}
	if int64(len(encoded)) > t.maxDirectorySnapshotRecordBytes() {
		return fmt.Errorf("serialized directory snapshot exceeds metadata budget")
	}
	return t.cache.ReplaceArchiveIndex(ctx, t.directorySnapshotKey(parentID), encoded)
}

func (t *Tree) maxDirectorySnapshotRecordBytes() int64 {
	if t.opts.MetadataBytes > int64(^uint64(0)>>1)/2 {
		return int64(^uint64(0) >> 1)
	}
	return t.opts.MetadataBytes * 2
}

func (t *Tree) restoreCloudDirectory(ctx context.Context, parentID int64) (*cloudDirectory, int64, bool) {
	if t.cache == nil || !t.cacheScopeStable {
		return nil, 0, false
	}
	t.directoryStats.diskRestoreAttempts.Add(1)
	key := t.directorySnapshotKey(parentID)
	h, err := t.cache.OpenArchiveIndex(key)
	if err != nil {
		return nil, 0, false
	}
	if h.Size() <= 0 || h.Size() > t.maxDirectorySnapshotRecordBytes() {
		_ = h.Close()
		t.directoryStats.corruptSnapshots.Add(1)
		_ = t.cache.Remove(key)
		return nil, 0, false
	}
	var snapshot directoryDiskSnapshot
	data, readErr := io.ReadAll(io.NewSectionReader(h, 0, h.Size()))
	closeErr := h.Close()
	if readErr == nil {
		readErr = closeErr
	}
	if readErr == nil {
		readErr = json.Unmarshal(data, &snapshot)
	}
	if readErr != nil || snapshot.Version != 1 || snapshot.ParentID != parentID || snapshot.FetchedAt.IsZero() || snapshot.FetchedAt.After(time.Now().Add(5*time.Minute)) || len(snapshot.Files) > t.opts.MaxEntries {
		t.directoryStats.corruptSnapshots.Add(1)
		_ = t.cache.Remove(key)
		return nil, 0, false
	}
	directory, size, err := t.buildCloudDirectoryFromFiles(ctx, snapshot.Files)
	if err != nil {
		if ctx.Err() == nil {
			t.directoryStats.corruptSnapshots.Add(1)
			_ = t.cache.Remove(key)
		}
		return nil, 0, false
	}
	directory.fetchedAt = snapshot.FetchedAt
	t.directoryStats.diskRestores.Add(1)
	return directory, size, true
}

// Only the mounted name changes. Keep File.Name intact for archive format,
// password-sidecar and split-volume discovery against the cloud namespace.
func cloudDuplicateName(file panapi.File, reserved map[string]bool) string {
	stem, extension := file.Name, ""
	if !file.IsDir {
		extension = path.Ext(stem)
		if extension == stem { // A dotfile has no separate extension.
			extension = ""
		}
		stem = stem[:len(stem)-len(extension)]
	}
	for attempt := 1; ; attempt++ {
		suffix := fmt.Sprintf(" [id=%d]", file.ID)
		if attempt > 1 {
			suffix = fmt.Sprintf(" [id=%d-%d]", file.ID, attempt)
		}
		base, ext := stem, extension
		if len(ext)+len(suffix) >= 255 {
			base, ext = file.Name, ""
		}
		if limit := 255 - len(suffix) - len(ext); len(base) > limit {
			base = base[:limit]
			for !utf8.ValidString(base) {
				base = base[:len(base)-1]
			}
		}
		name := base + suffix + ext
		if !reserved[name] {
			reserved[name] = true
			return name
		}
	}
}
