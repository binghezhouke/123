package webserve

import (
	"os/exec"
	"testing"
)

func TestArchiveEntriesBrowserPendingFlow(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is required for the browser-script boundary test")
	}
	out, err := exec.Command(node, "testdata/archive_entries_pending_test.js", "static/js/archive_entries.js").CombinedOutput()
	if err != nil {
		t.Fatalf("archive browser pending behavior: %v\n%s", err, out)
	}
}
