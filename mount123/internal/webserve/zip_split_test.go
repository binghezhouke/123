package webserve

import (
	"net/http"
	"strings"
	"testing"

	"github.com/binghezhouke/123/mount123/internal/panapi"
)

func TestBrowseZIPVolumeLinksAndPasswordTargets(t *testing.T) {
	fixture := newListingFixture(t)
	files := []panapi.File{
		{ID: 71, Name: "album.zip.001", Size: 1024},
		{ID: 72, Name: "album.zip.002", Size: 1024},
		{ID: 73, Name: "other.z01", Size: 1024},
		{ID: 74, Name: "other.zip", Size: 1024},
	}
	// The fixture constructor prepares its root snapshot; use a new directory
	// so this test exercises the supplied listing rather than that cached root.
	fixture.api.files[88] = files
	fixture.api.details[88] = panapi.File{ID: 88, Name: "split-fixtures", IsDir: true}
	for _, file := range files {
		fixture.api.details[file.ID] = file
	}
	page := authenticatedGET(t, fixture.server.Handler(), "/browse?parent_id=88", nil)
	if page.Code != http.StatusOK {
		t.Fatalf("browse status = %d", page.Code)
	}
	body := page.Body.String()
	for _, href := range []string{`href="/archive/71"`, `href="/archive/74"`, `href="/file/72"`, `href="/file/73"`} {
		if !strings.Contains(body, href) {
			t.Errorf("browse did not render %s", href)
		}
	}
	page = authenticatedGET(t, fixture.server.Handler(), "/passwords?parent_id=88", nil)
	if page.Code != http.StatusOK {
		t.Fatalf("password page status = %d", page.Code)
	}
	body = page.Body.String()
	if !strings.Contains(body, "album.zip.001") || !strings.Contains(body, "other.zip") || strings.Contains(body, "album.zip.002") || strings.Contains(body, "other.z01") {
		t.Fatal("password targets must include only canonical archive entries")
	}
}
