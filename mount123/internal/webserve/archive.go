package webserve

import (
	"bytes"
	"fmt"
	"html/template"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/binghezhouke/123/mount123/internal/mountfs"
	"github.com/binghezhouke/123/mount123/internal/workqueue"
)

const archivePageName = "archive.html"

var (
	archiveOnce sync.Once
	archivePage *template.Template
	archiveErr  error
)

func archiveTemplate() (*template.Template, error) {
	archiveOnce.Do(func() {
		archivePage, archiveErr = template.ParseFS(assets, "templates/base.html", "templates/"+archivePageName)
	})
	return archivePage, archiveErr
}

type archivePageData struct {
	pageData
	FileID     int64
	FileName   string
	Prefix     string
	Parent     string
	Rows       []archiveRow
	ImageCount int
}

type archiveRow struct {
	Name        string
	Size        string
	IsDir       bool
	Encrypted   bool
	IsImage     bool
	Nested      bool
	Kind        string
	Icon        string
	FAIcon      string
	Href        string
	Download    string
	FavoriteKey string
	MemberPath  string
}

func (s *Server) registerArchiveRoutes() {
	s.mux.HandleFunc("GET /archive/{id}", s.requireAuth(s.handleArchiveList))
	s.mux.HandleFunc("GET /archive/{id}/member", s.requireAuth(s.handleArchiveMember))
}

func (s *Server) renderArchive(w http.ResponseWriter, data archivePageData) {
	page, err := archiveTemplate()
	if err != nil {
		http.Error(w, "页面不可用", http.StatusInternalServerError)
		return
	}
	var buffer bytes.Buffer
	if err := page.ExecuteTemplate(&buffer, "layout", data); err != nil {
		s.logger.Printf("webserve: render archive: %v", err)
		http.Error(w, "页面渲染失败", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buffer.WriteTo(w)
}

func (s *Server) handleArchiveList(w http.ResponseWriter, r *http.Request) {
	if s.service == nil {
		http.Error(w, "服务不可用", http.StatusServiceUnavailable)
		return
	}
	fileID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || fileID <= 0 {
		http.Error(w, "无效的文件 ID", http.StatusBadRequest)
		return
	}
	prefix := strings.Trim(r.URL.Query().Get("path"), "/")
	// Archive scans can be expensive for remote solid RARs. Use the same
	// progressive index path as FUSE so the first safe batch renders without
	// waiting for the complete archive.
	members, err := s.service.ListArchiveProgressive(r.Context(), fileID, prefix)
	if err != nil {
		if err == syscall.EACCES {
			http.Error(w, "压缩包已加密：请先在密码管理页为它设置密码。", http.StatusForbidden)
			return
		}
		http.Error(w, "无法读取压缩包："+err.Error(), http.StatusBadRequest)
		return
	}
	file, _ := s.service.File(r.Context(), fileID)
	name := ""
	if file != nil {
		name = file.Name
	}
	parent := path.Dir(prefix)
	if parent == "." {
		parent = ""
	}
	rows := archiveRows(fileID, prefix, members)
	imageCount := 0
	for _, row := range rows {
		if row.IsImage {
			imageCount++
		}
	}
	s.renderArchive(w, archivePageData{
		pageData:   pageData{Info: s.info, Title: "压缩包浏览", Authenticated: s.authEnabled},
		FileID:     fileID,
		FileName:   name,
		Prefix:     prefix,
		Parent:     parent,
		Rows:       rows,
		ImageCount: imageCount,
	})
}

func archiveRows(fileID int64, prefix string, members []mountfs.ArchiveEntry) []archiveRow {
	rows := make([]archiveRow, 0, len(members))
	for _, member := range members {
		kind := classifyEntry(member.Name, member.IsDir)
		row := archiveRow{
			Name:        member.Name,
			IsDir:       member.IsDir,
			Encrypted:   member.Encrypted,
			IsImage:     kind.class == "image",
			Kind:        kind.label,
			Icon:        kind.class,
			FAIcon:      faIconFor(kind),
			FavoriteKey: fmt.Sprintf("zip:%d:%s:%s", fileID, prefix, member.Name),
		}
		memberPath := member.Name
		if prefix != "" {
			memberPath = prefix + "/" + member.Name
		}
		row.MemberPath = memberPath
		if member.IsDir || (!member.IsDir && isArchiveName(member.Name)) {
			row.Href = "/archive/" + strconv.FormatInt(fileID, 10) + "?path=" + url.QueryEscape(memberPath)
			row.Nested = !member.IsDir
			rows = append(rows, row)
			continue
		}
		row.Size = humanBytes(int64(member.Size))
		row.Href = "/archive/" + strconv.FormatInt(fileID, 10) + "/member?path=" + url.QueryEscape(memberPath)
		row.Download = row.Href + "&download=1"
		rows = append(rows, row)
	}
	return rows
}

func (s *Server) handleArchiveMember(w http.ResponseWriter, r *http.Request) {
	if s.service == nil {
		http.Error(w, "服务不可用", http.StatusServiceUnavailable)
		return
	}
	fileID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || fileID <= 0 {
		http.Error(w, "无效的文件 ID", http.StatusBadRequest)
		return
	}
	memberPath := strings.Trim(r.URL.Query().Get("path"), "/")
	ctx := r.Context()
	if r.Header.Get("X-Priority") == "background" {
		ctx = workqueue.Background(ctx)
	}
	started := time.Now()
	reader, size, err := s.service.OpenArchiveMember(ctx, fileID, memberPath)
	extractMillis := float64(time.Since(started).Microseconds()) / 1000.0
	w.Header().Set("Server-Timing", fmt.Sprintf("extract;dur=%.1f", extractMillis))
	if err != nil {
		if err == syscall.EACCES {
			http.Error(w, "该成员已加密：请先在密码管理页为压缩包设置密码。", http.StatusForbidden)
			return
		}
		http.Error(w, "读取成员失败："+err.Error(), http.StatusBadRequest)
		return
	}
	if closer, ok := reader.(io.Closer); ok {
		defer closer.Close()
	}
	section := io.NewSectionReader(reader, 0, size)
	base := path.Base(memberPath)
	suffix := strings.ToLower(path.Ext(base))
	contentType := mime.TypeByExtension(suffix)
	if suffix == ".mkv" {
		contentType = "video/x-matroska"
	}
	if suffix == ".txt" || suffix == ".md" || suffix == ".json" || suffix == ".log" {
		contentType = "text/plain; charset=utf-8"
	}
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(base))
		http.ServeContent(w, r, base, time.Time{}, section)
		return
	}
	// Browsers generally cannot decode Matroska directly, even when the
	// underlying video/audio codecs are browser-compatible. Remux it to a
	// fragmented MP4 stream on demand so the archive preview can behave like
	// an ordinary MP4 without first materialising the member on disk.
	if suffix == ".mkv" && r.URL.Query().Get("transcode") == "mp4" {
		s.serveMKVAsMP4(w, r, section, base)
		return
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	// Serve browser-playable media, images, and text inline. ServeContent keeps
	// Range support, which lets native video players seek inside the member.
	if strings.HasPrefix(contentType, "video/") || strings.HasPrefix(contentType, "audio/") || strings.HasPrefix(contentType, "image/") {
		w.Header().Set("Content-Disposition", "inline; filename*=UTF-8''"+url.PathEscape(base))
		http.ServeContent(w, r, base, time.Time{}, section)
		return
	}
	if strings.HasPrefix(contentType, "text/") {
		if size > maxInlineTextBytes {
			http.Error(w, "文本过大，请下载", http.StatusRequestEntityTooLarge)
			return
		}
		http.ServeContent(w, r, base, time.Time{}, section)
		return
	}
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(base))
	http.ServeContent(w, r, base, time.Time{}, section)
}

func (s *Server) serveMKVAsMP4(w http.ResponseWriter, r *http.Request, input io.Reader, name string) {
	ctx := r.Context()
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error",
		"-i", "pipe:0", "-map", "0:v:0?", "-map", "0:a:0?", "-c", "copy",
		"-movflags", "frag_keyframe+empty_moov+default_base_moof", "-f", "mp4", "pipe:1")
	cmd.Stdin = input
	cmd.Stdout = w
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Content-Disposition", "inline; filename*=UTF-8''"+url.PathEscape(strings.TrimSuffix(name, path.Ext(name))+".mp4"))
	w.Header().Set("Cache-Control", "no-store")
	if err := cmd.Run(); err != nil && ctx.Err() == nil {
		s.logger.Printf("webserve: remux MKV %q: %v (%s)", name, err, strings.TrimSpace(stderr.String()))
	}
}
