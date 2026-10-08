package mountfs

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/workqueue"
	"github.com/hanwen/go-fuse/v2/fs"
)

func TestForegroundOpenWhileBackgroundBuildsBlocked(t *testing.T) {
	root, _ := fixture(t)
	fs.NewNodeFS(root, &fs.Options{})
	ctx, cancel := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	t.Cleanup(func() { cancel(); workers.Wait() })
	started := make(chan struct{}, 3)
	for i := 0; i < 3; i++ {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			_, _ = root.tree.loadMeta(workqueue.Background(ctx), fmt.Sprint("background", i), time.Hour,
				func(ctx context.Context) (any, int64, error) {
					started <- struct{}{}
					<-ctx.Done()
					return nil, 0, ctx.Err()
				})
		}(i)
	}
	for i := 0; i < 3; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("background build did not start")
		}
	}
	// A real public Lookup/Open/Read path still gets the reserved build slot.
	finished := make(chan []byte, 1)
	go func() { finished <- readNode(t, lookup(t, lookup(t, root, "folder"), "plain.txt"), 0, 11) }()
	select {
	case content := <-finished:
		if string(content) != "hello cloud" {
			t.Fatalf("content: %q", content)
		}
	case <-time.After(time.Second):
		t.Fatal("foreground file waited behind background builds")
	}
}
