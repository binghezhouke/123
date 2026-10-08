package mountfs

import (
	"bytes"
	"context"
	"syscall"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/iostats"
	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func TestPublicReadAPIUpdatesMountIOStats(t *testing.T) {
	root, _ := fixture(t)
	entries, err := root.list(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	folder := &Node{tree: root.tree, item: entries["folder"], parent: root}
	children, err := folder.list(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	file := &Node{tree: root.tree, item: children["plain.txt"], parent: folder}
	handle, _, errno := file.Open(context.Background(), syscall.O_RDONLY)
	if errno != 0 {
		t.Fatal(errno)
	}
	defer handle.(fs.FileReleaser).Release(context.Background())
	result, errno := handle.(fs.FileReader).Read(context.Background(), make([]byte, 32), 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	defer result.Done()
	data, status := result.Bytes(nil)
	if status != fuse.OK || !bytes.Equal(data, []byte("hello cloud")) {
		t.Fatalf("public read = %q, %v", data, status)
	}
	snapshot := root.IOStats()
	if snapshot.ForegroundReadLatency.Status != "measured" || snapshot.ForegroundReadLatency.Samples == nil || *snapshot.ForegroundReadLatency.Samples != 1 {
		t.Fatalf("FUSE-facing Read latency was not recorded: %#v", snapshot.ForegroundReadLatency)
	}
	if snapshot.DownloadedBytes.Status != "measured" || snapshot.DownloadedBytes.Bytes == nil || *snapshot.DownloadedBytes.Bytes == 0 {
		t.Fatalf("HTTP range bytes were not recorded: %#v", snapshot.DownloadedBytes)
	}
	if snapshot.ForegroundReadBytes.Status != "measured" || snapshot.ForegroundReadBytes.Bytes == nil || *snapshot.ForegroundReadBytes.Bytes != 11 || snapshot.ForegroundReadTime.TotalNanos == nil {
		t.Fatalf("application-visible read throughput inputs were not recorded: %#v %#v", snapshot.ForegroundReadBytes, snapshot.ForegroundReadLatency)
	}
	if snapshot.ReadAheadWait.Status != "measured" {
		t.Fatalf("public FileReader read-ahead feedback was not recorded: %#v", snapshot.ReadAheadWait)
	}
	if snapshot.DownloadScheduler.Status != "measured" || snapshot.DownloadScheduler.MaximumRequests == 0 {
		t.Fatalf("cache scheduler capacity was not attached to mount stats: %#v", snapshot.DownloadScheduler)
	}
	if snapshot.Stages.DirectoryLookup.Status != "measured" || snapshot.Stages.DirectoryLookup.Samples == nil || *snapshot.Stages.DirectoryLookup.Samples < 2 {
		t.Fatalf("public directory operations were not attributed: %#v", snapshot.Stages.DirectoryLookup)
	}
	if snapshot.Stages.FileOpen.Status != "measured" || snapshot.Stages.FileOpen.Samples == nil || *snapshot.Stages.FileOpen.Samples != 1 {
		t.Fatalf("public open was not attributed: %#v", snapshot.Stages.FileOpen)
	}
}

func TestPublicArchiveOpenAttributesIndexQueueAndDecompressionStages(t *testing.T) {
	root, _ := fixture(t)
	fs.NewNodeFS(root, &fs.Options{})
	archive := lookup(t, root, "photos.zip")
	images := lookup(t, archive, "images")
	member := lookup(t, images, "8.txt")
	if got := readNode(t, member, 0, 128); len(got) != 128 {
		t.Fatalf("archive member read length = %d", len(got))
	}
	stages := root.IOStats().Stages
	for name, stage := range map[string]iostats.DurationSummary{
		"archive index": stages.ArchiveIndex,
		"build queue":   stages.BuildQueue,
		"decompression": stages.Decompression,
		"file open":     stages.FileOpen,
	} {
		if stage.Status != "measured" || stage.Samples == nil || *stage.Samples == 0 {
			t.Errorf("%s stage was not attributed: %+v", name, stage)
		}
	}
}

type delayedDirectoryAPI struct {
	API
	delay time.Duration
}

func (a delayedDirectoryAPI) List(ctx context.Context, parentID int64) ([]panapi.File, error) {
	timer := time.NewTimer(a.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return a.API.List(ctx, parentID)
	}
}

func TestDirectoryLookupStageAttributesInjectedListLatency(t *testing.T) {
	root, _ := fixture(t)
	root.tree.api = delayedDirectoryAPI{API: root.tree.api, delay: 30 * time.Millisecond}
	if _, err := root.list(context.Background()); err != nil {
		t.Fatal(err)
	}
	stage := root.IOStats().Stages.DirectoryLookup
	if stage.Status != "measured" || stage.TotalNanos == nil || *stage.TotalNanos < uint64(20*time.Millisecond) {
		t.Fatalf("injected directory latency not reflected in its stage: %+v", stage)
	}
}
