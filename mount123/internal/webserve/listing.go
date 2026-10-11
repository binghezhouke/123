package webserve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"

	"github.com/binghezhouke/123/mount123/internal/mountfs"
	"github.com/binghezhouke/123/mount123/internal/panapi"
)

// registerListingRoutes wires directory browsing, file download and preview.
// Implemented by issue #72.
func (s *Server) registerListingRoutes() {
	s.mux.HandleFunc("GET /browse", s.requireAuth(s.handleBrowse))
	s.mux.HandleFunc("POST /browse/probe", s.requireAuth(s.handleProbe))
	s.mux.HandleFunc("GET /browse/probe-status", s.requireAuth(s.handleProbeStatus))
	s.mux.HandleFunc("GET /file/{id}", s.requireAuth(s.handleFileDetail))
	s.mux.HandleFunc("GET /file/{id}/preview", s.requireAuth(s.handlePreview))
	s.mux.HandleFunc("GET /file/{id}/download", s.requireAuth(s.handleDownload))
}

func (s *Server) handleProbe(w http.ResponseWriter, r *http.Request) {
	if s.service == nil {
		http.Error(w, "服务不可用", http.StatusServiceUnavailable)
		return
	}
	parentID := queryInt64(r, "parent_id", s.info.RootID)
	status, err := s.service.ProbeDirectory(r.Context(), parentID)
	if err != nil {
		http.Error(w, "无法启动探测："+err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

func (s *Server) handleProbeStatus(w http.ResponseWriter, r *http.Request) {
	if s.service == nil {
		http.Error(w, "服务不可用", http.StatusServiceUnavailable)
		return
	}
	parentID := queryInt64(r, "parent_id", s.info.RootID)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.service.ProbeStatus(parentID))
}

const (
	listingPageName = "listing.html"
	filePageName    = "file.html"

	// maxBreadcrumbDepth bounds how far the folder chain walk climbs, which
	// also stops a cycle in the cloud metadata from looping forever.
	maxBreadcrumbDepth = 100
	// maxInlineTextBytes mirrors the Flask preview: larger text files are
	// offered as a download instead of being rendered in the browser.
	maxInlineTextBytes = 2 << 20
)

var (
	listingPagesOnce sync.Once
	listingPages     map[string]*template.Template
	listingPagesErr  error
)

// listingPage parses the browse and detail templates once. They reuse
// templates/base.html for the surrounding layout but carry listing-specific
// data, so they are kept apart from Server.pages instead of widening the
// shared pageData struct.
func listingPage(name string) (*template.Template, error) {
	listingPagesOnce.Do(func() {
		listingPages = make(map[string]*template.Template, 2)
		for _, page := range []string{listingPageName, filePageName} {
			parsed, err := template.ParseFS(assets, "templates/base.html", "templates/"+page)
			if err != nil {
				listingPagesErr = err
				return
			}
			listingPages[page] = parsed
		}
	})
	if listingPagesErr != nil {
		return nil, listingPagesErr
	}
	parsed, ok := listingPages[name]
	if !ok {
		return nil, fmt.Errorf("webserve: unknown listing page %q", name)
	}
	return parsed, nil
}

// breadcrumb is one ancestor folder link above a listing or a file.
type breadcrumb struct {
	ID   int64
	Name string
	Href string
}

// entryKindInfo maps an extension class to the badge, label and CSS class the
// listing shows for one entry.
type entryKindInfo struct {
	class string
	badge string
	label string
}

var entryKinds = struct {
	dir     entryKindInfo
	image   entryKindInfo
	video   entryKindInfo
	audio   entryKindInfo
	archive entryKindInfo
	text    entryKindInfo
	pdf     entryKindInfo
	other   entryKindInfo
}{
	dir:     entryKindInfo{"dir", "DIR", "文件夹"},
	image:   entryKindInfo{"image", "IMG", "图片"},
	video:   entryKindInfo{"video", "VID", "视频"},
	audio:   entryKindInfo{"audio", "AUD", "音频"},
	archive: entryKindInfo{"archive", "ARC", "压缩包"},
	text:    entryKindInfo{"text", "TXT", "文本"},
	pdf:     entryKindInfo{"pdf", "PDF", "PDF"},
	other:   entryKindInfo{"file", "FILE", "文件"},
}

// imageSuffixes and videoSuffixes double as the media-type table for inline
// preview and as the media filter the listing page shares with the Flask page.
var (
	imageSuffixes = map[string]string{
		".png":  "image/png",
		".jpg":  "image/jpeg",
		".jpeg": "image/jpeg",
		".gif":  "image/gif",
		".webp": "image/webp",
		".bmp":  "image/bmp",
	}
	videoSuffixes = map[string]string{
		".mp4":  "video/mp4",
		".m4v":  "video/x-m4v",
		".webm": "video/webm",
		".mov":  "video/quicktime",
	}
	audioSuffixes = map[string]string{
		".mp3": "audio/mpeg",
		".wav": "audio/wav",
		".ogg": "audio/ogg",
		".m4a": "audio/mp4",
	}
	archiveSuffixes = map[string]bool{
		".zip": true,
		".7z":  true,
		".rar": true,
		".iso": true,
	}
	textSuffixes = map[string]bool{
		".txt":  true,
		".md":   true,
		".log":  true,
		".json": true,
		".csv":  true,
		".xml":  true,
		".html": true,
		".css":  true,
		".js":   true,
		".py":   true,
		".yaml": true,
		".yml":  true,
		".ini":  true,
		".toml": true,
		".sh":   true,
		".sql":  true,
		".svg":  true,
	}
)

// browseQuery is the validated sorting and filtering half of the listing URL.
type browseQuery struct {
	parentID  int64
	sort      string
	direction string
	kind      string
	arrange   string
	offset    int
	limit     int
}

func parseBrowseQuery(values url.Values) (browseQuery, error) {
	parentID, err := parseFileID(values.Get("parent_id"))
	if err != nil {
		return browseQuery{}, err
	}
	query := browseQuery{parentID: parentID, sort: "original", direction: "asc", kind: "all", limit: 20}
	switch values.Get("sort") {
	case "name", "size", "date":
		query.sort = values.Get("sort")
	}
	if values.Get("direction") == "desc" {
		query.direction = "desc"
	}
	switch values.Get("kind") {
	case "image", "video":
		query.kind = values.Get("kind")
	}
	if values.Get("arrange") == "kind" {
		query.arrange = "kind"
	}
	if offset, err := strconv.Atoi(values.Get("offset")); err == nil && offset >= 0 {
		query.offset = offset
	}
	return query, nil
}

// parseFileID accepts the numeric IDs the cloud API uses. An empty value means
// the caller did not ask for a specific folder.
func parseFileID(value string) (int64, error) {
	if strings.TrimSpace(value) == "" {
		return 0, nil
	}
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id < 0 {
		return 0, fmt.Errorf("webserve: invalid file id %q", value)
	}
	return id, nil
}

// listingRow is one rendered directory entry.
type listingRow struct {
	ID      int64
	Name    string
	Href    string
	Size    string
	IsDir   bool
	Updated string
	Kind    string
	Icon    string
	Badge   string
	FAIcon  string
	IsImage bool
	Preview string
}

type listingPageData struct {
	pageData
	ParentID    int64
	CurrentName string
	Breadcrumbs []breadcrumb
	Rows        []listingRow
	Count       int
	Hidden      int
	Sort        string
	Direction   string
	Kind        string
	Arrange     string
	Groups      []listingGroup
	Offset      int
	NextOffset  int
}

type listingGroup struct {
	Title string
	Rows  []listingRow
}

type fileView struct {
	ID        int64
	Name      string
	Size      string
	SizeBytes int64
	IsDir     bool
	Kind      string
	Icon      string
	FAIcon    string
	Badge     string
	Updated   string
	Created   string
	ParentID  int64
}

type filePageData struct {
	pageData
	File         fileView
	Breadcrumbs  []breadcrumb
	ParentHref   string
	FolderHref   string
	DownloadHref string
	PreviewHref  string
	ArchiveHref  string
	PreviewKind  string
}

// basePage mirrors the payload handleIndex renders so the listing pages keep
// the same header, footer and cache facts.
func (s *Server) basePage(title string) pageData {
	return pageData{
		Info:          s.info,
		Title:         title,
		Authenticated: s.authEnabled,
		CacheCapacity: humanBytes(s.info.CacheCapacityBytes),
		IndexBudget:   humanBytes(s.info.IndexBudgetBytes),
	}
}

func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	query, err := parseBrowseQuery(r.URL.Query())
	if err != nil {
		http.Error(w, "无效的目录参数。", http.StatusBadRequest)
		return
	}
	if query.parentID == 0 {
		query.parentID = s.info.RootID
	}
	data := listingPageData{
		pageData:  s.basePage("网盘浏览"),
		ParentID:  query.parentID,
		Sort:      query.sort,
		Direction: query.direction,
		Kind:      query.kind,
		Arrange:   query.arrange,
	}
	ctx := r.Context()
	data.Breadcrumbs, data.CurrentName = s.folderNames(ctx, query.parentID)
	if s.service == nil {
		data.pageData.Error = "网盘服务未配置。"
		s.renderListingPage(w, http.StatusServiceUnavailable, listingPageName, data)
		return
	}
	entries, err := s.service.ListDirectory(ctx, query.parentID)
	if err != nil {
		s.logger.Printf("webserve: list directory %d: %v", query.parentID, err)
		data.pageData.Error = "读取目录失败，请刷新重试。"
		s.renderListingPage(w, httpStatusForError(err), listingPageName, data)
		return
	}
	rows, hidden := buildRows(entries, query)
	detected, _ := s.service.DetectedArchives(ctx, query.parentID)
	for i := range rows {
		if _, ok := detected[rows[i].ID]; ok && !rows[i].IsDir {
			rows[i].Kind = "压缩包"
			rows[i].Icon = "archive"
			rows[i].Badge = "ARC"
			rows[i].FAIcon = "fas fa-file-archive"
			rows[i].Href = "/archive/" + strconv.FormatInt(rows[i].ID, 10)
		}
	}
	data.Hidden = hidden
	data.Count = len(rows)
	start, end := query.offset, query.offset+query.limit
	if start > len(rows) {
		start = len(rows)
	}
	if end > len(rows) {
		end = len(rows)
	}
	data.Rows = rows[start:end]
	data.Offset = query.offset
	if end < len(rows) {
		data.NextOffset = end
	}
	if query.arrange == "kind" {
		data.Groups = groupRows(data.Rows)
	}
	s.renderListingPage(w, http.StatusOK, listingPageName, data)
}

func (s *Server) handleFileDetail(w http.ResponseWriter, r *http.Request) {
	meta, ok := s.lookupFile(w, r)
	if !ok {
		return
	}
	if meta.IsDir {
		http.Redirect(w, r, browseHref(meta.ID, browseQuery{}), http.StatusSeeOther)
		return
	}
	ctx := r.Context()
	kind := classifyEntry(meta.Name, false)
	data := filePageData{
		pageData:     s.basePage(meta.Name),
		File:         newFileView(meta, kind),
		ParentHref:   browseHref(meta.ParentID, browseQuery{}),
		DownloadHref: fileHref(meta.ID, "download"),
		PreviewHref:  fileHref(meta.ID, "preview"),
		PreviewKind:  previewKind(meta.Name),
	}
	if kind.class == "archive" {
		data.ArchiveHref = "/archive/" + strconv.FormatInt(meta.ID, 10)
	}
	data.Breadcrumbs, _ = s.folderNames(ctx, meta.ParentID)
	s.renderListingPage(w, http.StatusOK, filePageName, data)
}

func groupRows(rows []listingRow) []listingGroup {
	order := []string{"文件夹", "图片", "视频", "音频", "压缩包", "文本", "PDF", "文件"}
	byKind := make(map[string][]listingRow, len(order))
	for _, row := range rows {
		byKind[row.Kind] = append(byKind[row.Kind], row)
	}
	groups := make([]listingGroup, 0, len(order))
	for _, kind := range order {
		if grouped := byKind[kind]; len(grouped) > 0 {
			groups = append(groups, listingGroup{Title: kind, Rows: grouped})
		}
	}
	for kind, grouped := range byKind {
		known := false
		for _, name := range order {
			if kind == name {
				known = true
				break
			}
		}
		if !known {
			groups = append(groups, listingGroup{Title: kind, Rows: grouped})
		}
	}
	return groups
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	meta, ok := s.lookupFile(w, r)
	if !ok {
		return
	}
	if meta.IsDir {
		http.Error(w, "目录不能下载。", http.StatusBadRequest)
		return
	}
	s.serveFileContent(w, r, meta, false)
}

func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	meta, ok := s.lookupFile(w, r)
	if !ok {
		return
	}
	if meta.IsDir {
		http.Error(w, "目录不能预览。", http.StatusBadRequest)
		return
	}
	if r.URL.Query().Get("download") == "1" {
		s.serveFileContent(w, r, meta, false)
		return
	}
	switch previewKind(meta.Name) {
	case "text":
		s.serveInlineText(w, r, meta)
	case "image", "video", "audio", "pdf":
		s.serveFileContent(w, r, meta, true)
	default:
		http.Redirect(w, r, fileHref(meta.ID, "download"), http.StatusSeeOther)
	}
}

// lookupFile resolves the {id} path value, answering the request itself when
// the ID is malformed or the metadata cannot be read.
func (s *Server) lookupFile(w http.ResponseWriter, r *http.Request) (*panapi.File, bool) {
	if s.service == nil {
		http.Error(w, "网盘服务未配置。", http.StatusServiceUnavailable)
		return nil, false
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "无效的文件 ID。", http.StatusBadRequest)
		return nil, false
	}
	meta, err := s.service.File(r.Context(), id)
	if err != nil {
		s.logger.Printf("webserve: file %d metadata: %v", id, err)
		http.Error(w, "无法读取文件信息。", httpStatusForError(err))
		return nil, false
	}
	return meta, true
}

// serveFileContent streams a cloud file, letting http.ServeContent handle
// Range negotiation (206 with Content-Range, 416 for an unsatisfiable range)
// on top of the cached random-access reader.
func (s *Server) serveFileContent(w http.ResponseWriter, r *http.Request, meta *panapi.File, inline bool) {
	reader, size, err := s.service.OpenFile(r.Context(), meta.ID)
	if err != nil {
		s.logger.Printf("webserve: open file %d: %v", meta.ID, err)
		http.Error(w, "读取文件失败，请刷新重试。", httpStatusForError(err))
		return
	}
	contentType := "application/octet-stream"
	if known, ok := mediaTypes()[suffixOf(meta.Name)]; ok {
		contentType = known
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", contentDisposition(meta.Name, inline))
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, meta.Name, meta.UpdatedAt, io.NewSectionReader(reader, 0, size))
}

// serveInlineText renders small text files directly in the browser, decoding
// UTF-8 first and falling back to GB18030 like the Flask preview did.
func (s *Server) serveInlineText(w http.ResponseWriter, r *http.Request, meta *panapi.File) {
	reader, size, err := s.service.OpenFile(r.Context(), meta.ID)
	if err != nil {
		s.logger.Printf("webserve: open file %d: %v", meta.ID, err)
		http.Error(w, "读取文件失败，请刷新重试。", httpStatusForError(err))
		return
	}
	if size > maxInlineTextBytes {
		http.Error(w, "文本超过 2 MiB，请下载后查看。", http.StatusRequestEntityTooLarge)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(io.NewSectionReader(reader, 0, size), maxInlineTextBytes))
	if err != nil {
		s.logger.Printf("webserve: read file %d: %v", meta.ID, err)
		http.Error(w, "读取文件失败，请刷新重试。", httpStatusForError(err))
		return
	}
	body := decodeText(raw)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", contentDisposition(meta.Name, true))
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = io.WriteString(w, body)
}

// renderListingPage executes one of the listing templates with the base.html
// layout, buffering the output so a template error cannot half-write a page.
func (s *Server) renderListingPage(w http.ResponseWriter, status int, name string, data any) {
	page, err := listingPage(name)
	if err != nil {
		s.logger.Printf("webserve: %v", err)
		http.Error(w, "页面渲染失败", http.StatusInternalServerError)
		return
	}
	var buffer bytes.Buffer
	if err := page.ExecuteTemplate(&buffer, "layout", data); err != nil {
		s.logger.Printf("webserve: render %s: %v", name, err)
		http.Error(w, "页面渲染失败", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buffer.WriteTo(w)
}

// folderNames returns the breadcrumb chain of the folder's ancestors plus the
// folder's own display name for the page heading.
func (s *Server) folderNames(ctx context.Context, id int64) ([]breadcrumb, string) {
	path := s.folderPath(ctx, id)
	name := "根目录"
	if len(path) > 0 {
		name = path[len(path)-1].Name
		path = path[:len(path)-1]
	}
	return path, name
}

// folderPath walks the cloud metadata from the mount root down to id. The
// mount root is not included; id is. A missing ancestor or a cycle in the
// metadata stops the walk and keeps what was already resolved.
func (s *Server) folderPath(ctx context.Context, id int64) []breadcrumb {
	rootID := s.info.RootID
	if s.service == nil || id == 0 || id == rootID {
		return nil
	}
	var reversed []breadcrumb
	visited := make(map[int64]bool, 4)
	for current := id; current != 0 && current != rootID && len(reversed) < maxBreadcrumbDepth; {
		if visited[current] {
			s.logger.Printf("webserve: folder cycle at %d", current)
			break
		}
		visited[current] = true
		meta, err := s.service.File(ctx, current)
		if err != nil {
			s.logger.Printf("webserve: breadcrumb %d: %v", current, err)
			break
		}
		reversed = append(reversed, breadcrumb{ID: current, Name: displayName(meta), Href: browseHref(current, browseQuery{})})
		current = meta.ParentID
	}
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	return reversed
}

func displayName(file *panapi.File) string {
	if file.Name != "" {
		return file.Name
	}
	return strconv.FormatInt(file.ID, 10)
}

func buildRows(entries []mountfs.FileEntry, query browseQuery) ([]listingRow, int) {
	ordered := orderEntries(entries, query)
	rows := make([]listingRow, 0, len(ordered))
	for _, entry := range ordered {
		rows = append(rows, newListingRow(entry))
	}
	return rows, len(entries) - len(ordered)
}

func newListingRow(entry mountfs.FileEntry) listingRow {
	kind := classifyEntry(entry.Name, entry.IsDir)
	row := listingRow{
		ID:      entry.ID,
		Name:    entry.Name,
		IsDir:   entry.IsDir,
		Kind:    kind.label,
		Icon:    kind.class,
		Badge:   kind.badge,
		FAIcon:  faIconFor(kind),
		Size:    "—",
		Updated: formatTimestamp(entry.UpdatedAt),
	}
	if entry.IsDir {
		row.Href = browseHref(entry.ID, browseQuery{})
		return row
	}
	row.Href = "/file/" + strconv.FormatInt(entry.ID, 10)
	row.Size = humanBytes(entry.Size)
	if kind.class == "image" {
		row.IsImage = true
		row.Preview = "/file/" + strconv.FormatInt(entry.ID, 10) + "/preview"
	}
	return row
}

func faIconFor(kind entryKindInfo) string {
	switch kind.class {
	case "dir":
		return "fas fa-folder"
	case "image":
		return "fas fa-file-image"
	case "video":
		return "fas fa-file-video"
	case "audio":
		return "fas fa-file-audio"
	case "archive":
		return "fas fa-file-archive"
	case "pdf":
		return "fas fa-file-pdf"
	case "text":
		return "fas fa-file-alt"
	default:
		return "fas fa-file"
	}
}

func newFileView(file *panapi.File, kind entryKindInfo) fileView {
	view := fileView{
		ID:       file.ID,
		Name:     displayName(file),
		IsDir:    file.IsDir,
		Kind:     kind.label,
		Icon:     kind.class,
		FAIcon:   faIconFor(kind),
		Badge:    kind.badge,
		Updated:  formatTimestamp(file.UpdatedAt),
		Created:  formatTimestamp(file.CreatedAt),
		ParentID: file.ParentID,
		Size:     "—",
	}
	if !file.IsDir {
		view.Size = humanBytes(file.Size)
		view.SizeBytes = file.Size
	}
	return view
}

// orderEntries mirrors the Flask listing: folders stay on top, the media
// filter drops non-matching files, and sorting falls back to the natural name
// and then the ID so the order is total.
func orderEntries(entries []mountfs.FileEntry, query browseQuery) []mountfs.FileEntry {
	ordered := make([]mountfs.FileEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir || matchesKind(entry.Name, query.kind) {
			ordered = append(ordered, entry)
		}
	}
	if query.sort == "original" {
		return ordered
	}
	descending := query.direction == "desc"
	sort.SliceStable(ordered, func(i, j int) bool {
		compare := compareEntries(ordered[i], ordered[j], query.sort)
		if descending {
			return compare > 0
		}
		return compare < 0
	})
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].IsDir && !ordered[j].IsDir })
	return ordered
}

func compareEntries(a, b mountfs.FileEntry, sortKey string) int {
	if sortKey == "size" && a.Size != b.Size {
		if a.Size < b.Size {
			return -1
		}
		return 1
	}
	if sortKey == "date" && !a.UpdatedAt.Equal(b.UpdatedAt) {
		if a.UpdatedAt.Before(b.UpdatedAt) {
			return -1
		}
		return 1
	}
	if compare := compareNatural(a.Name, b.Name); compare != 0 {
		return compare
	}
	switch {
	case a.ID < b.ID:
		return -1
	case a.ID > b.ID:
		return 1
	}
	return 0
}

// naturalChunk is one digit or non-digit run of a filename.
type naturalChunk struct {
	digits bool
	value  string
}

// compareNatural orders names the way the Flask listing does: digit runs
// compare numerically, other runs compare case-folded, digit runs sort after
// text at the same position, and a prefix sorts before a longer name.
func compareNatural(a, b string) int {
	aChunks := naturalChunks(a)
	bChunks := naturalChunks(b)
	for i := 0; i < len(aChunks) && i < len(bChunks); i++ {
		if compare := compareChunk(aChunks[i], bChunks[i]); compare != 0 {
			return compare
		}
	}
	switch {
	case len(aChunks) < len(bChunks):
		return -1
	case len(aChunks) > len(bChunks):
		return 1
	}
	return 0
}

func naturalChunks(name string) []naturalChunk {
	chunks := make([]naturalChunk, 0, 4)
	for index := 0; index < len(name); {
		end := index
		digits := name[index] >= '0' && name[index] <= '9'
		for end < len(name) && (name[end] >= '0' && name[end] <= '9') == digits {
			end++
		}
		chunk := naturalChunk{digits: digits, value: name[index:end]}
		if !digits {
			chunk.value = strings.ToLower(chunk.value)
		}
		chunks = append(chunks, chunk)
		index = end
	}
	return chunks
}

func compareChunk(a, b naturalChunk) int {
	if a.digits != b.digits {
		if a.digits {
			return 1
		}
		return -1
	}
	if a.digits {
		return compareDigitRuns(a.value, b.value)
	}
	if a.value == b.value {
		return 0
	}
	if a.value < b.value {
		return -1
	}
	return 1
}

// compareDigitRuns compares numeric runs by value without converting them to a
// fixed-width integer, so very long runs still order correctly.
func compareDigitRuns(a, b string) int {
	a = strings.TrimLeft(a, "0")
	b = strings.TrimLeft(b, "0")
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// matchesKind applies the media filter of the listing page.
func matchesKind(name, kind string) bool {
	switch kind {
	case "image":
		return imageSuffixes[suffixOf(name)] != ""
	case "video":
		return videoSuffixes[suffixOf(name)] != ""
	default:
		return true
	}
}

func classifyEntry(name string, isDir bool) entryKindInfo {
	if isDir {
		return entryKinds.dir
	}
	extension := suffixOf(name)
	switch {
	case imageSuffixes[extension] != "":
		return entryKinds.image
	case videoSuffixes[extension] != "":
		return entryKinds.video
	case audioSuffixes[extension] != "":
		return entryKinds.audio
	case archiveSuffixes[extension]:
		return entryKinds.archive
	case textSuffixes[extension]:
		return entryKinds.text
	case extension == ".pdf":
		return entryKinds.pdf
	default:
		return entryKinds.other
	}
}

// previewKind reports how (or whether) a file can be shown inline.
func previewKind(name string) string {
	extension := suffixOf(name)
	switch {
	case imageSuffixes[extension] != "":
		return "image"
	case videoSuffixes[extension] != "":
		return "video"
	case audioSuffixes[extension] != "":
		return "audio"
	case textSuffixes[extension]:
		return "text"
	case extension == ".pdf":
		return "pdf"
	default:
		return ""
	}
}

func mediaTypes() map[string]string {
	types := make(map[string]string, len(imageSuffixes)+len(videoSuffixes)+len(audioSuffixes)+1)
	for extension, contentType := range imageSuffixes {
		types[extension] = contentType
	}
	for extension, contentType := range videoSuffixes {
		types[extension] = contentType
	}
	for extension, contentType := range audioSuffixes {
		types[extension] = contentType
	}
	types[".pdf"] = "application/pdf"
	return types
}

func suffixOf(name string) string {
	return strings.ToLower(path.Ext(name))
}

// browseHref builds a listing URL, omitting the default query values so links
// stay stable and readable.
func browseHref(parentID int64, query browseQuery) string {
	values := url.Values{}
	values.Set("parent_id", strconv.FormatInt(parentID, 10))
	if query.sort != "" && query.sort != "original" {
		values.Set("sort", query.sort)
	}
	if query.direction == "desc" {
		values.Set("direction", "desc")
	}
	if query.kind != "" && query.kind != "all" {
		values.Set("kind", query.kind)
	}
	return "/browse?" + values.Encode()
}

func fileHref(id int64, action string) string {
	return "/file/" + strconv.FormatInt(id, 10) + "/" + action
}

func contentDisposition(name string, inline bool) string {
	disposition := "attachment"
	if inline {
		disposition = "inline"
	}
	return fmt.Sprintf("%s; filename*=UTF-8''%s", disposition, url.PathEscape(name))
}

func formatTimestamp(value time.Time) string {
	if value.IsZero() {
		return "—"
	}
	return value.Local().Format("2006-01-02 15:04")
}

// decodeText renders text previews the way the Flask page did: UTF-8 with an
// optional byte-order mark first, then GB18030 for the legacy Chinese
// encodings found in real accounts.
func decodeText(raw []byte) string {
	data := bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	if utf8.Valid(data) {
		return string(data)
	}
	decoded, err := simplifiedchinese.GB18030.NewDecoder().Bytes(data)
	if err != nil {
		return strings.ToValidUTF8(string(data), "\uFFFD")
	}
	return string(decoded)
}

// httpStatusForError maps the cloud tree's errors onto HTTP status codes.
func httpStatusForError(err error) int {
	switch {
	case errors.Is(err, syscall.ENOENT):
		return http.StatusNotFound
	case errors.Is(err, syscall.EISDIR), errors.Is(err, syscall.ENOTDIR):
		return http.StatusBadRequest
	case errors.Is(err, syscall.ENOTSUP), errors.Is(err, syscall.EOPNOTSUPP):
		return http.StatusNotImplemented
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout
	default:
		return http.StatusBadGateway
	}
}
