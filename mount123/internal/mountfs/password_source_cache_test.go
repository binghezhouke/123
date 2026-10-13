package mountfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
)

type passwordSourceAPI struct {
	mu          sync.Mutex
	directories map[int64][]panapi.File
	passwords   map[int64][]byte
	reads       map[int64]int
	failures    int
	started     chan struct{}
	release     chan struct{}
}

func (a *passwordSourceAPI) List(_ context.Context, id int64) ([]panapi.File, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]panapi.File(nil), a.directories[id]...), nil
}
func (*passwordSourceAPI) DownloadURL(context.Context, int64) (string, error) {
	return "", syscall.EOPNOTSUPP
}
func (a *passwordSourceAPI) ReadSmallFile(ctx context.Context, id, limit int64) ([]byte, error) {
	a.mu.Lock()
	a.reads[id]++
	data := append([]byte(nil), a.passwords[id]...)
	fail := a.failures > 0
	if fail {
		a.failures--
	}
	started, release := a.started, a.release
	a.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if release != nil {
		select {
		case <-ctx.Done():
			clear(data)
			return nil, ctx.Err()
		case <-release:
		}
	}
	if fail {
		clear(data)
		return nil, errors.New("synthetic sidecar read failure")
	}
	if int64(len(data)) > limit {
		clear(data)
		return nil, syscall.EACCES
	}
	return data, nil
}

func newPasswordSourceFixture(t *testing.T, sidecar string) (*Tree, *passwordSourceAPI) {
	t.Helper()
	api := &passwordSourceAPI{
		directories: map[int64][]panapi.File{
			0: {{ID: 7, ParentID: 0, Name: "directory", IsDir: true}},
			7: {{ID: 101, ParentID: 7, Name: sidecar, Size: 6}},
		},
		passwords: map[int64][]byte{101: []byte("first!")}, reads: map[int64]int{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	root := NewWithOptions(ctx, api, nil, 0, true, Options{DirectoryTTL: time.Hour})
	if _, err := root.tree.cloudDirectory(ctx, 0); err != nil {
		t.Fatal(err)
	}
	return root.tree, api
}

func passwordSourceArchive(name string, id, size int64) *archiveDescriptor {
	return &archiveDescriptor{id: id, parentID: 7, name: name, size: size, version: "archive-v1"}
}

func requireSourcePassword(t *testing.T, tree *Tree, archive *archiveDescriptor, want string) {
	t.Helper()
	password, err := tree.archivePassword(context.Background(), archive)
	defer clear(password)
	if err != nil || string(password) != want {
		t.Fatalf("selected password mismatch for %s: err=%v", archive.name, err)
	}
}

func refreshPasswordSourceDirectory(t *testing.T, tree *Tree, id int64) {
	t.Helper()
	if _, err := tree.refreshCloudDirectory(context.Background(), id, nil, ""); err != nil {
		t.Fatal(err)
	}
}

func TestPasswordSourceRefreshWithoutVersionAndReplacement(t *testing.T) {
	for _, sidecar := range []string{"a.7z.pwd", ".mount123.pwd"} {
		for _, version := range []string{"", ":6"} {
			t.Run(sidecar+"/version="+version, func(t *testing.T) {
				tree, api := newPasswordSourceFixture(t, sidecar)
				api.directories[7][0].Version = version
				archive := passwordSourceArchive("a.7z", 10, 42)
				requireSourcePassword(t, tree, archive, "first!")
				api.mu.Lock()
				api.passwords[101] = []byte("second") // Same ID, size and missing API version.
				api.mu.Unlock()
				refreshPasswordSourceDirectory(t, tree, 7)
				requireSourcePassword(t, tree, archive, "second")
				api.mu.Lock()
				if api.reads[101] != 2 {
					t.Errorf("same-ID sidecar reads=%d, want 2 after refresh", api.reads[101])
				}
				api.directories[7] = nil
				api.mu.Unlock()
				refreshPasswordSourceDirectory(t, tree, 7)
				requireSourcePassword(t, tree, archive, "")
				api.mu.Lock()
				api.directories[7] = []panapi.File{{ID: 102, ParentID: 7, Name: sidecar, Size: 6}}
				api.passwords[102] = []byte("third!")
				api.mu.Unlock()
				refreshPasswordSourceDirectory(t, tree, 7)
				requireSourcePassword(t, tree, archive, "third!")
				api.mu.Lock()
				defer api.mu.Unlock()
				if api.reads[101] != 2 || api.reads[102] != 1 {
					t.Errorf("delete/replacement reads=%v", api.reads)
				}
			})
		}
	}
}

func TestPasswordSourceSelectionRetainsOwnAndNearestSharedPrecedence(t *testing.T) {
	tree, api := newPasswordSourceFixture(t, ".mount123.pwd")
	outer := passwordSourceArchive("a.7z", 10, 42)
	other := passwordSourceArchive("b.rar", 20, 99)
	inner := passwordSourceArchive("inner.7z", 10, 12)
	requireSourcePassword(t, tree, outer, "first!")
	api.mu.Lock()
	api.directories[0] = append(api.directories[0], panapi.File{ID: 100, Name: ".mount123.pwd", Size: 6})
	api.passwords[100] = []byte("parent")
	api.directories[7] = append(api.directories[7], panapi.File{ID: 102, ParentID: 7, Name: "a.7z.pwd", Size: 6}, panapi.File{ID: 103, ParentID: 7, Name: "inner.7z.pwd", Size: 6})
	api.passwords[102], api.passwords[103] = []byte("ownpwd"), []byte("inner!")
	api.mu.Unlock()
	refreshPasswordSourceDirectory(t, tree, 0)
	refreshPasswordSourceDirectory(t, tree, 7)
	requireSourcePassword(t, tree, outer, "ownpwd")
	requireSourcePassword(t, tree, inner, "inner!")
	requireSourcePassword(t, tree, other, "first!")
	api.mu.Lock()
	api.directories[7] = nil
	api.mu.Unlock()
	refreshPasswordSourceDirectory(t, tree, 7)
	requireSourcePassword(t, tree, outer, "parent")
	requireSourcePassword(t, tree, other, "parent")
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.reads[100] != 1 || api.reads[101] != 2 || api.reads[102] != 1 || api.reads[103] != 1 {
		t.Errorf("password selection reads=%v", api.reads)
	}
}

func TestPasswordSourceCoalescesConcurrentDifferentArchives(t *testing.T) {
	tree, api := newPasswordSourceFixture(t, ".mount123.pwd")
	api.started, api.release = make(chan struct{}, 1), make(chan struct{})
	errs := make(chan error, 20)
	var calls sync.WaitGroup
	for i := 0; i < cap(errs); i++ {
		calls.Add(1)
		go func(i int) {
			defer calls.Done()
			archive := passwordSourceArchive(fmt.Sprintf("archive-%d.7z", i), int64(10+i), int64(42+i))
			password, err := tree.archivePassword(context.Background(), archive)
			defer clear(password)
			if err != nil || string(password) != "first!" {
				errs <- fmt.Errorf("archive %d password error: %v", i, err)
			}
		}(i)
	}
	select {
	case <-api.started:
	case <-time.After(time.Second):
		t.Fatal("password read did not begin")
	}
	close(api.release)
	calls.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.reads[101] != 1 {
		t.Errorf("concurrent shared sidecar reads=%d, want 1", api.reads[101])
	}
}

func TestPasswordSourceFailedReadAndCancellationRetry(t *testing.T) {
	for _, interrupted := range []string{"failure", "cancelled"} {
		t.Run(interrupted, func(t *testing.T) {
			tree, api := newPasswordSourceFixture(t, ".mount123.pwd")
			first := passwordSourceArchive("a.7z", 10, 42)
			second := passwordSourceArchive("b.rar", 20, 99)
			if interrupted == "failure" {
				api.failures = 1
				if password, err := tree.archivePassword(context.Background(), first); err == nil {
					clear(password)
					t.Fatal("synthetic sidecar read should fail")
				}
				requireSourcePassword(t, tree, second, "first!")
			} else {
				api.started, api.release = make(chan struct{}, 2), make(chan struct{})
				ctx, cancel := context.WithCancel(context.Background())
				firstDone := make(chan error, 1)
				secondDone := make(chan error, 1)
				go func() {
					password, err := tree.archivePassword(ctx, first)
					clear(password)
					firstDone <- err
				}()
				select {
				case <-api.started:
				case <-time.After(time.Second):
					t.Fatal("password read did not begin")
				}
				go func() {
					password, err := tree.archivePassword(context.Background(), second)
					defer clear(password)
					if err == nil && string(password) != "first!" {
						err = errors.New("retry password mismatch")
					}
					secondDone <- err
				}()
				cancel()
				if err := <-firstDone; !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled read error=%v", err)
				}
				close(api.release)
				if err := <-secondDone; err != nil {
					t.Fatalf("uncancelled archive retry error=%v", err)
				}
				if password, err := tree.archivePassword(ctx, second); !errors.Is(err, context.Canceled) {
					clear(password)
					t.Fatalf("cancelled cached read error=%v", err)
				}
			}
			api.mu.Lock()
			defer api.mu.Unlock()
			if api.reads[101] != 2 {
				t.Errorf("interrupted sidecar read attempts=%d, want 2", api.reads[101])
			}
		})
	}
}

func TestPasswordSourceCacheRemainsTreeLocalAndProtected(t *testing.T) {
	tree, api := newPasswordSourceFixture(t, ".mount123.pwd")
	archive := passwordSourceArchive("a.7z", 10, 42)
	requireSourcePassword(t, tree, archive, "first!")
	tree.mu.Lock()
	for key, item := range tree.meta {
		if !strings.HasPrefix(key, "password:") {
			continue
		}
		protected, ok := item.value.(protectedPassword)
		if !ok || bytes.Contains(protected.ciphertext, []byte("first!")) {
			t.Error("password source cache must contain protected contents")
		}
		if password, err := tree.decryptPassword(key+"different", protected); err == nil {
			clear(password)
			t.Error("protected password accepted a different cache key")
		}
	}
	tree.mu.Unlock()
	otherRoot := NewWithOptions(context.Background(), api, nil, 0, true, Options{DirectoryTTL: time.Hour})
	requireSourcePassword(t, otherRoot.tree, archive, "first!")
	separateTree, separateAPI := newPasswordSourceFixture(t, ".mount123.pwd")
	separateAPI.passwords[101] = []byte("second")
	requireSourcePassword(t, separateTree, archive, "second")
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.reads[101] != 2 {
		t.Errorf("fresh tree must read mutable sidecar: reads=%d", api.reads[101])
	}
}
