package mountfs

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/workqueue"
)

func TestIndexStatusRequeuesAfterMemoryEviction(t *testing.T) {
	root, _ := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root.tree.ctx = ctx
	waitComplete := func() {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for {
			status, err := root.StatusArchiveIndex(ctx, "photos.zip")
			if err != nil || status.State == "failed" {
				t.Fatalf("status: %+v, %v", status, err)
			}
			if status.State == "complete" {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("index did not complete")
			}
			time.Sleep(time.Millisecond)
		}
	}
	waitComplete()
	// Hold the background slots so a persisted index cannot be restored yet.
	var releases []func()
	for i := 0; i < 3; i++ {
		release, err := root.tree.acquireBuild(workqueue.Background(ctx))
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
		defer release()
	}
	root.tree.mu.Lock()
	for key, item := range root.tree.meta {
		if strings.HasPrefix(key, "zip:") {
			delete(root.tree.meta, key)
			root.tree.metaBytes -= item.bytes
		}
	}
	root.tree.mu.Unlock()
	status, err := root.StatusArchiveIndex(ctx, "photos.zip")
	if err != nil || (status.State != "queued" && status.State != "scanning") {
		t.Fatalf("evicted index reported complete before restoration: %+v, %v", status, err)
	}
	for _, release := range releases {
		release()
	}
	waitComplete()
}
