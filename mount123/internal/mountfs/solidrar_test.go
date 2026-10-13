package mountfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
)

func solidRARTestSource(t *testing.T, payload []byte) (*Tree, *storage.Remote, *archiveDescriptor, *member) {
	t.Helper()
	data := storedRARImages(payload, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"solid-rar-test"`)
		http.ServeContent(w, r, "test.rar", time.Unix(1, 0), bytes.NewReader(data))
	}))
	t.Cleanup(server.Close)
	cache, err := storage.NewCache(t.TempDir(), 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	source, err := storage.NewRemote(ctx, cache, "solid-rar-test", int64(len(data)), func(context.Context) (string, error) { return server.URL, nil })
	if err != nil {
		t.Fatal(err)
	}
	a := &archiveDescriptor{id: 77, name: "test.rar", version: "v1", size: int64(len(data))}
	m := &member{format: ".rar", name: "0001.jpg", size: uint64(len(payload)), rarGroupSize: uint64(2 * len(payload))}
	if err := scanArchive(ctx, ".rar", bytes.NewReader(data), int64(len(data)), nil, func(f archiveMember) error {
		if f.ordinal == 0 {
			m.rarLocator = f.rarLocator
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return New(ctx, nil, cache, 0, true).tree, source, a, m
}

type notifyingRARWriter struct {
	bytes.Buffer
	threshold int
	notified  chan struct{}
	once      sync.Once
}

func (w *notifyingRARWriter) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	if w.Len() >= w.threshold {
		w.once.Do(func() { close(w.notified) })
	}
	return n, err
}

func TestSolidRARFailureAfterOutputDoesNotAppendFallback(t *testing.T) {
	payload := bytes.Repeat([]byte{'r'}, 400<<10)
	tree, source, a, m := solidRARTestSource(t, payload)
	output := &notifyingRARWriter{threshold: 256 << 10, notified: make(chan struct{})}
	key := tree.solidRARKey(archiveIdentity(source, a), a, m, nil)
	group, err := tree.cache.AcquireGrowingRetained(tree.ctx, tree.ctx, key, int64(m.rarGroupSize), func(ctx context.Context, w io.Writer) error {
		if _, err := w.Write(payload[:256<<10]); err != nil {
			return err
		}
		select {
		case <-output.notified:
			return syscall.EIO
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer group.Close()
	err = tree.extractOtherMember(tree.ctx, source, a, m, nil, output)
	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("partial group failure = %v, want EIO", err)
	}
	if output.Len() != 256<<10 || !bytes.Equal(output.Bytes(), payload[:256<<10]) {
		t.Fatalf("partial output size = %d, want 256 KiB without fallback append", output.Len())
	}
}

func TestSolidRARFailureBeforeOutputFallsBack(t *testing.T) {
	payload := bytes.Repeat([]byte{'r'}, 400<<10)
	tree, source, a, m := solidRARTestSource(t, payload)
	key := tree.solidRARKey(archiveIdentity(source, a), a, m, nil)
	ctx := &verifyingRARContext{Context: tree.ctx, entered: make(chan struct{})}
	group, err := tree.cache.AcquireGrowingRetained(tree.ctx, tree.ctx, key, int64(m.rarGroupSize), func(fillCtx context.Context, _ io.Writer) error {
		select {
		case <-ctx.entered:
			return syscall.EIO
		case <-fillCtx.Done():
			return fillCtx.Err()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer group.Close()
	var output bytes.Buffer
	if err := tree.extractOtherMember(ctx, source, a, m, nil, &output); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), payload) {
		t.Fatalf("fallback output size=%d, want %d", output.Len(), len(payload))
	}
}

type verifyingRARContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *verifyingRARContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func TestSolidRARMemberWaitsForOwnValidation(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "verified-prefix", true: "delayed-checksum-failure"}[fail], func(t *testing.T) {
			payload := bytes.Repeat([]byte{'r'}, 400<<10)
			tree, source, a, m := solidRARTestSource(t, payload)
			key := tree.solidRARKey(archiveIdentity(source, a), a, m, nil)
			written, validate, later := make(chan struct{}), make(chan struct{}), make(chan struct{})
			group, err := tree.cache.AcquireGrowingRetained(tree.ctx, tree.ctx, key, int64(m.rarGroupSize), func(ctx context.Context, w io.Writer) error {
				if _, err := w.Write(payload); err != nil {
					return err
				}
				close(written)
				select {
				case <-validate:
				case <-ctx.Done():
					return ctx.Err()
				}
				if fail {
					return errArchiveIntegrity
				}
				if err := w.(storage.VerifiedPrefixWriter).VerifyThrough(int64(len(payload))); err != nil {
					return err
				}
				select {
				case <-later:
					_, err := w.Write(payload)
					return err
				case <-ctx.Done():
					return ctx.Err()
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			defer group.Close()
			<-written
			ctx := &verifyingRARContext{Context: tree.ctx, entered: make(chan struct{})}
			var output bytes.Buffer
			done := make(chan error, 1)
			go func() {
				done <- tree.extractSolidRARMember(ctx, source, a, m, nil, archiveIdentity(source, a), &output)
			}()
			select {
			case err := <-done:
				t.Fatalf("member finished before its checksum check: %v", err)
			case <-ctx.entered:
			case <-tree.ctx.Done():
				t.Fatal("member did not reach validation wait")
			}
			close(validate)
			err = <-done
			if fail {
				if !errors.Is(err, errArchiveIntegrity) {
					t.Fatalf("member checksum failure = %v", err)
				}
				if _, err := tree.cache.Open(key); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("failed group was published: %v", err)
				}
				return
			}
			if err != nil || !bytes.Equal(output.Bytes(), payload) {
				t.Fatalf("verified member error=%v size=%d", err, output.Len())
			}
			if group.Materialized() {
				t.Fatal("member waited for the entire group")
			}
			close(later)
			if err := group.Wait(tree.ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRARIgnoresMemberCachesFromBeforeBoundaryValidation(t *testing.T) {
	payload := bytes.Repeat([]byte{'r'}, 400<<10)
	root, api := otherArchiveFixture(t, "test.rar", storedRARImages(payload, 2), nil, false)
	root.tree.api = rarServiceTestAPI{api}
	archive := lookup(t, root, "test.rar")
	node := lookup(t, archive, "0001.jpg")
	m, a := node.item.member, node.item.archive
	identity := archiveIdentity(node.item.source, a)
	oldKey := root.tree.diskCacheScope() + ":" + identity + ":.rar:archive-member:" + m.name + fmt.Sprintf(":%d:%08x:", m.size, m.crc) + root.tree.passwordTag(a, nil)
	stale, err := root.tree.cache.Acquire(context.Background(), oldKey, int64(m.size), func(_ context.Context, w io.Writer) error {
		_, err := w.Write(bytes.Repeat([]byte{'x'}, len(payload)))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = stale.Close()
	readOther(t, node, payload)
	reader, size, err := NewServiceFromNode(root).OpenArchiveMember(context.Background(), a.id, m.name)
	if err != nil {
		t.Fatal(err)
	}
	if closer, ok := reader.(io.Closer); ok {
		defer closer.Close()
	}
	got := make([]byte, size)
	if n, err := reader.ReadAt(got, 0); err != nil || n != len(payload) || !bytes.Equal(got, payload) {
		t.Fatalf("service reused unverified legacy cache: n=%d err=%v", n, err)
	}
}

type rarServiceTestAPI struct{ *archiveAPI }

func (a rarServiceTestAPI) Infos(ctx context.Context, ids []int64) ([]panapi.File, error) {
	var files []panapi.File
	for _, id := range ids {
		f, err := a.Detail(ctx, id)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, nil
}

func (a rarServiceTestAPI) Detail(_ context.Context, id int64) (panapi.File, error) {
	for _, f := range a.files {
		if f.ID == id {
			return f, nil
		}
	}
	return panapi.File{}, syscall.ENOENT
}
