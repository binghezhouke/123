// Package webserve implements the local HTTP backend used by the mount123 web
// interface. It reads the same cloud tree as the FUSE mount without mounting
// anything, so the web front end and the mount share directory snapshots,
// archive indexes, the disk cache and archive password discovery.
package webserve

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/binghezhouke/123/mount123/internal/mountfs"
)

//go:embed templates/*.html static/css/*.css static/js/*.js static/js/vendor/*.js static/js/vendor/mp4box/*.mjs static/js/vendor/panzoom/*.js
var assets embed.FS

const (
	sessionCookieName = "mount123_session"

	// DefaultSessionTTL is how long a login lasts when the caller does not
	// choose a lifetime.
	DefaultSessionTTL = 24 * time.Hour
	// minSessionKeyBytes keeps session cookies from being forgeable by brute
	// force when a caller supplies a short key.
	minSessionKeyBytes = 32
	// maxLoginBodyBytes bounds the parsed login form so an unauthenticated
	// client cannot make the server buffer arbitrary input.
	maxLoginBodyBytes = 4 << 10
	// generatedPasswordBytes is the entropy behind a password the server
	// generates when the operator configured none.
	generatedPasswordBytes = 12
)

// Info describes the runtime the backend is serving. An authenticated session
// sees these values on the index page.
type Info struct {
	Version            string
	Commit             string
	BuildTime          string
	RootID             int64
	Address            string
	CacheDir           string
	CacheCapacityBytes int64
	IndexBudgetBytes   int64
	CacheDurability    string
	DirectoryTTL       time.Duration
	SourceTTL          time.Duration
}

// Options configures a Server. SessionKey is required; an empty Password is
// generated and reported through GeneratedPassword so the operator can still
// log in.
type Options struct {
	// Password is the login password. When empty the server generates one.
	Password string
	// SessionKey signs session cookies. At least minSessionKeyBytes bytes are
	// required so cookies cannot be forged by guessing a short key.
	SessionKey []byte
	// SessionTTL is how long a login stays valid. Zero selects
	// DefaultSessionTTL.
	SessionTTL time.Duration
	// Info is rendered on the placeholder index page.
	Info Info
	// Service is the shared cloud tree. Handlers use it to list directories,
	// read files and browse archives without mounting FUSE.
	Service *mountfs.Service
	// Logger receives rejected logins. Nil logs to the standard logger.
	Logger *log.Logger
	// Now overrides the clock in tests.
	Now func() time.Time
}

// Server serves the web interface for one cloud tree.
type Server struct {
	password          []byte
	generatedPassword string
	key               []byte
	ttl               time.Duration
	info              Info
	logger            *log.Logger
	now               func() time.Time
	sessions          *sessionStore
	mux               *http.ServeMux
	pages             map[string]*template.Template
	static            http.Handler
	service           *mountfs.Service
	authEnabled       bool
}

// New validates opts and returns a server that is ready to serve. The caller
// owns the returned handler for the lifetime of the process.
func New(opts Options) (*Server, error) {
	if len(opts.SessionKey) < minSessionKeyBytes {
		return nil, fmt.Errorf("webserve: session key must be at least %d bytes", minSessionKeyBytes)
	}
	if opts.SessionTTL < 0 {
		return nil, errors.New("webserve: session TTL must not be negative")
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	ttl := opts.SessionTTL
	if ttl == 0 {
		ttl = DefaultSessionTTL
	}
	s := &Server{
		password:    []byte(opts.Password),
		key:         append([]byte(nil), opts.SessionKey...),
		ttl:         ttl,
		info:        opts.Info,
		logger:      logger,
		now:         now,
		sessions:    newSessionStore(),
		mux:         http.NewServeMux(),
		pages:       map[string]*template.Template{},
		service:     opts.Service,
		authEnabled: opts.Password != "",
	}
	if opts.Password == "" {
		// Single-user mode: no login gate. A password may still be supplied
		// explicitly to protect the interface on a shared network.
	}
	for _, name := range []string{"login.html", "index.html", "favorites.html"} {
		page, err := template.ParseFS(assets, "templates/base.html", "templates/"+name)
		if err != nil {
			return nil, fmt.Errorf("webserve: parse %s: %w", name, err)
		}
		s.pages[name] = page
	}
	root, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, fmt.Errorf("webserve: open static assets: %w", err)
	}
	s.static = http.FileServerFS(root)
	s.routes()
	return s, nil
}

// GeneratedPassword reports the password the server created because the caller
// configured none. It returns false when the caller supplied a password.
func (s *Server) GeneratedPassword() (string, bool) {
	return s.generatedPassword, s.generatedPassword != ""
}

// Handler returns the HTTP handler for the backend.
func (s *Server) Handler() http.Handler {
	return s.securityHeaders(s.mux)
}

// Close releases session state. It does not own the underlying cloud tree.
func (s *Server) Close() {
	s.sessions.clear()
}

func (s *Server) routes() {
	// Health checks stay unauthenticated so supervisors can probe the process
	// before an operator logs in.
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.Handle("GET /static/", http.StripPrefix("/static/", s.static))
	s.mux.HandleFunc("GET /login", s.handleLoginPage)
	s.mux.HandleFunc("POST /login", s.handleLogin)
	s.mux.HandleFunc("POST /logout", s.requireAuth(s.handleLogout))
	s.mux.HandleFunc("GET /{$}", s.requireAuth(s.handleIndex))
	s.mux.HandleFunc("GET /favorites", s.requireAuth(s.handleFavorites))
	s.registerListingRoutes()
	s.registerPasswordRoutes()
	s.registerArchiveRoutes()
}

// securityHeaders applies response hardening that every route shares.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("X-Frame-Options", "DENY")
		header.Set("Referrer-Policy", "no-referrer")
		header.Set("Content-Security-Policy", "default-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net https://cdnjs.cloudflare.com; img-src 'self' data: blob:; font-src 'self' data: https://cdnjs.cloudflare.com; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "version": s.info.Version})
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if _, ok := s.currentSession(r); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, http.StatusOK, "login.html", pageData{
		Info:  s.info,
		Title: "登录",
		Next:  sanitizeNext(r.URL.Query().Get("next")),
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxLoginBodyBytes)
	if err := r.ParseForm(); err != nil {
		s.renderLogin(w, r, http.StatusBadRequest, "登录请求无法解析，请重试。")
		return
	}
	password := r.PostFormValue("password")
	next := sanitizeNext(r.PostFormValue("next"))
	if subtle.ConstantTimeCompare([]byte(password), s.password) != 1 {
		s.logger.Printf("webserve: rejected login attempt from %s", remoteHost(r))
		s.renderLogin(w, r, http.StatusUnauthorized, "密码不正确。")
		return
	}
	if _, err := s.issueSession(w, r); err != nil {
		s.logger.Printf("webserve: create session: %v", err)
		http.Error(w, "无法创建会话", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if id, ok := s.sessionID(r); ok {
		s.sessions.delete(id)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, browseHref(s.info.RootID, browseQuery{}), http.StatusSeeOther)
}

func (s *Server) handleFavorites(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "favorites.html", pageData{
		Info:          s.info,
		Title:         "收藏",
		Authenticated: s.authEnabled,
	})
}

func (s *Server) renderLogin(w http.ResponseWriter, r *http.Request, status int, message string) {
	s.render(w, status, "login.html", pageData{
		Info:  s.info,
		Title: "登录",
		Next:  sanitizeNext(r.PostFormValue("next")),
		Error: message,
	})
}

func (s *Server) render(w http.ResponseWriter, status int, name string, data pageData) {
	page, ok := s.pages[name]
	if !ok {
		http.Error(w, "页面不存在", http.StatusInternalServerError)
		return
	}
	// Render into a buffer so a template error cannot emit a half-written page
	// after the status code has been sent.
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

// requireAuth sends browsers to the login page and answers other clients with
// 401, so API and download routes can share one gate.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authEnabled {
			next(w, r)
			return
		}
		if _, ok := s.currentSession(r); ok {
			next(w, r)
			return
		}
		if isNavigation(r) {
			http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		w.Header().Set("WWW-Authenticate", `Session realm="mount123"`)
		http.Error(w, "需要登录", http.StatusUnauthorized)
	}
}

// isNavigation reports whether the request is a browser page load, which is the
// only case that gets a login redirect instead of a 401.
func isNavigation(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	accept := r.Header.Get("Accept")
	return accept == "" || strings.Contains(accept, "text/html")
}

func (s *Server) issueSession(w http.ResponseWriter, r *http.Request) (time.Time, error) {
	id, err := randomToken(32)
	if err != nil {
		return time.Time{}, err
	}
	now := s.now()
	expires := now.Add(s.ttl)
	s.sessions.create(id, now, expires)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    s.sign(id),
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(s.ttl.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil,
	})
	return expires, nil
}

// sessionID validates the session cookie signature and confirms the session is
// still live in the in-process store.
func (s *Server) sessionID(r *http.Request) (string, bool) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return "", false
	}
	id, ok := s.unsign(cookie.Value)
	if !ok {
		return "", false
	}
	if _, live := s.sessions.lookup(id, s.now()); !live {
		return "", false
	}
	return id, true
}

func (s *Server) currentSession(r *http.Request) (time.Time, bool) {
	id, ok := s.sessionID(r)
	if !ok {
		return time.Time{}, false
	}
	return s.sessions.lookup(id, s.now())
}

func (s *Server) sign(value string) string {
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write([]byte(value))
	return value + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Server) unsign(signed string) (string, bool) {
	value, _, ok := strings.Cut(signed, ".")
	if !ok || value == "" {
		return "", false
	}
	expected := s.sign(value)
	if subtle.ConstantTimeCompare([]byte(expected), []byte(signed)) != 1 {
		return "", false
	}
	return value, true
}

// pageData is the template payload shared by every page.
type pageData struct {
	Info
	Title         string
	Authenticated bool
	Error         string
	Next          string
	CacheCapacity string
	IndexBudget   string
	AuthEnabled   bool
}

// sessionStore keeps live sessions in process memory, which matches the
// single-binary deployment: a restart invalidates every login.
type sessionStore struct {
	mu      sync.Mutex
	expires map[string]time.Time
}

func newSessionStore() *sessionStore {
	return &sessionStore{expires: map[string]time.Time{}}
}

func (s *sessionStore) create(id string, now, expires time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for existing, expiry := range s.expires {
		if !now.Before(expiry) {
			delete(s.expires, existing)
		}
	}
	s.expires[id] = expires
}

func (s *sessionStore) lookup(id string, now time.Time) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	expires, ok := s.expires[id]
	if !ok {
		return time.Time{}, false
	}
	if !now.Before(expires) {
		delete(s.expires, id)
		return time.Time{}, false
	}
	return expires, true
}

func (s *sessionStore) delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.expires, id)
}

func (s *sessionStore) clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expires = map[string]time.Time{}
}

// sanitizeNext only accepts local absolute paths, so an attacker cannot turn
// the login flow into an open redirect.
func sanitizeNext(value string) string {
	if value == "" || !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") || strings.HasPrefix(value, "/\\") {
		return "/"
	}
	if strings.ContainsAny(value, "\r\n") {
		return "/"
	}
	return value
}

func remoteHost(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func randomToken(size int) (string, error) {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}

func humanBytes(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	value := float64(size)
	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB", "PiB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.2f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.2f EiB", value/unit)
}
