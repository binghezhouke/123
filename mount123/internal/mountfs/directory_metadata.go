package mountfs

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"sync/atomic"
	"unicode/utf8"

	"github.com/binghezhouke/123/mount123/internal/panapi"
)

// cloudDirectory is an immutable parent listing shared by cloud lookup,
// password-sidecar discovery and split-volume discovery.
type cloudDirectory struct {
	generation uint64
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
	key := fmt.Sprintf("dir:%d", parentID)
	value, err := t.loadRefreshingMeta(ctx, key, t.opts.DirectoryTTL, func(ctx context.Context) (any, int64, error) {
		return t.buildCloudDirectory(ctx, parentID)
	})
	if err != nil {
		return nil, err
	}
	return value.(*cloudDirectory), nil
}

func (t *Tree) buildCloudDirectory(ctx context.Context, parentID int64) (*cloudDirectory, int64, error) {
	files, err := t.api.List(ctx, parentID)
	if err != nil {
		return nil, 0, err
	}
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
