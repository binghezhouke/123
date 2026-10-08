package mountfs

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
)

// RefreshResult describes a successfully replaced cloud-directory snapshot.
type RefreshResult struct {
	Entries int `json:"entries"`
}

// RefreshDirectory synchronously replaces one cloud directory's listing.
// Existing directory handles keep their immutable snapshot; future lookups
// use the refreshed listing. Archive and disc-image directories are not cloud
// directories and cannot be refreshed this way.
func (n *Node) RefreshDirectory(ctx context.Context, mountRelativePath string) (RefreshResult, error) {
	if n == nil || n.tree == nil {
		return RefreshResult{}, syscall.EINVAL
	}
	parent, err := n.cloudDirectoryAt(ctx, mountRelativePath)
	if err != nil {
		return RefreshResult{}, err
	}
	if parent.item == nil || parent.item.cloud == nil || !parent.item.cloud.IsDir || parent.item.archive != nil || parent.item.source != nil || parent.item.disc != nil || parent.item.member != nil {
		return RefreshResult{}, syscall.EINVAL
	}
	return n.tree.refreshCloudDirectory(ctx, parent.item.cloud.ID, n, mountRelativePath)
}

func (n *Node) cloudDirectoryAt(ctx context.Context, value string) (*Node, error) {
	if strings.ContainsRune(value, '\x00') {
		return nil, syscall.EINVAL
	}
	for _, component := range strings.Split(filepathSlash(value), "/") {
		if component == ".." {
			return nil, syscall.EINVAL
		}
	}
	clean := path.Clean("/" + strings.TrimPrefix(filepathSlash(value), "/"))
	if clean == "/" || clean == "/." {
		return n, nil
	}
	current := n
	for _, component := range strings.Split(strings.TrimPrefix(clean, "/"), "/") {
		entries, pending, err := current.lookupEntries(ctx, component)
		if err != nil {
			return nil, err
		}
		if pending {
			return nil, syscall.EAGAIN
		}
		item := entries[component]
		if item == nil {
			return nil, syscall.ENOENT
		}
		if !item.directory {
			return nil, syscall.ENOTDIR
		}
		if item.cloud == nil || !item.cloud.IsDir || item.archive != nil || item.source != nil || item.disc != nil || item.member != nil {
			return nil, syscall.EINVAL
		}
		current = &Node{tree: n.tree, item: item, parent: current}
	}
	return current, nil
}

func filepathSlash(value string) string { return strings.ReplaceAll(value, "\\", "/") }

func (t *Tree) refreshCloudDirectory(ctx context.Context, parentID int64, root *Node, mountRelativePath string) (RefreshResult, error) {
	if err := ctx.Err(); err != nil {
		return RefreshResult{}, err
	}
	key := fmt.Sprintf("dir:%d", parentID)
	var previous *metaItem
	var flight *sourceCall
	for {
		t.mu.Lock()
		if existing := t.sources[key]; existing != nil {
			t.mu.Unlock()
			select {
			case <-ctx.Done():
				return RefreshResult{}, ctx.Err()
			case <-existing.done:
			}
			continue
		}
		previous = t.meta[key]
		if previous != nil {
			delete(t.meta, key)
			t.metaBytes -= previous.bytes
		}
		if t.sources == nil {
			t.sources = make(map[string]*sourceCall)
		}
		if t.refreshing == nil {
			t.refreshing = make(map[string]bool)
		}
		flight = &sourceCall{done: make(chan struct{})}
		t.sources[key] = flight
		t.refreshing[key] = true
		t.mu.Unlock()
		break
	}

	release, err := t.acquireBuild(ctx)
	var fresh *cloudDirectory
	var size int64
	if err == nil {
		fresh, size, err = t.buildCloudDirectory(ctx, parentID)
		release()
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil && (size < 0 || size > t.opts.MetadataBytes) {
		err = fmt.Errorf("directory exceeds metadata budget; increase -metadata-mib")
	}

	t.mu.Lock()
	if err == nil && !t.makeMetadataRoomLocked(key, 0, size) {
		err = errors.New("metadata cache cannot fit refreshed directory")
	}
	if err == nil {
		t.seq++
		expires := time.Time{}
		if t.opts.DirectoryTTL > 0 {
			expires = time.Now().Add(t.opts.DirectoryTTL)
		}
		updated := &metaItem{key: key, value: fresh, bytes: size, expires: expires, seq: t.seq}
		t.meta[key] = updated
		t.metaBytes += size
		flight.value = fresh
	} else if previous != nil {
		// Restore the old snapshot when API refresh or validation fails.
		if t.makeMetadataRoomLocked(key, 0, previous.bytes) {
			t.meta[key] = previous
			t.metaBytes += previous.bytes
		}
		flight.value = previous.value
	}
	flight.err = err
	delete(t.refreshing, key)
	if t.sources[key] == flight {
		delete(t.sources, key)
	}
	close(flight.done)
	t.mu.Unlock()
	if err != nil {
		return RefreshResult{}, err
	}

	// Dependent metadata may have been built from the previous listing. Wait
	// only after releasing the directory flight: password discovery can itself
	// be waiting for that flight. Any replacement source that starts now sees
	// the fresh snapshot and is therefore left alone.
	root.notifyCloudDirectoryChange(mountRelativePath, previous, fresh)
	dependents := dependentMetadataKeys(t, parentID, previous, fresh)
	for key, oldFlight := range dependents {
		if strings.HasPrefix(key, "password:") || strings.HasPrefix(key, "volumes:") {
			if oldFlight != nil {
				go t.evictMetadataAfter(key, oldFlight)
			} else {
				t.evictMetadataNow(key)
			}
			continue
		}
		if oldFlight != nil {
			select {
			case <-oldFlight.done:
			case <-ctx.Done():
				for dependentKey, dependentFlight := range dependents {
					if dependentFlight != nil {
						go t.evictMetadataAfter(dependentKey, dependentFlight)
					} else {
						t.evictMetadataNow(dependentKey)
					}
				}
				return RefreshResult{Entries: len(fresh.files)}, nil
			case <-t.ctx.Done():
				return RefreshResult{Entries: len(fresh.files)}, nil
			}
		}
		t.mu.Lock()
		if t.sources[key] == nil {
			if item := t.meta[key]; item != nil {
				delete(t.meta, key)
				t.metaBytes -= item.bytes
			}
		}
		t.mu.Unlock()
	}

	return RefreshResult{Entries: len(fresh.files)}, nil
}

func (t *Tree) evictMetadataAfter(key string, flight *sourceCall) {
	select {
	case <-flight.done:
	case <-t.ctx.Done():
		return
	}
	t.evictMetadataNow(key)
}

func (t *Tree) evictMetadataNow(key string) {
	t.mu.Lock()
	if t.sources[key] == nil {
		if item := t.meta[key]; item != nil {
			delete(t.meta, key)
			t.metaBytes -= item.bytes
		}
	}
	t.mu.Unlock()
}

func dependentMetadataKeys(t *Tree, parentID int64, previous *metaItem, fresh *cloudDirectory) map[string]*sourceCall {
	keys := make(map[string]*sourceCall)
	addInfoKeys := func(directory *cloudDirectory) {
		if directory == nil {
			return
		}
		for _, file := range directory.files {
			keys[fmt.Sprintf("info:%d:%s:%d:%t", file.ID, file.Version, file.Size, file.IsDir)] = nil
		}
	}
	if previous != nil {
		if directory, ok := previous.value.(*cloudDirectory); ok {
			addInfoKeys(directory)
			for _, file := range directory.files {
				if archiveKind(file.Name) != "" {
					keys[fmt.Sprintf("password:%d:%s:%d:%d:%s:g%d", file.ID, file.Version, file.Size, parentID, file.Name, directory.generation)] = nil
				}
				if strings.HasSuffix(strings.ToLower(file.Name), ".7z.001") {
					keys[fmt.Sprintf("volumes:%d:%s:g%d", file.ID, file.Version, directory.generation)] = nil
				}
			}
		}
	}
	t.mu.Lock()
	for key := range keys {
		keys[key] = t.sources[key]
	}
	t.mu.Unlock()
	return keys
}

func (n *Node) notifyCloudDirectoryChange(mountRelativePath string, previous *metaItem, fresh *cloudDirectory) {
	if n == nil || n.Operations() == nil { // The zero Inode used by unit tests has no bridge.
		return
	}
	clean := path.Clean("/" + strings.TrimPrefix(filepathSlash(mountRelativePath), "/"))
	parent := n
	if clean != "/" {
		for _, component := range strings.Split(strings.TrimPrefix(clean, "/"), "/") {
			child := parent.GetChild(component)
			if child == nil {
				return
			}
			ops, ok := child.Operations().(*Node)
			if !ok {
				return
			}
			parent = ops
		}
	}
	if parent.Operations() == nil {
		return
	}
	oldFiles := make(map[string]panapi.File)
	if previous != nil {
		if old, ok := previous.value.(*cloudDirectory); ok {
			oldFiles = old.byName
		}
	}
	newFiles := fresh.byName
	changed := make([]string, 0)
	for name, old := range oldFiles {
		current, ok := newFiles[name]
		if !ok || current.ID != old.ID || current.Version != old.Version || current.Size != old.Size || current.IsDir != old.IsDir || !current.UpdatedAt.Equal(old.UpdatedAt) || !current.CreatedAt.Equal(old.CreatedAt) {
			changed = append(changed, name)
		}
	}
	for name := range newFiles {
		if _, ok := oldFiles[name]; !ok {
			changed = append(changed, name)
		}
	}
	sort.Strings(changed)
	for _, name := range changed {
		if _, stillExists := newFiles[name]; !stillExists {
			if child := parent.GetChild(name); child != nil {
				_ = parent.NotifyDelete(name, child)
			} else {
				_ = parent.NotifyEntry(name)
			}
		} else {
			_ = parent.NotifyEntry(name)
		}
	}
}
