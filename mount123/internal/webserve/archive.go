package webserve

import (
	"bytes"
	"fmt"
	"html/template"
	"io"
	"mime"
	"net/http"
	"net/url"
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
	FileID   int64
	FileName string
	Prefix   string
	Parent   string
	Rows     []archiveRow
}

type archiveRow struct {
	Name      string
	Size      string
	IsDir     bool
	Encrypted bool
	IsImage   bool
	Nested    bool
	Kind      string
	FAIcon    string
	Href      string
	Download  string
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
	members, err := s.service.ListArchive(r.Context(), fileID, prefix)
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
	s.renderArchive(w, archivePageData{
		pageData: pageData{Info: s.info, Title: "压缩包浏览", Authenticated: s.authEnabled},
		FileID:   fileID,
		FileName: name,
		Prefix:   prefix,
		Parent:   parent,
		Rows:     archiveRows(fileID, prefix, members),
	})
}

func archiveRows(fileID int64, prefix string, members []mountfs.ArchiveEntry) []archiveRow {
	rows := make([]archiveRow, 0, len(members))
	for _, member := range members {
		kind := classifyEntry(member.Name, member.IsDir)
		row := archiveRow{
			Name:      member.Name,
			IsDir:     member.IsDir,
			Encrypted: member.Encrypted,
			IsImage:   kind.class == "image",
			Kind:      kind.label,
			FAIcon:    faIconFor(kind),
		}
		memberPath := member.Name
		if prefix != "" {
			memberPath = prefix + "/" + member.Name
		}
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
	section := io.NewSectionReader(reader, 0, size)
	base := path.Base(memberPath)
	suffix := strings.ToLower(path.Ext(base))
	contentType := mime.TypeByExtension(suffix)
	if suffix == ".txt" || suffix == ".md" || suffix == ".json" || suffix == ".log" {
		contentType = "text/plain; charset=utf-8"
	}
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(base))
		http.ServeContent(w, r, base, time.Time{}, section)
		return
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	// Serve inline images/text; every other type is offered as a download.
	if strings.HasPrefix(contentType, "image/") {
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
