package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	remoteIdentityDescriptorVersion = 2
	remoteIdentityLegacyVersion     = 1
	remoteIdentityDefaultTTL        = 6 * 24 * time.Hour
	remoteIdentityMaxDescriptor     = 8 << 10
)

// RemoteIdentity opts a remote source into durable identity restoration. The
// account value must be stable and opaque (normally Cache.StableDigest output).
// A descriptor is written only after the initial probe yields a strong ETag or
// a valid Last-Modified date when no ETag is available.
type RemoteIdentity struct {
	Account       string
	FileID        int64
	Version       string
	TTL           time.Duration
	OnInvalidated func()
}

type remoteIdentityDescriptor struct {
	Version  int       `json:"schema"`
	Identity string    `json:"identity"`
	FileID   int64     `json:"file_id"`
	Content  string    `json:"content_version"`
	Size     int64     `json:"size"`
	ETag     string    `json:"etag"`
	Modified string    `json:"last_modified,omitempty"`
	Obtained time.Time `json:"obtained_at"`
}

// NewCachedRemoteContext restores an identity descriptor before probing the
// source. It preserves NewRemoteContext's cache key format and falls back to
// eager URL resolution and probing whenever durable reuse cannot be proven.
func NewCachedRemoteContext(lifetimeCtx, operationCtx context.Context, cache *Cache, key string, size int64, resolve ResolveURL, identity RemoteIdentity) (*Remote, error) {
	if cache == nil || resolve == nil || size < 0 {
		return nil, errors.New("invalid remote reader configuration")
	}
	if lifetimeCtx == nil {
		lifetimeCtx = context.Background()
	}
	if operationCtx == nil {
		operationCtx = context.Background()
	}
	ctx, cancel := combineContexts(lifetimeCtx, operationCtx)
	defer cancel()
	ctx, stopCache := combineContexts(cache.lifetimeCtx, ctx)
	defer stopCache()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	identityOK := remoteIdentityUsable(identity, size)
	descriptorKey, identityDigest := "", ""
	if identityOK && size > 0 {
		descriptorKey, identityDigest = remoteIdentityKeys(cache, identity, size)
		if restored, valid, err := restoreRemoteIdentity(ctx, cache, descriptorKey, identityDigest, identity, size); err != nil {
			return nil, err
		} else if valid {
			keyWithValidator := ""
			if isStrongETag(restored.ETag) {
				keyWithValidator = key + "\x00etag:" + restored.ETag
			} else {
				keyWithValidator = key + "\x00modified:" + restored.Modified
			}
			r := &Remote{
				lifetimeCtx:           lifetimeCtx,
				cache:                 cache,
				size:                  size,
				key:                   keyWithValidator,
				linkKey:               key,
				resolve:               resolve,
				client:                http.DefaultClient,
				etag:                  restored.ETag,
				modified:              restored.Modified,
				rangeID:               cacheID(keyWithValidator),
				identityExpiresAt:     restored.Obtained.Add(remoteIdentityTTL(identity.TTL)),
				identityDescriptorKey: descriptorKey,
				onIdentityInvalidated: identity.OnInvalidated,
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return r, nil
		}
	}

	r, err := NewRemoteContext(lifetimeCtx, ctx, cache, key, size, resolve)
	if err != nil {
		return nil, err
	}
	useStrongETag := isStrongETag(r.etag)
	useModified := r.etag == "" && isValidLastModified(r.modified)
	if identityOK && size > 0 && (useStrongETag || useModified) {
		r.identityDescriptorKey = descriptorKey
		r.onIdentityInvalidated = identity.OnInvalidated
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		obtained := time.Now().UTC()
		r.identityExpiresAt = obtained.Add(remoteIdentityTTL(identity.TTL))
		descriptor := remoteIdentityDescriptor{
			Version:  remoteIdentityDescriptorVersion,
			Identity: identityDigest,
			FileID:   identity.FileID,
			Content:  identity.Version,
			Size:     size,
			ETag:     r.etag,
			Modified: r.modified,
			Obtained: obtained,
		}
		data, marshalErr := json.Marshal(descriptor)
		if marshalErr == nil && len(data) <= remoteIdentityMaxDescriptor {
			// Descriptor admission is best effort. The eager Remote is still
			// useful when the bounded cache has no room for metadata.
			_ = cache.ReplaceArchiveIndex(ctx, descriptorKey, data)
		}
	}
	return r, nil
}

// IdentityExpiresAt returns the absolute expiry retained by a cached identity.
// Cache owners should use it to avoid extending the descriptor lifetime when a
// Remote is restored in a fresh process or metadata cache.
func (r *Remote) IdentityExpiresAt() time.Time {
	if r == nil {
		return time.Time{}
	}
	return r.identityExpiresAt
}

func remoteIdentityTTL(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return remoteIdentityDefaultTTL
	}
	return ttl
}

func isValidLastModified(value string) bool {
	if len(value) == 0 || len(value) > 128 || strings.TrimSpace(value) != value {
		return false
	}
	_, err := http.ParseTime(value)
	return err == nil
}

func remoteIdentityUsable(identity RemoteIdentity, size int64) bool {
	version := strings.TrimSpace(identity.Version)
	if identity.Account == "" || identity.FileID <= 0 || version == "" || size <= 0 {
		return false
	}
	// PanAPI represents a missing content version as ":<size>". It is not a
	// version token and must not make unrelated same-size contents reusable.
	if version == ":"+strconv.FormatInt(size, 10) {
		return false
	}
	return len(identity.Account) <= 256 && len(version) <= 2048
}

func remoteIdentityKeys(cache *Cache, identity RemoteIdentity, size int64) (descriptorKey, digest string) {
	material := fmt.Sprintf("%s\x00%d\x00%s\x00%d", identity.Account, identity.FileID, identity.Version, size)
	digest = cache.StableDigest("remote-content-identity-v1", material)
	return "remote-identity-v1:" + digest, digest
}

func restoreRemoteIdentity(ctx context.Context, cache *Cache, key, digest string, identity RemoteIdentity, size int64) (remoteIdentityDescriptor, bool, error) {
	h, err := cache.OpenArchiveIndex(key)
	if err != nil {
		if errors.Is(err, ErrClosed) {
			return remoteIdentityDescriptor{}, false, err
		}
		return remoteIdentityDescriptor{}, false, nil
	}
	data := make([]byte, 0)
	if h.Size() > 0 && h.Size() <= remoteIdentityMaxDescriptor {
		data = make([]byte, h.Size())
		_, err = h.ReadAt(data, 0)
	}
	_ = h.Close()
	if ctx.Err() != nil {
		return remoteIdentityDescriptor{}, false, ctx.Err()
	}
	if err != nil || len(data) == 0 {
		_ = cache.Remove(key)
		return remoteIdentityDescriptor{}, false, nil
	}
	var descriptor remoteIdentityDescriptor
	if json.Unmarshal(data, &descriptor) != nil || !validRemoteIdentityDescriptor(descriptor, digest, identity, size, identity.TTL) {
		_ = cache.Remove(key)
		return remoteIdentityDescriptor{}, false, nil
	}
	return descriptor, true, nil
}

func validRemoteIdentityDescriptor(d remoteIdentityDescriptor, digest string, identity RemoteIdentity, size int64, ttl time.Duration) bool {
	validValidator := isStrongETag(d.ETag) || (d.ETag == "" && isValidLastModified(d.Modified))
	validSchema := d.Version == remoteIdentityDescriptorVersion || (d.Version == remoteIdentityLegacyVersion && isStrongETag(d.ETag))
	if !validSchema || d.Identity != digest || d.FileID != identity.FileID || d.Content != identity.Version || d.Size != size || !validValidator || d.Obtained.IsZero() {
		return false
	}
	ttl = remoteIdentityTTL(ttl)
	age := time.Since(d.Obtained)
	return age >= 0 && age < ttl
}

func isStrongETag(etag string) bool {
	etag = strings.TrimSpace(etag)
	if len(etag) < 2 || len(etag) > 1024 || etag[0] != '"' || etag[len(etag)-1] != '"' {
		return false
	}
	for i := 1; i < len(etag)-1; i++ {
		b := etag[i]
		if b == '"' || b < 0x21 || b == 0x7f {
			return false
		}
	}
	return true
}

func (r *Remote) invalidateIdentityDescriptor() {
	if r == nil || r.identityDescriptorKey == "" || !r.identityInvalidated.CompareAndSwap(false, true) {
		return
	}
	// A concurrent constructor can briefly hold the descriptor pinned. Retry
	// removal for a bounded interval so its stale identity is not kept alive.
	for attempt := 0; attempt < 20; attempt++ {
		err := r.cache.Remove(r.identityDescriptorKey)
		if !errors.Is(err, syscall.EBUSY) {
			break
		}
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-r.cache.lifetimeCtx.Done():
			timer.Stop()
			attempt = 20
		case <-timer.C:
		}
	}
	if r.onIdentityInvalidated != nil {
		r.onIdentityInvalidated()
	}
}
