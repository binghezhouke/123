package mountfs

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"
)

func pageTestIndex(names ...string) *zipIndex {
	idx := &zipIndex{root: &zipDir{dirs: map[string]*zipDir{}, files: map[string]*member{}}, members: map[string]*member{}}
	for _, name := range names {
		m := &member{name: name}
		idx.root.order = append(idx.root.order, name)
		idx.root.files[name] = m
		idx.members[name] = m
	}
	return idx
}

func TestArchivePageKeepsDiscoveryCursorWhenEarlierNameArrives(t *testing.T) {
	idx := pageTestIndex("z.jpg")
	first, err := archivePageFromIndex(idx, "", "archive:1", "", 60)
	if err != nil || len(first.Items) != 1 || first.NextCursor == "" || first.Complete {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	// Poll before new entries arrive: retain the position, not a terminal cursor.
	waiting, err := archivePageFromIndex(idx, "", "archive:1", first.NextCursor, 100)
	if err != nil || len(waiting.Items) != 0 || waiting.NextCursor != first.NextCursor {
		t.Fatalf("waiting=%+v err=%v", waiting, err)
	}
	idx.root.order = append(idx.root.order, "a.jpg")
	idx.root.files["a.jpg"] = &member{name: "a.jpg"}
	idx.complete = true
	last, err := archivePageFromIndex(idx, "", "archive:1", waiting.NextCursor, 100)
	if err != nil || len(last.Items) != 1 || last.Items[0].Name != "a.jpg" || !last.Complete || last.NextCursor != "" {
		t.Fatalf("last=%+v err=%v", last, err)
	}
	// A new visit to the completed archive uses alphabetical order.
	fresh, err := archivePageFromIndex(idx, "", "archive:1", "", 1)
	if err != nil || fresh.Items[0].Name != "a.jpg" {
		t.Fatalf("fresh=%+v err=%v", fresh, err)
	}
}

func TestArchivePageRestoredIndexAndDirectoryScope(t *testing.T) {
	idx := pageTestIndex("z.jpg", "a.jpg")
	idx.complete = true
	first, _ := archivePageFromIndex(idx, "", "archive:1", "", 1)
	// Nested indexes are reconstructed per request; completed alphabetical
	// cursors must also survive those reconstructions and process restarts.
	restored := pageTestIndex("a.jpg", "z.jpg")
	restored.complete = true
	next, err := archivePageFromIndex(restored, "", "archive:1", first.NextCursor, 1)
	if err != nil || len(next.Items) != 1 || next.Items[0].Name != "z.jpg" || next.NextCursor != "" {
		t.Fatalf("restored=%+v err=%v", next, err)
	}
	other, err := archivePageFromIndex(restored, "", "archive:2", first.NextCursor, 1)
	if err != nil || other.Items[0].Name != "a.jpg" {
		t.Fatalf("scope=%+v err=%v", other, err)
	}
	partial := pageTestIndex("z.jpg")
	old, _ := archivePageFromIndex(partial, "", "archive:1", "", 1)
	restarted, err := archivePageFromIndex(restored, "", "archive:1", old.NextCursor, 100)
	if err != nil || len(restarted.Items) != 2 {
		t.Fatalf("restart=%+v err=%v", restarted, err)
	}
}

func TestArchivePageEmptyProgressiveDirectoryAndFailure(t *testing.T) {
	idx := pageTestIndex()
	empty, err := archivePageFromIndex(idx, "pending", "scope", "", 60)
	if err != nil || empty.Complete || empty.NextCursor == "" || len(empty.Items) != 0 {
		t.Fatalf("empty=%+v err=%v", empty, err)
	}
	idx.root.dirs["pending"] = &zipDir{order: []string{"one.jpg"}, files: map[string]*member{"one.jpg": {name: "one.jpg"}}}
	idx.complete = true
	ready, err := archivePageFromIndex(idx, "pending", "scope", empty.NextCursor, 60)
	if err != nil || !ready.Complete || len(ready.Items) != 1 || ready.Items[0].Name != "one.jpg" {
		t.Fatalf("ready=%+v err=%v", ready, err)
	}
	idx.scanErr = syscall.EIO
	if _, err := archivePageFromIndex(idx, "pending", "scope", "", 60); !errors.Is(err, syscall.EIO) {
		t.Fatalf("scan failure hidden: %v", err)
	}
}

func TestProgressiveArchiveWaitWakesOnFirstMember(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tree := &Tree{ctx: ctx, opts: Options{MaxConcurrentBuilds: 1}}
	idx := pageTestIndex()
	idx.changed = make(chan struct{})
	published, finish := make(chan struct{}), make(chan struct{})
	defer close(finish)
	result := make(chan error, 1)
	go func() {
		_, err := tree.waitArchiveIndex(ctx, "test-index", 500*time.Millisecond, func(context.Context) (any, error) {
			tree.mu.Lock()
			call := tree.archiveTasks["test-index"]
			call.partial = idx
			close(call.updated)
			call.updated = nil
			tree.mu.Unlock()
			close(published)
			<-finish
			return idx, nil
		}, true)
		result <- err
	}()
	<-published
	// Let the waiter observe the initially empty snapshot before publishing a
	// member; the background scan intentionally remains blocked afterwards.
	time.Sleep(20 * time.Millisecond)
	idx.mu.Lock()
	idx.members["one.jpg"] = &member{name: "one.jpg"}
	close(idx.changed)
	idx.changed = make(chan struct{})
	idx.mu.Unlock()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("first member failed: %v", err)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("first member waited for scan completion instead of its notification")
	}
}
