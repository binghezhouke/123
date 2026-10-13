package mountfs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"syscall"

	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/bodgit/sevenzip"
)

// A clear header may describe both clear and encrypted body groups. Inspect
// only the selected outer member (possibly an ancestor of a nested path).
// Cache the immutable group fact, never a password or discovery failure.
func (t *Tree) root7zReadNeedsPassword(ctx context.Context, source *storage.Remote, a *archiveDescriptor, idx *zipIndex, memberPath string) (bool, error) {
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
	if m == nil || m.size == 0 {
		return false, nil
	}
	if !m.sevenStream.valid(m.size) {
		return true, nil // Older/unsupported metadata retains password discovery.
	}
	prepared, err := t.prepareCloudArchiveInput(ctx, source, a)
	if err != nil {
		return false, err
	}
	key := fmt.Sprintf("7z-body-encryption:%s:%d", prepared.identity, m.sevenStream.Stream)
	value, err := t.loadMeta(ctx, key, 0, func(ctx context.Context) (any, int64, error) {
		input := &nestedSourceReader{ReaderAt: archiveRequestReader{ctx: ctx, input: prepared.input}}
		zr, err := sevenzip.NewReader(input, prepared.size)
		if err != nil {
			if sourceErr := input.sourceError(); sourceErr != nil {
				return nil, 0, sourceErr
			}
			if needsPassword(err) {
				return true, int64(len(key) + 128), nil
			}
			return nil, 0, archiveReadError(ctx, err, nil)
		}
		if m.ordinal < 0 || m.ordinal >= len(zr.File) || zr.File[m.ordinal].Name != m.name || zr.File[m.ordinal].Stream != m.sevenStream.Stream {
			return nil, 0, syscall.ESTALE
		}
		layout, err := zr.StreamLayout(m.sevenStream.Stream)
		if err != nil {
			return nil, 0, err
		}
		for _, coder := range layout.Coders {
			if bytes.Equal(coder.ID, []byte{0x06, 0xf1, 0x07, 0x01}) {
				return true, int64(len(key) + 128), nil
			}
		}
		return false, int64(len(key) + 128), nil
	})
	if err != nil {
		return false, err
	}
	return value.(bool), nil
}

// Small groups retain whole-group batching. Larger supported groups use the
// same demand blocks/cursor as nested 7z. A previously verified complete member
// or group always wins: changing the strategy must not discard warm content.
// A nil reader asks the caller to retain its existing unsupported/budget path.
func (t *Tree) tryRoot7zDemand(ctx context.Context, view *nestedArchive, original *member, password []byte, memberKey, sourceIdentity string) (*nestedReadHandle, error) {
	if view.format != ".7z" || !original.sevenStream.valid(original.size) || original.sevenStream.Size <= nestedMemberBlockBytes {
		return nil, nil
	}
	m, err := viewMember(view, original)
	if err != nil {
		return nil, err
	}
	if cached, err := t.cache.OpenCached(memberKey, int64(m.size)); err != nil {
		return nil, err
	} else if cached != nil {
		return &nestedReadHandle{ctx: ctx, input: archivePlainInput{reader: cached}, owned: cached}, nil
	}
	groupKey := t.solid7zKey(sourceIdentity, view.archive, m, password)
	if group, err := t.cache.OpenCached(groupKey, m.sevenStream.Size); err != nil {
		return nil, err
	} else if group != nil {
		return &nestedReadHandle{ctx: ctx, input: archivePlainInput{reader: io.NewSectionReader(group, m.sevenStream.Offset, int64(m.size))}, owned: group}, nil
	}
	plan, err := t.rootSolid7zPlan(ctx, view, m, password)
	if err != nil || plan == nil {
		return nil, err
	}
	releaseView, err := view.retain()
	if err != nil {
		return nil, err
	}
	input, owned, err := t.decodedViewMemberInput(ctx, view, m, password, false, plan)
	if err != nil {
		releaseView()
		return nil, err
	}
	if m.sevenCRCDefined {
		input = newArchiveCheckedInput(input, int64(m.size), m.crc, true, errArchiveIntegrity)
	}
	return &nestedReadHandle{ctx: ctx, input: input, owned: owned, release: releaseView}, nil
}

// Root admission needs no recursive decode permit. Cache its immutable plan
// metadata before acquiring one, avoiding a header reparse on every warm open.
// The cached copy owns no view, reader or context; each caller binds a fresh
// copy to its own live view. Nested ancestry still uses its original admission.
func (t *Tree) rootSolid7zPlan(ctx context.Context, view *nestedArchive, m *member, password []byte) (*nestedSolid7zPlan, error) {
	key := fmt.Sprintf("root-solid7z-plan:%s:%s:%d", view.identity, t.passwordTag(view.archive, password), m.ordinal)
	value, err := t.loadMeta(ctx, key, 0, func(ctx context.Context) (any, int64, error) {
		plan, err := t.prepareNestedSolid7z(ctx, view, m, password)
		if err != nil {
			return nil, 0, err
		}
		bytes := int64(len(key) + 1024)
		if plan != nil {
			plan.parent = nil
			bytes += int64(len(plan.key) + len(plan.member.name) + len(plan.password.ciphertext))
		}
		return plan, bytes, nil
	})
	if err != nil {
		return nil, err
	}
	plan := value.(*nestedSolid7zPlan)
	if plan == nil {
		return nil, nil
	}
	bound := *plan
	bound.parent = view
	return &bound, nil
}
