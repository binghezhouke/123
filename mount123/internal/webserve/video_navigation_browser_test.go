package webserve

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestNestedVideoOpenInBrowser(t *testing.T) {
	python := os.Getenv("MOUNT123_BROWSER_PYTHON")
	if python == "" {
		t.Skip("set MOUNT123_BROWSER_PYTHON to a Python with Playwright")
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is required to generate the synthetic video")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	videoPath := filepath.Join(t.TempDir(), "clip.mp4")
	out, err := exec.CommandContext(ctx, ffmpeg, "-v", "error", "-f", "lavfi", "-i", "color=c=blue:s=64x48:r=10", "-t", "2", "-c:v", "libx264", "-threads", "1", "-pix_fmt", "yuv420p", videoPath).CombinedOutput()
	if err != nil {
		t.Fatalf("create synthetic video: %v\n%s", err, out)
	}
	video, err := os.ReadFile(videoPath)
	if err != nil {
		t.Fatal(err)
	}
	pack := func(name string, payload []byte) []byte {
		var buffer bytes.Buffer
		writer := zip.NewWriter(&buffer)
		member, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := member.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		return buffer.Bytes()
	}
	server, _ := newArchiveFixtureData(t, pack("inner.zip", pack("clip.mp4", video)))
	server.authEnabled = false
	host := httptest.NewServer(server.Handler())
	defer host.Close()
	out, err = exec.CommandContext(ctx, python, "testdata/video_navigation_browser_test.py", host.URL).CombinedOutput()
	if err != nil {
		t.Fatalf("nested video browser navigation: %v\n%s", err, out)
	}
	t.Log(string(out))
}
