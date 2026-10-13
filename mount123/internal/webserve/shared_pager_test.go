package webserve

import (
	"net/http"
	"strings"
	"testing"
)

// The first loaded page determines which scripts survive partial navigation.
// Both starting points need the shared pager for archive-to-directory switches
// and restoration of positions beyond the initial metadata page.
func TestSharedPagerLoadsOnDirectoryAndArchiveShells(t *testing.T) {
	directory := newListingFixture(t)
	archive, _ := newArchiveFixture(t)
	for _, tc := range []struct {
		name    string
		handler http.Handler
		path    string
	}{
		{"directory", directory.server.Handler(), "/browse"},
		{"archive", archive.Handler(), "/archive/1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := authenticatedGET(t, tc.handler, tc.path, nil)
			if page.Code != http.StatusOK {
				t.Fatalf("page status = %d", page.Code)
			}
			body := page.Body.String()
			if count := strings.Count(body, `src="/static/js/file_pagination.js?`); count != 1 {
				t.Fatalf("shared pager script count = %d, want 1", count)
			}
			asset := authenticatedGET(t, tc.handler, "/static/js/file_pagination.js", nil)
			if asset.Code != http.StatusOK || !strings.Contains(asset.Body.String(), "window.loadNextFilePage") {
				t.Fatalf("shared pager asset missing: status %d", asset.Code)
			}
		})
	}
}
