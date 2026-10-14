package webserve

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/binghezhouke/123/mount123/internal/panapi"
)

func archiveBreadcrumbFixture(t *testing.T) *listingFixture {
	t.Helper()
	f := newListingFixture(t)
	var data bytes.Buffer
	w := zip.NewWriter(&data)
	member, err := w.Create("内层 & 照片.zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := member.Write(archiveBytes(t)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	f.payload[8] = data.Bytes()
	f.api.details[8] = panapi.File{ID: 8, ParentID: 7, Name: "photos.zip", Size: int64(len(f.payload[8]))}
	f.api.files[7] = []panapi.File{f.api.details[8]}
	return f
}

func TestArchiveBreadcrumbRetainsCloudAncestors(t *testing.T) {
	f := archiveBreadcrumbFixture(t)
	for _, prefix := range []string{"", "内层 & 照片.zip", "内层 & 照片.zip/pics"} {
		t.Run(prefix, func(t *testing.T) {
			r := authenticatedGET(t, f.server.Handler(), "/archive/8?path="+url.QueryEscape(prefix), nil)
			if r.Code != http.StatusOK {
				t.Fatalf("archive page status=%d", r.Code)
			}
			_, nav, found := strings.Cut(r.Body.String(), `<nav class="browser-path" aria-label="压缩包路径">`)
			if !found {
				t.Fatal("archive navigation missing")
			}
			nav, _, _ = strings.Cut(nav, "</nav>")
			var got []string
			for _, m := range regexp.MustCompile(`href="([^"]+)"`).FindAllStringSubmatch(nav, -1) {
				got = append(got, html.UnescapeString(m[1]))
			}
			want := []string{}
			for _, id := range []int64{0, 2, 7} {
				want = append(want, fmt.Sprintf("/browse?parent_id=%d", id))
			}
			want = append(want, "/archive/8")
			if prefix != "" {
				want = append(want, "/archive/8?path="+url.QueryEscape("内层 & 照片.zip"))
				if strings.HasSuffix(prefix, "/pics") {
					want = append(want, "/archive/8?path="+url.QueryEscape(prefix))
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("archive breadcrumb=%v; want full cloud + archive path %v", got, want)
			}
		})
	}
}

func TestArchiveBreadcrumbRespectsMountedRoot(t *testing.T) {
	f := archiveBreadcrumbFixture(t)
	for _, root := range []int64{2, 7} {
		// Info.RootID is the renderer's mounted-root boundary, shared by all
		// page kinds. The archive itself still belongs to cloud directory 7.
		f.server.info.RootID = root
		r := authenticatedGET(t, f.server.Handler(), "/archive/8", nil)
		_, nav, _ := strings.Cut(r.Body.String(), `<nav class="browser-path" aria-label="压缩包路径">`)
		nav, _, _ = strings.Cut(nav, "</nav>")
		if r.Code != 200 || !strings.Contains(nav, fmt.Sprintf(`href="/browse?parent_id=%d"`, root)) || strings.Contains(nav, `href="/browse?parent_id=0"`) {
			t.Fatalf("mounted root %d not respected: %s", root, nav)
		}
		if root == 7 && strings.Contains(nav, `href="/browse?parent_id=2"`) {
			t.Fatal("breadcrumb escaped mounted root")
		}
	}
}

func TestArchiveBreadcrumbReusesBrowsedCloudMetadata(t *testing.T) {
	f := archiveBreadcrumbFixture(t)
	for _, id := range []int64{0, 2, 7} {
		if r := authenticatedGET(t, f.server.Handler(), fmt.Sprintf("/browse?parent_id=%d", id), nil); r.Code != 200 {
			t.Fatalf("browse status=%d", r.Code)
		}
	}
	beforeDetails, beforeLists := f.api.detailCalls.Load(), f.api.lists.Load()
	for _, suffix := range []string{"", "?path=" + url.QueryEscape("内层 & 照片.zip/pics")} {
		if r := authenticatedGET(t, f.server.Handler(), "/archive/8"+suffix, nil); r.Code != 200 {
			t.Fatalf("archive status=%d", r.Code)
		}
	}
	if details, lists := f.api.detailCalls.Load()-beforeDetails, f.api.lists.Load()-beforeLists; details != 0 || lists != 0 {
		t.Fatalf("warm ancestors triggered metadata calls: Detail=%d List=%d", details, lists)
	}
}

func TestArchiveBreadcrumbInBrowser(t *testing.T) {
	python := os.Getenv("MOUNT123_BROWSER_PYTHON")
	if python == "" {
		t.Skip("set MOUNT123_BROWSER_PYTHON to a Python with Playwright")
	}
	f := archiveBreadcrumbFixture(t)
	f.server.authEnabled = false
	host := httptest.NewServer(f.server.Handler())
	defer host.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, python, "testdata/archive_breadcrumb_browser_test.py", host.URL).CombinedOutput()
	if err != nil {
		t.Fatalf("breadcrumb browser navigation: %v\n%s", err, out)
	}
	t.Log(string(out))
}
