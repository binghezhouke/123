package webserve

import (
	"os/exec"
	"testing"
)

func TestArchiveBrowserNavigationAfterReturningToCloudDirectory(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is required for the browser-script boundary test")
	}
	out, err := exec.Command(node, "testdata/archive_browser_navigation_test.js", "static/js/archive_browser.js").CombinedOutput()
	if err != nil {
		t.Fatalf("archive navigation after partial return: %v\n%s", err, out)
	}
}
