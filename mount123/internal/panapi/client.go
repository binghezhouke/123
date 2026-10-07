// Package panapi implements the read-only subset of the 123 Open Platform API
// used by mount123.
package panapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultBaseURL = "https://open-api.123pan.com"

// File is the subset of remote file metadata needed by the mount.
type File struct {
	ID      int64
	Name    string
	Size    int64
	IsDir   bool
	Version string
}

// Config contains Open Platform credentials and optional token settings.
type Config struct {
	ClientID     string
	ClientSecret string
	AccessToken  string
	BaseURL      string
	TokenCache   string
}

// Client performs read-only requests to the 123 Open Platform API.
type Client struct {
	config  Config
	baseURL string
	http    *http.Client

	mu       sync.Mutex
	token    string
	expires  time.Time
	nextCall time.Time
}

type tokenCache struct {
	ClientID string    `json:"clientID"`
	BaseURL  string    `json:"baseURL"`
	Token    string    `json:"accessToken"`
	Expires  time.Time `json:"expiresAt"`
}

type apiEnvelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// New constructs a client. Credentials may be omitted when AccessToken is set.
func New(cfg Config) (*Client, error) {
	if cfg.AccessToken == "" && (cfg.ClientID == "" || cfg.ClientSecret == "") {
		return nil, errors.New("panapi: client ID and secret are required when no access token is configured")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultBaseURL
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("panapi: invalid base URL")
	}
	c := &Client{config: cfg, baseURL: strings.TrimRight(cfg.BaseURL, "/"), http: &http.Client{Timeout: 20 * time.Second}}
	if cfg.AccessToken != "" {
		c.token = cfg.AccessToken
		c.expires = time.Now().Add(24 * time.Hour)
	} else {
		c.loadTokenCache()
	}
	return c, nil
}

type remoteFile struct {
	ID       json.RawMessage `json:"fileId"`
	Name     string          `json:"filename"`
	Size     json.RawMessage `json:"size"`
	Type     json.RawMessage `json:"type"`
	ETag     string          `json:"etag"`
	UpdateAt string          `json:"updateAt"`
	Trashed  json.RawMessage `json:"trashed"`
}

// List returns every non-trashed item in parentID. The API is paged in batches
// of 100 and requests are limited to three per second.
func (c *Client) List(ctx context.Context, parentID int64) ([]File, error) {
	var files []File
	var cursor int64
	seen := make(map[int64]struct{})
	for page := 0; page < 10000; page++ {
		q := url.Values{"limit": {"100"}, "parentFileId": {strconv.FormatInt(parentID, 10)}}
		if page > 0 {
			q.Set("lastFileId", strconv.FormatInt(cursor, 10))
		}
		var body struct {
			FileList []remoteFile    `json:"fileList"`
			LastID   json.RawMessage `json:"lastFileId"`
		}
		if err := c.getJSON(ctx, "/api/v2/file/list", q, &body); err != nil {
			return nil, err
		}
		for _, item := range body.FileList {
			if isTrashed(item.Trashed) {
				continue
			}
			id, err := nonNegativeInt(item.ID)
			if err != nil {
				return nil, errors.New("panapi: invalid file ID in response")
			}
			size, err := nonNegativeInt(item.Size)
			if err != nil {
				return nil, errors.New("panapi: invalid file size in response")
			}
			typ, err := nonNegativeInt(item.Type)
			if err != nil {
				return nil, errors.New("panapi: invalid file type in response")
			}
			files = append(files, File{ID: id, Name: item.Name, Size: size, IsDir: typ == 1, Version: item.ETag + ":" + strconv.FormatInt(size, 10) + ":" + item.UpdateAt})
		}
		if len(body.LastID) == 0 || string(body.LastID) == "null" {
			return files, nil
		}
		next, err := intValue(body.LastID)
		if err != nil {
			return nil, errors.New("panapi: invalid pagination cursor")
		}
		if next == -1 {
			return files, nil
		}
		if next < 0 {
			return nil, errors.New("panapi: invalid pagination cursor")
		}
		if _, ok := seen[next]; ok || next == cursor {
			return nil, errors.New("panapi: pagination cursor repeated")
		}
		seen[next] = struct{}{}
		cursor = next
	}
	return nil, errors.New("panapi: pagination exceeded safety limit")
}

// DownloadURL returns the short-lived URL supplied by download_info. Callers
// should not log this value because it may contain signed credentials.
func (c *Client) DownloadURL(ctx context.Context, id int64) (string, error) {
	var body struct {
		DownloadURL string `json:"downloadUrl"`
	}
	q := url.Values{"fileId": {strconv.FormatInt(id, 10)}}
	if err := c.getJSON(ctx, "/api/v1/file/download_info", q, &body); err != nil {
		return "", err
	}
	if body.DownloadURL == "" {
		return "", errors.New("panapi: download URL missing from response")
	}
	return body.DownloadURL, nil
}

func (c *Client) getJSON(ctx context.Context, endpoint string, query url.Values, out any) error {
	for attempt := 0; attempt < 2; attempt++ {
		token, err := c.accessToken(ctx)
		if err != nil {
			return err
		}
		if err := c.waitRateLimit(ctx); err != nil {
			return err
		}
		u := c.baseURL + endpoint
		if len(query) > 0 {
			u += "?" + query.Encode()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return errors.New("panapi: could not create request")
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Platform", "open_platform")
		resp, err := c.http.Do(req)
		if err != nil {
			return fmt.Errorf("panapi: request failed: %s", safeError(err.Error(), c.config))
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if readErr != nil {
			return errors.New("panapi: could not read API response")
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 && c.config.AccessToken == "" {
			c.invalidateToken()
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("panapi: API returned HTTP %d", resp.StatusCode)
		}
		var envelope apiEnvelope
		if err := json.Unmarshal(data, &envelope); err != nil {
			return errors.New("panapi: malformed API response")
		}
		if envelope.Code != 0 {
			msg := safeError(envelope.Message, c.config, token)
			if msg == "" {
				return fmt.Errorf("panapi: API error code %d", envelope.Code)
			}
			return fmt.Errorf("panapi: API error code %d: %s", envelope.Code, msg)
		}
		if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
			return errors.New("panapi: API response missing data")
		}
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return errors.New("panapi: malformed API data")
		}
		return nil
	}
	return errors.New("panapi: unauthorized")
}

func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.token != "" && time.Now().Before(c.expires.Add(-time.Minute)) {
		t := c.token
		c.mu.Unlock()
		return t, nil
	}
	if c.config.AccessToken != "" {
		t := c.token
		c.mu.Unlock()
		return t, nil
	}
	c.mu.Unlock()
	return c.fetchToken(ctx)
}

func (c *Client) fetchToken(ctx context.Context) (string, error) {
	if err := c.waitRateLimit(ctx); err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.expires.Add(-time.Minute)) {
		return c.token, nil
	}
	payload, _ := json.Marshal(map[string]string{"clientID": c.config.ClientID, "clientSecret": c.config.ClientSecret})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/access_token", strings.NewReader(string(payload)))
	if err != nil {
		return "", errors.New("panapi: could not create token request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Platform", "open_platform")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("panapi: token request failed: %s", safeError(err.Error(), c.config))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("panapi: token endpoint returned HTTP %d", resp.StatusCode)
	}
	var envelope struct {
		Code int `json:"code"`
		Data struct {
			Token     string `json:"accessToken"`
			ExpiresIn int64  `json:"expiresIn"`
			ExpiredAt string `json:"expiredAt"`
		} `json:"data"`
	}
	if json.Unmarshal(data, &envelope) != nil || envelope.Code != 0 || envelope.Data.Token == "" {
		return "", errors.New("panapi: token response invalid")
	}
	expires := time.Now().Add(time.Duration(envelope.Data.ExpiresIn) * time.Second)
	if envelope.Data.ExpiresIn <= 0 {
		expires = time.Now().Add(time.Hour)
	}
	if envelope.Data.ExpiredAt != "" {
		if parsed, e := time.Parse(time.RFC3339, envelope.Data.ExpiredAt); e == nil {
			expires = parsed
		}
	}
	c.token, c.expires = envelope.Data.Token, expires
	c.saveTokenCache(expires)
	return c.token, nil
}

func (c *Client) invalidateToken() {
	c.mu.Lock()
	c.token = ""
	c.expires = time.Time{}
	c.mu.Unlock()
}

func (c *Client) waitRateLimit(ctx context.Context) error {
	c.mu.Lock()
	now := time.Now()
	wait := c.nextCall.Sub(now)
	if wait < 0 {
		wait = 0
	}
	c.nextCall = now.Add(wait).Add(time.Second / 3)
	c.mu.Unlock()
	if wait == 0 {
		return nil
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *Client) cachePath() string {
	if c.config.TokenCache != "" {
		return c.config.TokenCache
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "mount123", "token.json")
}

func (c *Client) loadTokenCache() {
	path := c.cachePath()
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	_ = os.Chmod(path, 0600)
	var cached tokenCache
	if json.Unmarshal(data, &cached) != nil || cached.ClientID != c.config.ClientID || cached.BaseURL != c.baseURL || cached.Token == "" || time.Now().After(cached.Expires.Add(-time.Minute)) {
		return
	}
	c.token, c.expires = cached.Token, cached.Expires
}

func (c *Client) saveTokenCache(expires time.Time) {
	path := c.cachePath()
	if path == "" {
		return
	}
	dir := filepath.Dir(path)
	if os.MkdirAll(dir, 0700) != nil {
		return
	}
	_ = os.Chmod(dir, 0700)
	cache := tokenCache{ClientID: c.config.ClientID, BaseURL: c.baseURL, Token: c.token, Expires: expires}
	b, err := json.Marshal(cache)
	if err != nil {
		return
	}
	var suffix [8]byte
	if _, err = rand.Read(suffix[:]); err != nil {
		return
	}
	tmp := filepath.Join(dir, ".token-"+hex.EncodeToString(suffix[:]))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return
	}
	_, writeErr := f.Write(b)
	closeErr := f.Close()
	if writeErr == nil && closeErr == nil {
		_ = os.Chmod(tmp, 0600)
		_ = os.Rename(tmp, path)
	} else {
		_ = os.Remove(tmp)
	}
}

func intValue(raw json.RawMessage) (int64, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, nil
	}
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return n, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strconv.ParseInt(s, 10, 64)
	}
	return 0, errors.New("invalid integer")
}

func nonNegativeInt(raw json.RawMessage) (int64, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, errors.New("missing integer")
	}
	n, err := intValue(raw)
	if err != nil || n < 0 {
		return 0, errors.New("invalid non-negative integer")
	}
	return n, nil
}

func isTrashed(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" || string(raw) == "false" || string(raw) == "0" {
		return false
	}
	if string(raw) == "true" || string(raw) == "1" {
		return true
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s == "1" || strings.EqualFold(s, "true")
	}
	return false
}

var urlPattern = regexp.MustCompile(`https?://[^\s"']+`)

func safeError(message string, cfg Config, additionalSecrets ...string) string {
	secrets := []string{cfg.ClientID, cfg.ClientSecret, cfg.AccessToken}
	secrets = append(secrets, additionalSecrets...)
	for _, secret := range secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}
	message = urlPattern.ReplaceAllString(message, "[redacted URL]")
	if len(message) > 200 {
		message = message[:200]
	}
	return strings.TrimSpace(message)
}
