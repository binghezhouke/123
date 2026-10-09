package mountfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/hanwen/go-fuse/v2/fs"
)

// Two different members in the same solid stream must share actual decoding,
// even when the later member is opened first.
func TestSolid7zDifferentMembersDecodeOnce(t *testing.T) {
	data, err := os.ReadFile("testdata/encrypted/solid.7z")
	if err != nil {
		t.Fatal(err)
	}
	root, _ := otherArchiveFixture(t, "private.7z", data, []byte("mount-test-password"), false)
	archive := lookup(t, root, "private.7z")
	readOther(t, lookup(t, archive, "last.txt"), []byte("last solid member\n"))
	readOther(t, lookup(t, lookup(t, archive, "folder"), "hello.txt"), bytes.Repeat([]byte("mount 7z fixture contents\n"), 200))
	stage := root.IOStats().Stages.Decompression
	if stage.Samples == nil || *stage.Samples != 1 {
		t.Fatalf("same solid stream decoded repeatedly: samples=%v", stage.Samples)
	}
}

func TestSolid7zShortReadThenConcurrentMembers(t *testing.T) {
	data, err := os.ReadFile("testdata/encrypted/solid.7z")
	if err != nil {
		t.Fatal(err)
	}
	root, _ := otherArchiveFixture(t, "private.7z.001", data, []byte("mount-test-password"), true)
	root.tree.opts.StreamMemberThreshold = 1
	archive := lookup(t, root, "private.7z.001")
	first := lookup(t, lookup(t, archive, "folder"), "hello.txt")
	last := lookup(t, archive, "last.txt")
	ctx, cancel := context.WithCancel(context.Background())
	h, _, errno := first.Open(ctx, syscall.O_RDONLY)
	if errno != 0 {
		t.Fatal(errno)
	}
	// Cancelling the Open request cannot poison the group decoder lifetime.
	cancel()
	result, errno := h.(fs.FileReader).Read(context.Background(), make([]byte, 32), 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	result.Done()
	h.(fs.FileReleaser).Release(context.Background())
	var wg sync.WaitGroup
	for _, node := range []*Node{first, last, first, last, last, first} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			want := []byte("last solid member\n")
			if node == first {
				want = bytes.Repeat([]byte("mount 7z fixture contents\n"), 200)
			}
			if got := readGrowingOtherMember(t, node); !bytes.Equal(got, want) {
				t.Error("concurrent member differs")
			}
		}()
	}
	wg.Wait()
	stage := root.IOStats().Stages.Decompression
	if stage.Samples == nil || *stage.Samples != 1 {
		t.Fatal("concurrent members did not share one decoder")
	}
}

func TestSolid7zGroupSurvivesRestartWithoutMemberCache(t *testing.T) {
	data, err := os.ReadFile("testdata/encrypted/solid.7z")
	if err != nil {
		t.Fatal(err)
	}
	root, api := otherArchiveFixture(t, "private.7z", data, []byte("mount-test-password"), false)
	archive := lookup(t, root, "private.7z")
	last := lookup(t, archive, "last.txt")
	readOther(t, last, []byte("last solid member\n"))
	if err := root.tree.cache.Remove(growingOtherMemberKey(t, last)); err != nil {
		t.Fatal(err)
	}
	cacheDir := root.tree.cache.Directory()
	if err := root.tree.cache.Close(); err != nil {
		t.Fatal(err)
	}
	cache, err := storage.NewCache(cacheDir, 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	restarted := New(context.Background(), api, cache, 0, true)
	fs.NewNodeFS(restarted, &fs.Options{})
	first := lookup(t, lookup(t, lookup(t, restarted, "private.7z"), "folder"), "hello.txt")
	readOther(t, first, bytes.Repeat([]byte("mount 7z fixture contents\n"), 200))
	if stage := restarted.IOStats().Stages.Decompression; stage.Samples != nil && *stage.Samples != 0 {
		t.Fatal("restart decoded a persisted solid group again")
	}
}

func TestSolid7zShortReadRetainedGroupCompletes(t *testing.T) {
	data, err := os.ReadFile("testdata/encrypted/solid.7z")
	if err != nil {
		t.Fatal(err)
	}
	root, _ := otherArchiveFixture(t, "private.7z", data, []byte("mount-test-password"), false)
	root.tree.opts.StreamMemberThreshold = 1
	node := lookup(t, lookup(t, lookup(t, root, "private.7z"), "folder"), "hello.txt")
	h, _, errno := node.Open(context.Background(), syscall.O_RDONLY)
	if errno != 0 {
		t.Fatal(errno)
	}
	result, errno := h.(fs.FileReader).Read(context.Background(), make([]byte, 32), 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	result.Done()
	h.(fs.FileReleaser).Release(context.Background())
	password, err := root.tree.otherPassword(context.Background(), node.item.archive)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(password)
	_, _, identity, err := root.tree.archiveSource(context.Background(), node.item.source, node.item.archive)
	if err != nil {
		t.Fatal(err)
	}
	key := root.tree.solid7zKey(identity, node.item.archive, node.item.member, password)
	deadline := time.Now().Add(time.Second)
	for {
		group, err := root.tree.cache.Open(key)
		if err == nil {
			group.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("short read did not preserve group: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSolid7zPrefetchUsesCurrentGroupAndPhysicalOrder(t *testing.T) {
	cache, err := storage.NewCache(t.TempDir(), 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	tree := &Tree{cache: cache, opts: Options{PrefetchFiles: 9, PrefetchBytes: 1 << 20, PrefetchWorkers: 2}}
	p := newImagePrefetch(tree)
	parent := &Node{}
	entries := map[string]*entry{}
	for i := 1; i <= 12; i++ {
		name := fmt.Sprintf("%d.jpg", i)
		entries[name] = &entry{name: name, member: &member{format: ".7z", size: 10, sevenStream: &sevenStreamLocation{Stream: i % 2, Offset: int64(12-i) * 10, Size: 512}}}
	}
	for _, name := range []string{"1.jpg", "2.jpg", "3.jpg"} {
		plan := p.plan(&Node{parent: parent, item: entries[name]}, entries)
		lastOffset := int64(-1)
		for _, item := range plan {
			loc := item.member.sevenStream
			if loc.Stream != entries[name].member.sevenStream.Stream || loc.Offset < lastOffset {
				t.Fatal("prefetch starts unrelated groups or ignores physical order")
			}
			lastOffset = loc.Offset
		}
		if name == "3.jpg" && len(plan) != 4 {
			t.Fatalf("same-group window size=%d want=4", len(plan))
		}
	}
}

func TestSolid7zGroupReservationFallsBackToMember(t *testing.T) {
	data, err := os.ReadFile("testdata/encrypted/solid.7z")
	if err != nil {
		t.Fatal(err)
	}
	for _, capacity := range []int64{6000, 24000} {
		t.Run(fmt.Sprint(capacity), func(t *testing.T) {
			root, _ := otherArchiveFixture(t, "private.7z", data, []byte("mount-test-password"), false)
			cache, err := storage.NewCache(t.TempDir(), capacity)
			if err != nil {
				t.Fatal(err)
			}
			defer cache.Close()
			root.tree.cache = cache
			archive := lookup(t, root, "private.7z")
			node := lookup(t, lookup(t, archive, "folder"), "hello.txt")
			if capacity == 24000 {
				pin, err := cache.Acquire(context.Background(), "pinned", 17000, func(ctx context.Context, w io.Writer) error {
					_, err := w.Write(make([]byte, 17000))
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
				defer pin.Close()
			}
			readOther(t, node, bytes.Repeat([]byte("mount 7z fixture contents\n"), 200))
			if cache.Stats().ReservedBytes != 0 {
				t.Fatal("fallback leaked a group reservation")
			}
		})
	}
}

func TestSolid7zInvalidGroupIsNotPublished(t *testing.T) {
	for _, wrongPassword := range []bool{true, false} {
		t.Run(fmt.Sprint(wrongPassword), func(t *testing.T) {
			fixture, password := "password.7z", []byte("wrong-password")
			if !wrongPassword {
				fixture, password = "copy.7z", []byte("mount-test-password")
			}
			data, err := os.ReadFile("testdata/encrypted/" + fixture)
			if err != nil {
				t.Fatal(err)
			}
			if !wrongPassword {
				data[32] ^= 1
			}
			root, _ := otherArchiveFixture(t, "private.7z", data, password, false)
			root.tree.opts.StreamMemberThreshold = 1
			node := lookup(t, lookup(t, lookup(t, root, "private.7z"), "folder"), "hello.txt")
			assertGrowingOtherArchiveFailsAndIsNotCached(t, node, syscall.EACCES)
			_, _, identity, err := root.tree.archiveSource(context.Background(), node.item.source, node.item.archive)
			if err != nil {
				t.Fatal(err)
			}
			key := root.tree.solid7zKey(identity, node.item.archive, node.item.member, password)
			deadline := time.Now().Add(time.Second)
			for root.tree.cache.Stats().ReservedBytes != 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if root.tree.cache.Stats().ReservedBytes != 0 {
				t.Fatal("invalid group fill did not release reservation")
			}
			cached, err := root.tree.cache.Open(key)
			if err == nil {
				cached.Close()
				t.Fatal("invalid group was published")
			}
			if !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
		})
	}
}
