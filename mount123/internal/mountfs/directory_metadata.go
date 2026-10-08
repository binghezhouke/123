package mountfs

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync/atomic"

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
	directory := &cloudDirectory{generation: cloudDirectoryGeneration.Add(1), files: append([]panapi.File(nil), files...), byName: make(map[string]panapi.File, len(files)), entries: make(map[string]*entry, len(files))}
	size := int64(128)
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		if !validName(f.Name) {
			return nil, 0, errors.New("cloud directory contains an invalid filename")
		}
		if _, exists := directory.byName[f.Name]; exists {
			return nil, 0, errDuplicateCloudName
		}
		size += int64(400 + len(f.Name) + len(f.Version))
		if size > t.opts.MetadataBytes {
			return nil, 0, fmt.Errorf("directory exceeds metadata budget; increase -metadata-mib")
		}
		file := f
		directory.byName[f.Name] = f
		directory.entries[f.Name] = &entry{name: f.Name, cloud: &file, directory: t.cloudIsDirectory(f)}
		directory.names = append(directory.names, f.Name)
	}
	sort.Strings(directory.names)
	directory.bytes = size
	return directory, size, nil
}
