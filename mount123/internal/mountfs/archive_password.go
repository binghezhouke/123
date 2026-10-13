package mountfs

import (
	"context"
	"errors"
	"strings"
	"syscall"

	"github.com/binghezhouke/123/mount123/internal/storage"
)

// Use immutable index facts for the selected outer member, possibly an ancestor
// of the requested nested path. This requires no source I/O or decoder permit.
func (idx *zipIndex) pathNeedsPassword(memberPath string) bool {
	memberPath = strings.Trim(memberPath, "/")
	idx.mu.RLock()
	m := idx.members[memberPath]
	for m == nil {
		end := strings.LastIndexByte(memberPath, '/')
		if end < 0 {
			break
		}
		memberPath = memberPath[:end]
		m = idx.members[memberPath]
	}
	idx.mu.RUnlock()
	return m != nil && m.needsPassword()
}

// otherIndexWithPassword shares header discovery across adapters. Plain headers
// never depend on sidecars; encrypted body groups are resolved at member open.
// The caller must clear the returned password, including on errors.
func (t *Tree) otherIndexWithPassword(ctx context.Context, source *storage.Remote, a *archiveDescriptor, progressive bool) (*zipIndex, []byte, error) {
	idx, err := t.otherIndexMode(ctx, source, a, nil, progressive)
	if err == nil || !(needsPassword(err) || errors.Is(err, syscall.EACCES)) {
		return idx, nil, err
	}
	password, err := t.otherPassword(ctx, a)
	if err != nil {
		return nil, password, err
	}
	idx, err = t.otherIndexMode(ctx, source, a, password, progressive)
	return idx, password, err
}

// RAR's progressive mount traversal retains its password-bound scan identity.
// Switching that identity between List and Lookup would start a second scan.
func (t *Tree) mountArchiveIndex(ctx context.Context, source *storage.Remote, a *archiveDescriptor) (*zipIndex, error) {
	if a.kind() == ".7z" {
		idx, password, err := t.otherIndexWithPassword(ctx, source, a, false)
		clear(password)
		return idx, err
	}
	password, err := t.otherPassword(ctx, a)
	defer clear(password)
	if err != nil {
		return nil, err
	}
	return t.otherIndex(ctx, source, a, password)
}
