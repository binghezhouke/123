package webserve

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/binghezhouke/123/mount123/internal/mountfs"
	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
)

func TestFileDetailIncludesImmediateParentBreadcrumb(t *testing.T) {
	for _, test := range []struct {
		name         string
		root, fileID int64
		parents      []int64
	}{
		{"nested file", 0, 8, []int64{0, 2, 7}},
		{"subdirectory mount", 2, 8, []int64{2, 7}},
		{"file in mounted root", 7, 8, []int64{7}},
		{"file in cloud root", 0, 9, []int64{0}},
	} {
		t.Run(test.name, func(t *testing.T) {
			api := &listingFixtureAPI{details: map[int64]panapi.File{
				2: {ID: 2, Name: "Collection", IsDir: true},
				7: {ID: 7, ParentID: 2, Name: "Album", IsDir: true},
				8: {ID: 8, ParentID: 7, Name: "sample.bin", Size: 100},
				9: {ID: 9, Name: "root.bin", Size: 100},
			}}
			cache, err := storage.NewCache(t.TempDir(), 16<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer cache.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			service, err := mountfs.NewService(ctx, api, cache, test.root, false, mountfs.Options{})
			if err != nil {
				t.Fatal(err)
			}
			server := newTestServer(t, func(opts *Options) {
				opts.Service = service
				opts.Info.RootID = test.root
			})
			response := authenticatedGET(t, server.Handler(), fmt.Sprintf("/file/%d", test.fileID), nil)
			if response.Code != http.StatusOK {
				t.Fatalf("file page status=%d", response.Code)
			}
			_, nav, found := strings.Cut(response.Body.String(), `<nav class="browser-path" aria-label="文件路径">`)
			if !found {
				t.Fatal("file navigation is missing")
			}
			nav, _, found = strings.Cut(nav, "</nav>")
			if !found {
				t.Fatal("file navigation is incomplete")
			}
			var got, want []string
			for _, match := range regexp.MustCompile(`href="([^"]+)"`).FindAllStringSubmatch(nav, -1) {
				got = append(got, match[1])
			}
			for _, id := range test.parents {
				want = append(want, fmt.Sprintf("/browse?parent_id=%d", id))
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("parent navigation=%v, want complete ordered chain %v", got, want)
			}
		})
	}
}
