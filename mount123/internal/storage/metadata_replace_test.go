package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestReplaceArchiveIndexPublishesNewBytes(t *testing.T) {
	cache, err := NewCache(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	if err := cache.ReplaceArchiveIndex(context.Background(), "metadata", []byte("old")); err != nil {
		t.Fatal(err)
	}
	if err := cache.ReplaceArchiveIndex(context.Background(), "metadata", []byte("new snapshot")); err != nil {
		t.Fatal(err)
	}
	h, err := cache.OpenArchiveIndex("metadata")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	got := make([]byte, h.Size())
	if _, err := h.ReadAt(got, 0); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte("new snapshot")) {
		t.Fatalf("replacement = %q", got)
	}
}

func TestReplaceArchiveIndexReservesTempBytesAndPreservesOldOnWriteFailure(t *testing.T) {
	cache, err := NewCache(t.TempDir(), 100)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	if err := cache.ReplaceArchiveIndex(context.Background(), "metadata", bytes.Repeat([]byte("o"), 40)); err != nil {
		t.Fatal(err)
	}
	if err := cache.Store(context.Background(), "other", bytes.Repeat([]byte("x"), 60)); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	cache.syncFile = func(f *os.File) error {
		close(started)
		<-release
		return errors.New("injected metadata write failure")
	}
	result := make(chan error, 1)
	go func() {
		result <- cache.ReplaceArchiveIndex(context.Background(), "metadata", bytes.Repeat([]byte("n"), 50))
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("replacement did not reach the paused write")
	}
	if err := cache.Store(context.Background(), "must-not-fit", bytes.Repeat([]byte("z"), 11)); err == nil {
		t.Fatal("concurrent fill ignored reserved temporary bytes")
	}
	close(release)
	if err := <-result; err == nil {
		t.Fatal("injected write failure was ignored")
	}
	h, err := cache.OpenArchiveIndex("metadata")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	got := make([]byte, h.Size())
	if _, err := h.ReadAt(got, 0); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if !bytes.Equal(got, bytes.Repeat([]byte("o"), 40)) {
		t.Fatal("write failure changed the previous metadata object")
	}
}

func TestReplaceArchiveIndexRejectsPinnedReader(t *testing.T) {
	cache, err := NewCache(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	if err := cache.ReplaceArchiveIndex(context.Background(), "metadata", []byte("old")); err != nil {
		t.Fatal(err)
	}
	pinned, err := cache.OpenArchiveIndex("metadata")
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.Close()
	if err := cache.ReplaceArchiveIndex(context.Background(), "metadata", []byte("new")); !errors.Is(err, syscall.EBUSY) {
		t.Fatalf("replacement with pinned reader error = %v", err)
	}
	got := make([]byte, 3)
	if _, err := pinned.ReadAt(got, 0); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if string(got) != "old" {
		t.Fatalf("pinned reader saw %q after rejected replacement", got)
	}
}

func TestReplaceArchiveIndexDemotesGrowthBeyondProtectedShare(t *testing.T) {
	cache, err := NewCache(t.TempDir(), 1<<10)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	if err := cache.ReplaceArchiveIndex(context.Background(), "metadata", []byte("small")); err != nil {
		t.Fatal(err)
	}
	id := cacheID("metadata")
	if cache.entries[id].class != cacheIndex {
		t.Fatalf("initial class=%v, want protected index", cache.entries[id].class)
	}
	if err := cache.ReplaceArchiveIndex(context.Background(), "metadata", bytes.Repeat([]byte("g"), 200)); err != nil {
		t.Fatal(err)
	}
	if cache.entries[id].class != cacheProbation {
		t.Fatalf("grown metadata class=%v, want probation outside index share", cache.entries[id].class)
	}
}

func TestReplaceArchiveIndexColdInsertTracksProtectedShare(t *testing.T) {
	cache, err := NewCache(t.TempDir(), 1<<10)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()

	for _, item := range []struct {
		key  string
		size int
	}{{"first", 80}, {"second", 40}, {"third", 20}} {
		if err := cache.ReplaceArchiveIndex(context.Background(), item.key, bytes.Repeat([]byte("x"), item.size)); err != nil {
			t.Fatalf("insert %s: %v", item.key, err)
		}
	}
	if got, want := cache.Stats().IndexUsedBytes, int64(120); got != want {
		t.Fatalf("protected index bytes = %d, want %d", got, want)
	}
	for _, item := range []struct {
		key  string
		want cacheClass
	}{{"first", cacheIndex}, {"second", cacheIndex}, {"third", cacheProbation}} {
		if got := cache.entries[cacheID(item.key)].class; got != item.want {
			t.Errorf("%s class = %v, want %v", item.key, got, item.want)
		}
	}
}

func TestReplaceArchiveIndexAdmissionFailureKeepsOldBytes(t *testing.T) {
	cache, err := NewCache(t.TempDir(), 100)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	if err := cache.ReplaceArchiveIndex(context.Background(), "metadata", bytes.Repeat([]byte("o"), 50)); err != nil {
		t.Fatal(err)
	}
	if err := cache.Store(context.Background(), "pinned", bytes.Repeat([]byte("p"), 50)); err != nil {
		t.Fatal(err)
	}
	check, err := cache.OpenArchiveIndex("metadata")
	if err != nil {
		t.Fatal(err)
	}
	if check.Size() != 50 {
		t.Fatalf("old object size before failed replacement = %d", check.Size())
	}
	_ = check.Close()
	pinned, err := cache.Open("pinned")
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.Close()
	if err := cache.ReplaceArchiveIndex(context.Background(), "metadata", bytes.Repeat([]byte("n"), 75)); err == nil {
		t.Fatal("replacement unexpectedly fit while the remaining cache object was pinned")
	}
	h, err := cache.OpenArchiveIndex("metadata")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	got := make([]byte, h.Size())
	if _, err := h.ReadAt(got, 0); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if !bytes.Equal(got, bytes.Repeat([]byte("o"), 50)) {
		t.Fatalf("failed replacement changed the prior object: size=%d prefix=%q", len(got), got[:min(len(got), 8)])
	}
}
