package webserve

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const testPassword = "correct horse battery staple"

var testKey = bytes.Repeat([]byte("k"), minSessionKeyBytes)

func newTestServer(t *testing.T, mutate func(*Options)) *Server {
	t.Helper()
	opts := Options{
		Password:   testPassword,
		SessionKey: testKey,
		Info:       Info{Version: "test", RootID: 7, CacheDir: "/tmp/cache"},
	}
	if mutate != nil {
		mutate(&opts)
	}
	server, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(server.Close)
	return server
}

func get(t *testing.T, handler http.Handler, path string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	for key, value := range header {
		request.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func login(t *testing.T, handler http.Handler, password string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"password": {password}}
	request := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func sessionCookie(t *testing.T, recorder *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == sessionCookieName && cookie.Value != "" {
			return cookie
		}
	}
	t.Fatalf("response did not set %s: %v", sessionCookieName, recorder.Result().Cookies())
	return nil
}

func TestHealthzNeedsNoSession(t *testing.T) {
	server := newTestServer(t, nil)
	recorder := get(t, server.Handler(), "/healthz", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", recorder.Code)
	}
	var payload map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("healthz body is not JSON: %v", err)
	}
	if payload["status"] != "ok" || payload["version"] != "test" {
		t.Fatalf("unexpected healthz payload: %v", payload)
	}
}

func TestProtectedPageRedirectsAnonymousBrowser(t *testing.T) {
	server := newTestServer(t, nil)
	recorder := get(t, server.Handler(), "/", map[string]string{"Accept": "text/html"})
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", recorder.Code)
	}
	if location := recorder.Header().Get("Location"); location != "/login?next=%2F" {
		t.Fatalf("Location = %q, want /login?next=%%2F", location)
	}
}

func TestProtectedRouteReturnsUnauthorizedForNonBrowserClients(t *testing.T) {
	server := newTestServer(t, nil)
	recorder := get(t, server.Handler(), "/", map[string]string{"Accept": "application/json"})
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", recorder.Code)
	}
	if recorder.Header().Get("WWW-Authenticate") == "" {
		t.Fatal("401 response is missing WWW-Authenticate")
	}
}

func TestLoginRejectsWrongPassword(t *testing.T) {
	server := newTestServer(t, nil)
	recorder := login(t, server.Handler(), "not the password")
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", recorder.Code)
	}
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == sessionCookieName && cookie.Value != "" {
			t.Fatalf("rejected login issued a session cookie: %v", cookie)
		}
	}
	if !strings.Contains(recorder.Body.String(), "密码不正确") {
		t.Fatalf("login page does not explain the failure: %s", recorder.Body.String())
	}
}

func TestLoginGrantsSessionAndRendersIndex(t *testing.T) {
	server := newTestServer(t, nil)
	handler := server.Handler()
	recorder := login(t, handler, testPassword)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d, want 303", recorder.Code)
	}
	if location := recorder.Header().Get("Location"); location != "/" {
		t.Fatalf("login redirected to %q, want /", location)
	}
	cookie := sessionCookie(t, recorder)
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie is not hardened: %+v", cookie)
	}

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.AddCookie(cookie)
	page := httptest.NewRecorder()
	handler.ServeHTTP(page, request)
	if page.Code != http.StatusOK {
		t.Fatalf("index status = %d, want 200", page.Code)
	}
	body := page.Body.String()
	for _, want := range []string{"网盘浏览", "/tmp/cache", "CloudArchive"} {
		if !strings.Contains(body, want) {
			t.Fatalf("index page is missing %q: %s", want, body)
		}
	}
}

func TestLoginRedirectsToSanitizedNext(t *testing.T) {
	cases := []struct {
		next string
		want string
	}{
		{next: "", want: "/"},
		{next: "/files?page=2", want: "/files?page=2"},
		{next: "https://evil.example/", want: "/"},
		{next: "//evil.example/", want: "/"},
		{next: "/\\evil.example", want: "/"},
		{next: "/files\r\nLocation: https://evil.example/", want: "/"},
	}
	for _, testCase := range cases {
		server := newTestServer(t, nil)
		form := url.Values{"password": {testPassword}, "next": {testCase.next}}
		request := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		if got := recorder.Header().Get("Location"); got != testCase.want {
			t.Fatalf("next=%q redirected to %q, want %q", testCase.next, got, testCase.want)
		}
	}
}

func TestLoginPageKeepsRequestedDestination(t *testing.T) {
	server := newTestServer(t, nil)
	recorder := get(t, server.Handler(), "/login?next=%2Ffiles%3Fpage%3D2", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), `value="/files?page=2"`) {
		t.Fatalf("login form lost the destination: %s", recorder.Body.String())
	}
}

func TestLogoutRevokesSession(t *testing.T) {
	server := newTestServer(t, nil)
	handler := server.Handler()
	cookie := sessionCookie(t, login(t, handler, testPassword))

	request := httptest.NewRequest(http.MethodPost, "/logout", nil)
	request.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("logout status = %d, want 303", recorder.Code)
	}
	if location := recorder.Header().Get("Location"); location != "/login" {
		t.Fatalf("logout redirected to %q, want /login", location)
	}
	cleared := false
	for _, returned := range recorder.Result().Cookies() {
		if returned.Name == sessionCookieName && returned.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("logout did not clear the session cookie")
	}

	after := httptest.NewRequest(http.MethodGet, "/", nil)
	after.AddCookie(cookie)
	after.Header.Set("Accept", "text/html")
	page := httptest.NewRecorder()
	handler.ServeHTTP(page, after)
	if page.Code != http.StatusSeeOther {
		t.Fatalf("a revoked session still reached the index: %d", page.Code)
	}
}

func TestSessionExpires(t *testing.T) {
	now := time.Now()
	server := newTestServer(t, func(opts *Options) {
		opts.SessionTTL = time.Hour
		opts.Now = func() time.Time { return now }
	})
	handler := server.Handler()
	cookie := sessionCookie(t, login(t, handler, testPassword))
	now = now.Add(2 * time.Hour)

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.AddCookie(cookie)
	request.Header.Set("Accept", "text/html")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 after expiry", recorder.Code)
	}
}

func TestForgedSessionCookieIsRejected(t *testing.T) {
	server := newTestServer(t, nil)
	handler := server.Handler()
	cookie := sessionCookie(t, login(t, handler, testPassword))
	forged := &http.Cookie{Name: sessionCookieName, Value: strings.TrimSuffix(cookie.Value, ".") + "x"}

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.AddCookie(forged)
	request.Header.Set("Accept", "text/html")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("forged cookie status = %d, want 303", recorder.Code)
	}
}

func TestEmptyPasswordDisablesAuth(t *testing.T) {
	server := newTestServer(t, func(opts *Options) { opts.Password = "" })
	generated, ok := server.GeneratedPassword()
	if ok || generated != "" {
		t.Fatalf("no-password server reported generated password = %q (ok=%v)", generated, ok)
	}
	// Without a configured password every route is open, including the index.
	if recorder := get(t, server.Handler(), "/", map[string]string{"Accept": "text/html"}); recorder.Code != http.StatusOK {
		t.Fatalf("unauthenticated root status = %d, want 200", recorder.Code)
	}
}

func TestConfiguredPasswordIsNotReportedAsGenerated(t *testing.T) {
	server := newTestServer(t, nil)
	if generated, ok := server.GeneratedPassword(); ok {
		t.Fatalf("generated password reported for a configured server: %q", generated)
	}
}

func TestNewRejectsWeakSessionKey(t *testing.T) {
	if _, err := New(Options{Password: "x", SessionKey: []byte("short")}); err == nil {
		t.Fatal("a short session key was accepted")
	}
}

func TestStaticAssetsAreServedWithoutASession(t *testing.T) {
	server := newTestServer(t, nil)
	recorder := get(t, server.Handler(), "/static/css/serve.css", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("static status = %d, want 200", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "topbar") {
		t.Fatal("static stylesheet body is unexpected")
	}
}

func TestSecurityHeadersAreAlwaysSet(t *testing.T) {
	server := newTestServer(t, nil)
	recorder := get(t, server.Handler(), "/healthz", nil)
	for _, header := range []string{"Content-Security-Policy", "X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy"} {
		if recorder.Header().Get(header) == "" {
			t.Fatalf("missing %s", header)
		}
	}
}

func TestLoginRejectsOversizedBody(t *testing.T) {
	server := newTestServer(t, nil)
	body := "password=" + strings.Repeat("a", maxLoginBodyBytes*2)
	request := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
}
