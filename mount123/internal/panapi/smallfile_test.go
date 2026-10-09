package panapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSaveArchivePasswordCanonicalNames(t *testing.T) {
	for _, tc := range []struct{ name, want string }{{"NO.001", "NO.001.pwd"}, {"a.rar", "a.rar.pwd"}, {"a.7z.001", "a.7z.pwd"}, {"a.zip", "a.zip.pwd"}} {
		t.Run(tc.name, func(t *testing.T) {
			creates := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v2/file/list":
					_, _ = w.Write([]byte(`{"code":0,"data":{"fileList":[],"lastFileId":-1}}`))
				case "/upload/v2/file/create":
					creates++
					var payload struct {
						Filename string `json:"filename"`
						ParentID int64  `json:"parentFileID"`
					}
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Error(err)
					}
					if payload.Filename != tc.want || payload.ParentID != 8 {
						t.Errorf("upload target=%+v", payload)
					}
					_, _ = w.Write([]byte(`{"code":0,"data":{"reuse":true,"fileID":21}}`))
				default:
					t.Error("unexpected network operation")
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			c, _ := New(Config{AccessToken: "fixture", BaseURL: srv.URL})
			f, skipped, err := c.SaveArchivePassword(context.Background(), File{ID: 20, ParentID: 8, Name: tc.name}, []byte("password"), false)
			if err != nil || skipped || f.Name != tc.want || creates != 1 {
				t.Fatalf("file=%+v skipped=%t err=%v creates=%d", f, skipped, err, creates)
			}
		})
	}
}

func TestSaveArchivePasswordSkipsExistingWithoutReadingOrUploading(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/file/list" {
			t.Error("existing password should not be read or overwritten")
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"fileList":[{"fileId":21,"parentFileID":8,"filename":"NO.001.pwd","size":2,"type":0}],"lastFileId":-1}}`))
	}))
	defer srv.Close()
	c, _ := New(Config{AccessToken: "fixture", BaseURL: srv.URL})
	f, skipped, err := c.SaveArchivePassword(context.Background(), File{ID: 20, ParentID: 8, Name: "NO.001"}, []byte("different"), false)
	if err != nil || !skipped || f.ID != 21 {
		t.Fatalf("file=%+v skipped=%t err=%v", f, skipped, err)
	}
}
