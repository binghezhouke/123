//go:build linux

package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/binghezhouke/123/mount123/internal/mountfs"
	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/binghezhouke/123/mount123/internal/webserve"
)

// serveDefaultAddr matches the port the retired Flask front end used, so
// bookmarks and deployment notes keep working after the migration.
const serveDefaultAddr = "127.0.0.1:8081"

const serveShutdownTimeout = 10 * time.Second

// serveSettings mirrors the storage- and tree-relevant mount flags so a
// `serve` process pointed at the mount's cache directory behaves the same way.
type serveSettings struct {
	configPath         string
	addr               string
	password           string
	sessionTTL         time.Duration
	cacheDir           string
	cacheGiB           int64
	cacheMinFreeGiB    int64
	indexBudgetMiB     int64
	cacheDurability    string
	rootID             int64
	zipDirs            bool
	isoDirs            bool
	archiveEntries     int
	metadataMiB        int64
	directoryTTL       time.Duration
	sourceTTL          time.Duration
	fileInfo           bool
	streamMembers      bool
	archivePageCache   bool
	downloadRequests   int
	downloadBytesMiB   int64
	downloadReserveMiB int64
}

// serveDeps isolates the process-wide dependencies so tests can run the
// command against a fake cloud and observe the bound address.
type serveDeps struct {
	newAPI   func(panapi.Config) (mountfs.API, error)
	newCache func(dir string, maxBytes, indexBudget, minFreeBytes int64, durability string, download storage.DownloadConfig) (*storage.Cache, error)
	onListen func(addr string)
}

func runServe(ctx context.Context, args []string, stderr, stdout io.Writer) error {
	return runServeWith(ctx, args, stderr, stdout, serveDeps{
		newAPI: func(cfg panapi.Config) (mountfs.API, error) { return panapi.New(cfg) },
		newCache: func(dir string, maxBytes, indexBudget, minFreeBytes int64, durability string, download storage.DownloadConfig) (*storage.Cache, error) {
			return openCacheWithIndexBudgetAndMinFreeShared(dir, maxBytes, indexBudget, minFreeBytes, durability, download)
		},
	})
}

func runServeWith(ctx context.Context, args []string, stderr, stdout io.Writer, deps serveDeps) error {
	settings, err := parseServeSettings(args, stderr, stdout)
	if err != nil {
		return err
	}
	if settings == nil {
		return nil
	}
	if err = validateServeSettings(*settings); err != nil {
		return err
	}
	config, err := readServeConfig(settings.configPath)
	if err != nil {
		return err
	}
	password := resolveServePassword(*settings, config)
	cacheAbs, err := prepareServeCacheDir(settings.cacheDir)
	if err != nil {
		return err
	}
	download := storage.DownloadConfig{
		MaxRequests:             settings.downloadRequests,
		MaxInFlightBytes:        settings.downloadBytesMiB << 20,
		ForegroundReservedBytes: settings.downloadReserveMiB << 20,
	}
	cache, err := deps.newCache(cacheAbs, settings.cacheGiB<<30, settings.indexBudgetMiB<<20, settings.cacheMinFreeGiB<<30, settings.cacheDurability, download)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := cache.Close(); closeErr != nil {
			log.Printf("serve: close cache: %v", closeErr)
		}
	}()
	api, err := deps.newAPI(panapi.Config{
		ClientID:     config.ClientID,
		ClientSecret: config.ClientSecret,
		AccessToken:  config.AccessToken,
		TokenCache:   filepath.Join(cacheAbs, "token.json"),
	})
	if err != nil {
		return err
	}
	indexBudget := settings.indexBudgetMiB
	if indexBudget <= 0 {
		indexBudget = (settings.cacheGiB << 30) / 8 >> 20
	}
	service, err := mountfs.NewService(ctx, api, cache, settings.rootID, settings.zipDirs, mountfs.Options{
		DisableISODirs:          !settings.isoDirs,
		MaxZIPEntries:           settings.archiveEntries,
		MaxExpandedNodes:        2 * settings.archiveEntries,
		MetadataBytes:           settings.metadataMiB << 20,
		DirectoryTTL:            settings.directoryTTL,
		SourceTTL:               settings.sourceTTL,
		RefreshFileMetadata:     settings.fileInfo,
		DisableStreamMembers:    !settings.streamMembers,
		DisableArchivePageCache: !settings.archivePageCache,
	})
	if err != nil {
		return fmt.Errorf("cloud root: %w", err)
	}
	// Bind before building the web layer so the page can report the address the
	// kernel actually assigned, which matters when -addr uses port 0.
	listener, err := net.Listen("tcp", settings.addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", settings.addr, err)
	}
	defer listener.Close()
	if deps.onListen != nil {
		deps.onListen(listener.Addr().String())
	}
	server, err := webserve.New(webserve.Options{
		Password:   password,
		SessionKey: serveSessionKey(config.SecretKey),
		SessionTTL: settings.sessionTTL,
		Service:    service,
		Info: webserve.Info{
			Version:            version,
			Commit:             commit,
			BuildTime:          buildTime,
			RootID:             settings.rootID,
			Address:            listener.Addr().String(),
			CacheDir:           cacheAbs,
			CacheCapacityBytes: settings.cacheGiB << 30,
			IndexBudgetBytes:   indexBudget << 20,
			CacheDurability:    settings.cacheDurability,
			DirectoryTTL:       settings.directoryTTL,
			SourceTTL:          settings.sourceTTL,
		},
	})
	if err != nil {
		return err
	}
	defer server.Close()
	if generated, ok := server.GeneratedPassword(); ok {
		log.Printf("serve: no password configured, generated access password: %s", generated)
	}
	httpServer := &http.Server{
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpServer.Serve(listener) }()
	log.Printf("serve: listening on http://%s (cache: %s)", listener.Addr(), cacheAbs)
	select {
	case <-ctx.Done():
	case err = <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), serveShutdownTimeout)
	defer cancel()
	if err = httpServer.Shutdown(shutdownCtx); err != nil {
		_ = httpServer.Close()
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

// parseServeSettings parses the serve flags. It returns nil settings when the
// caller asked for help, without treating that as an error.
func parseServeSettings(args []string, stderr, stdout io.Writer) (*serveSettings, error) {
	defaultCache := filepath.Join(os.TempDir(), "mount123")
	if userCache, err := os.UserCacheDir(); err == nil {
		defaultCache = filepath.Join(userCache, "mount123")
	}
	settings := &serveSettings{}
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "Usage: mount123 serve [flags]")
		_, _ = fmt.Fprintln(stderr)
		_, _ = fmt.Fprintln(stderr, "Starts the local web backend without mounting FUSE. It shares the")
		_, _ = fmt.Fprintln(stderr, "configured cache directory with the mount.")
		_, _ = fmt.Fprintln(stderr)
		fs.PrintDefaults()
	}
	fs.StringVar(&settings.configPath, "config", "config.json", "existing application config JSON (CLIENT_ID / CLIENT_SECRET, optional SECRET_KEY)")
	fs.StringVar(&settings.addr, "addr", serveDefaultAddr, "localhost listen address")
	fs.StringVar(&settings.password, "password", "", "login password (default: MOUNT123_SERVE_PASSWORD, config SERVE_PASSWORD, or a generated one)")
	fs.DurationVar(&settings.sessionTTL, "session-ttl", webserve.DefaultSessionTTL, "how long a web login stays valid")
	fs.StringVar(&settings.cacheDir, "cache-dir", defaultCache, "private disk cache directory (share it with the mount)")
	fs.Int64Var(&settings.cacheGiB, "cache-gib", 50, "maximum disk cache size in GiB")
	fs.Int64Var(&settings.cacheMinFreeGiB, "cache-min-free-gib", 0, "minimum free space to keep on the cache filesystem in GiB (0 disables)")
	fs.Int64Var(&settings.indexBudgetMiB, "index-budget-mib", 0, "protected archive index cache budget in MiB (0 derives it from the cache size)")
	fs.StringVar(&settings.cacheDurability, "cache-durability", "durable", "cache durability: durable or ephemeral")
	fs.Int64Var(&settings.rootID, "root-id", 0, "123 cloud root directory ID")
	fs.BoolVar(&settings.zipDirs, "zip-dirs", true, "expose ZIP, 7z and RAR archives as directories")
	fs.BoolVar(&settings.isoDirs, "iso-dirs", true, "expose ISO9660 and UDF optical images as directories")
	fs.IntVar(&settings.archiveEntries, "archive-max-entries", 100000, "maximum members per archive index")
	fs.Int64Var(&settings.metadataMiB, "metadata-mib", 64, "shared directory and archive index cache budget in MiB")
	fs.DurationVar(&settings.directoryTTL, "directory-ttl", 24*time.Hour, "directory snapshot freshness interval")
	fs.DurationVar(&settings.sourceTTL, "source-ttl", 144*time.Hour, "remote reader reuse interval across opens")
	fs.BoolVar(&settings.fileInfo, "file-info", false, "refresh metadata for looked-up cloud files (adds a batched API request)")
	fs.BoolVar(&settings.streamMembers, "stream-members", true, "stream large compressed archive members while they are being verified")
	fs.BoolVar(&settings.archivePageCache, "archive-page-cache", true, "allow fully materialized archive members to be cached")
	fs.IntVar(&settings.downloadRequests, "download-requests", 32, "maximum simultaneous HTTP range responses")
	fs.Int64Var(&settings.downloadBytesMiB, "download-bytes-mib", 128, "global in-flight HTTP response byte budget in MiB")
	fs.Int64Var(&settings.downloadReserveMiB, "download-foreground-reserve-mib", 16, "in-flight byte capacity reserved for foreground reads in MiB")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, nil
		}
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected serve arguments: %v", fs.Args())
	}
	return settings, nil
}

func validateServeSettings(settings serveSettings) error {
	if settings.addr == "" {
		return errors.New("serve: -addr is required")
	}
	if settings.sessionTTL < 0 {
		return errors.New("serve: -session-ttl must not be negative")
	}
	if settings.archiveEntries < 1 || settings.archiveEntries > 1000000 {
		return fmt.Errorf("invalid archive entry limit")
	}
	if settings.cacheDurability != "durable" && settings.cacheDurability != "ephemeral" {
		return fmt.Errorf("invalid cache durability: choose durable or ephemeral")
	}
	if settings.downloadRequests < 1 || settings.downloadRequests > 256 || settings.downloadBytesMiB < 1 || settings.downloadBytesMiB > 1<<20 || settings.downloadReserveMiB < 0 || settings.downloadReserveMiB >= settings.downloadBytesMiB {
		return fmt.Errorf("invalid download request or in-flight byte limits")
	}
	if settings.cacheGiB < 1 || settings.cacheGiB > 1<<20 || settings.cacheMinFreeGiB < 0 || settings.cacheMinFreeGiB > 1<<20 || settings.indexBudgetMiB < 0 || settings.indexBudgetMiB > 1<<20 || settings.rootID < 0 || settings.metadataMiB < 1 || settings.metadataMiB > 1<<20 || settings.directoryTTL <= 0 || settings.sourceTTL <= 0 || settings.cacheDir == "" {
		return fmt.Errorf("invalid cache size, freshness interval or root ID")
	}
	return nil
}

type serveConfig struct {
	ClientID      string `json:"CLIENT_ID"`
	ClientSecret  string `json:"CLIENT_SECRET"`
	SecretKey     string `json:"SECRET_KEY"`
	AccessToken   string
	ServePassword string `json:"SERVE_PASSWORD"`
	WebPassword   string `json:"WEB_PASSWORD"`
}

// readServeConfig reads the shared application config. A config file is
// optional when PAN123_ACCESS_TOKEN supplies the credential.
func readServeConfig(path string) (serveConfig, error) {
	var config serveConfig
	config.AccessToken = os.Getenv("PAN123_ACCESS_TOKEN")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) && config.AccessToken != "" {
			return config, nil
		}
		return config, fmt.Errorf("read config: %w", err)
	}
	if err = json.Unmarshal(data, &config); err != nil {
		return config, fmt.Errorf("invalid config JSON: %w", err)
	}
	return config, nil
}

// resolveServePassword applies the password precedence: explicit flag, then
// environment, then config. An empty result makes webserve generate one.
func resolveServePassword(settings serveSettings, config serveConfig) string {
	if settings.password != "" {
		return settings.password
	}
	if env := os.Getenv("MOUNT123_SERVE_PASSWORD"); env != "" {
		return env
	}
	if config.ServePassword != "" {
		return config.ServePassword
	}
	return config.WebPassword
}

// prepareServeCacheDir creates the shared cache directory with private
// permissions and resolves it to an absolute path.
func prepareServeCacheDir(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return "", err
	}
	abs, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	return filepath.Abs(abs)
}

// serveSessionKey derives a stable signing key from the configured secret so
// logins survive a restart, and falls back to a fresh random key otherwise.
func serveSessionKey(secret string) []byte {
	if secret != "" {
		sum := sha256.Sum256([]byte("mount123 serve session key\x00" + secret))
		return sum[:]
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		// crypto/rand does not fail in practice; a nil key would be rejected by
		// webserve.New, so surface the failure instead of serving weak cookies.
		panic(fmt.Sprintf("serve: session key: %v", err))
	}
	return key
}
