package mountfs

import (
	"context"
	"strings"
	"syscall"
)

// resolveArchivePath consumes root's creator reference and returns one owned
// view. Ordinary directories stay within their index; only an explicit archive
// component invokes enterNestedArchive. File mode leaves the final member raw.
func (t *Tree) resolveArchivePath(ctx context.Context, root *nestedArchive, displayPath string, directory bool) (view *nestedArchive, localPath string, m *member, err error) {
	current := root
	success := false
	defer func() {
		if !success {
			_ = current.Close()
		}
	}()
	displayPath = strings.Trim(displayPath, "/")
	if displayPath == "" {
		if !directory {
			return nil, "", nil, syscall.EISDIR
		}
		success = true
		return current, "", nil, nil
	}
	parts := strings.Split(displayPath, "/")
	for i, name := range parts {
		if !validName(name) {
			return nil, "", nil, syscall.EINVAL
		}
		current.index.mu.RLock()
		dir := current.index.dirLocked(localPath)
		complete, scanErr := current.index.complete, current.index.scanErr
		isDir := dir != nil && dir.dirs[name] != nil
		var candidate *member
		if dir != nil {
			candidate = dir.files[name]
		}
		current.index.mu.RUnlock()
		if scanErr != nil {
			return nil, "", nil, scanErr
		}
		next := name
		if localPath != "" {
			next = localPath + "/" + name
		}
		if isDir {
			localPath = next
			if i == len(parts)-1 && !directory {
				return nil, "", nil, syscall.EISDIR
			}
			continue
		}
		if candidate == nil {
			if directory && !complete {
				remaining := strings.Join(parts[i:], "/")
				if localPath != "" {
					remaining = localPath + "/" + remaining
				}
				success = true
				return current, remaining, nil, nil
			}
			return nil, "", nil, syscall.ENOENT
		}
		if i == len(parts)-1 && !directory {
			success = true
			return current, next, candidate, nil
		}
		kind := archiveKind(candidate.name)
		if kind != ".zip" && kind != ".7z" && kind != ".rar" {
			return nil, "", nil, syscall.ENOTDIR
		}
		child, e := t.enterNestedArchive(ctx, current, next, candidate)
		if e != nil {
			return nil, "", nil, e
		}
		_ = current.Close()
		current, localPath = child, ""
	}
	success = true
	return current, localPath, nil, nil
}
