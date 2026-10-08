package mountfs

import (
	"bytes"
	"context"
	"syscall"
	"testing"

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
}
