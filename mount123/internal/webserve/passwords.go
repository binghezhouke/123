package webserve

import (
	"bytes"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/binghezhouke/123/mount123/internal/mountfs"
	"github.com/binghezhouke/123/mount123/internal/panapi"
)

const passwordsPageName = "passwords.html"

var (
	passwordsOnce sync.Once
	passwordsPage *template.Template
	passwordsErr  error
)

func passwordsTemplate() (*template.Template, error) {
	passwordsOnce.Do(func() {
		passwordsPage, passwordsErr = template.ParseFS(assets, "templates/base.html", "templates/"+passwordsPageName)
	})
	return passwordsPage, passwordsErr
}

type passwordPageData struct {
	pageData
	ParentID      int64
	Archives      []mountfs.FileEntry
	SharedCurrent string
	Message       string
}

func (s *Server) registerPasswordRoutes() {
	s.mux.HandleFunc("GET /passwords", s.requireAuth(s.handlePasswords))
	s.mux.HandleFunc("POST /passwords/shared", s.requireAuth(s.handleSaveShared))
	s.mux.HandleFunc("POST /passwords/archive/{id}", s.requireAuth(s.handleSaveArchive))
	s.mux.HandleFunc("POST /passwords/batch", s.requireAuth(s.handleBatch))
}

func (s *Server) renderPasswords(w http.ResponseWriter, data passwordPageData) {
	page, err := passwordsTemplate()
	if err != nil {
		http.Error(w, "页面不可用", http.StatusInternalServerError)
		return
	}
	var buffer bytes.Buffer
	if err := page.ExecuteTemplate(&buffer, "layout", data); err != nil {
		s.logger.Printf("webserve: render passwords: %v", err)
		http.Error(w, "页面渲染失败", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buffer.WriteTo(w)
}

func (s *Server) handlePasswords(w http.ResponseWriter, r *http.Request) {
	if s.service == nil {
		http.Error(w, "服务不可用", http.StatusServiceUnavailable)
		return
	}
	parentID := queryInt64(r, "parent_id", s.info.RootID)
	archives := []mountfs.FileEntry{}
	if entries, err := s.service.ListDirectory(r.Context(), parentID); err == nil {
		for _, entry := range entries {
			if !entry.IsDir && isArchiveName(entry.Name) {
				archives = append(archives, entry)
			}
		}
	}
	shared, _ := s.service.SharedPassword(r.Context(), parentID)
	s.renderPasswords(w, passwordPageData{
		pageData: pageData{Info: s.info, Title: "密码管理", Authenticated: true},
		ParentID: parentID, Archives: archives, SharedCurrent: shared,
	})
}

func (s *Server) handleSaveShared(w http.ResponseWriter, r *http.Request) {
	if s.service == nil {
		http.Error(w, "服务不可用", http.StatusServiceUnavailable)
		return
	}
	parentID := queryInt64(r, "parent_id", s.info.RootID)
	password := r.PostFormValue("password")
	if !validPassword(password) {
		s.renderPasswordError(w, r, parentID, "密码必须为 1–4096 字节的有效 UTF-8 文本")
		return
	}
	overwrite := r.PostFormValue("overwrite") == "1"
	directory := panapi.File{ID: parentID, IsDir: true}
	if _, _, err := s.service.SaveSharedPassword(r.Context(), directory, []byte(password), overwrite); err != nil {
		s.renderPasswordError(w, r, parentID, "保存共享密码失败："+err.Error())
		return
	}
	http.Redirect(w, r, "/passwords?parent_id="+strconv.FormatInt(parentID, 10), http.StatusSeeOther)
}

func (s *Server) handleSaveArchive(w http.ResponseWriter, r *http.Request) {
	if s.service == nil {
		http.Error(w, "服务不可用", http.StatusServiceUnavailable)
		return
	}
	fileID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || fileID <= 0 {
		http.Error(w, "无效的文件 ID", http.StatusBadRequest)
		return
	}
	password := r.PostFormValue("password")
	if !validPassword(password) {
		http.Error(w, "密码必须为 1–4096 字节的有效 UTF-8 文本", http.StatusBadRequest)
		return
	}
	file, err := s.service.File(r.Context(), fileID)
	if err != nil {
		http.Error(w, "获取文件信息失败", http.StatusBadRequest)
		return
	}
	if _, _, err := s.service.SaveArchivePassword(r.Context(), *file, []byte(password), true); err != nil {
		http.Error(w, "保存归档密码失败："+err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/passwords?parent_id="+strconv.FormatInt(file.ParentID, 10), http.StatusSeeOther)
}

func (s *Server) handleBatch(w http.ResponseWriter, r *http.Request) {
	if s.service == nil {
		http.Error(w, "服务不可用", http.StatusServiceUnavailable)
		return
	}
	parentID := queryInt64(r, "parent_id", s.info.RootID)
	password := r.PostFormValue("password")
	if !validPassword(password) {
		http.Error(w, "密码必须为 1–4096 字节的有效 UTF-8 文本", http.StatusBadRequest)
		return
	}
	entries, err := s.service.ListDirectory(r.Context(), parentID)
	if err != nil {
		http.Error(w, "读取目录失败", http.StatusBadRequest)
		return
	}
	var saved, skipped, failed int
	for _, entry := range entries {
		if entry.IsDir || !isArchiveName(entry.Name) {
			continue
		}
		file, err := s.service.File(r.Context(), entry.ID)
		if err != nil {
			failed++
			continue
		}
		if _, skippedOne, err := s.service.SaveArchivePassword(r.Context(), *file, []byte(password), false); err != nil {
			failed++
		} else if skippedOne {
			skipped++
		} else {
			saved++
		}
	}
	http.Redirect(w, r, "/passwords?parent_id="+strconv.FormatInt(parentID, 10)+"&saved="+strconv.Itoa(saved)+"&skipped="+strconv.Itoa(skipped)+"&failed="+strconv.Itoa(failed), http.StatusSeeOther)
}

func (s *Server) renderPasswordError(w http.ResponseWriter, r *http.Request, parentID int64, message string) {
	s.renderPasswords(w, passwordPageData{
		pageData: pageData{Info: s.info, Title: "密码管理", Authenticated: true, Error: message},
		ParentID: parentID,
	})
}

func validPassword(password string) bool {
	return len(password) >= 1 && len(password) <= 4096 && utf8.ValidString(password)
}

func isArchiveName(name string) bool {
	lower := strings.ToLower(name)
	for _, suffix := range []string{".zip", ".7z", ".7zz", ".rar", ".7z.001"} {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

func queryInt64(r *http.Request, key string, fallback int64) int64 {
	value := r.URL.Query().Get(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fallback
	}
	return parsed
}
