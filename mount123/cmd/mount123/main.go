//go:build linux

// mount123 mounts a 123 cloud directory read-only using Linux FUSE.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/binghezhouke/123/mount123/internal/mountfs"
	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

var (
	version   = "dev"
	commit    = "unknown"
	buildTime = "unknown"
)

func openCache(dir string, maxBytes int64, durability string) (*storage.Cache, error) {
	return openCacheWithDownloadConfig(dir, maxBytes, durability, storage.DefaultDownloadConfig())
}

func openCacheWithDownloadConfig(dir string, maxBytes int64, durability string, download storage.DownloadConfig) (*storage.Cache, error) {
	switch durability {
	case "durable":
		return storage.NewCacheWithDownloadConfig(dir, maxBytes, download)
	case "ephemeral":
		return storage.NewEphemeralCacheWithDownloadConfig(dir, maxBytes, download)
	default:
		return nil, fmt.Errorf("invalid cache durability: choose durable or ephemeral")
	}
}

// openCacheWithIndexBudget opens a cache with an explicit protected archive
// index budget. A non-positive budget keeps the cache's own default.
func openCacheWithIndexBudget(dir string, maxBytes, indexBudget int64, durability string, download storage.DownloadConfig) (*storage.Cache, error) {
	return openCacheWithIndexBudgetAndMinFree(dir, maxBytes, indexBudget, 0, durability, download)
}

func openCacheWithIndexBudgetAndMinFree(dir string, maxBytes, indexBudget, minFreeBytes int64, durability string, download storage.DownloadConfig) (*storage.Cache, error) {
	if indexBudget <= 0 {
		indexBudget = maxBytes / 8
	}
	if indexBudget > maxBytes {
		return nil, fmt.Errorf("archive index budget must not exceed the cache size")
	}
	switch durability {
	case "durable":
		return storage.NewCacheWithIndexBudgetAndMinFree(dir, maxBytes, indexBudget, minFreeBytes, download)
	case "ephemeral":
		return storage.NewEphemeralCacheWithIndexBudgetAndMinFree(dir, maxBytes, indexBudget, minFreeBytes, download)
	default:
		return nil, fmt.Errorf("invalid cache durability: choose durable or ephemeral")
	}
}

func main() {
	if err := run(); err != nil {
		log.Print(err)
		var exitErr *commandExitError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.code)
		}
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) == 1 || (len(os.Args) > 1 && os.Args[1] == "help") {
		printUsage(os.Stdout)
		return nil
	}
	if len(os.Args) > 1 && os.Args[1] == "unlock" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runUnlock(ctx, os.Args[2:], os.Stdin, os.Stderr, os.Stdout)
	}
	if len(os.Args) > 1 && (os.Args[1] == "status" || os.Args[1] == "wait-index" || os.Args[1] == "io-stats" || os.Args[1] == "refresh" || os.Args[1] == "doctor") {
		ctx, stop := notifyContext()
		defer stop()
		return runControlCommand(ctx, os.Args[1:], os.Stderr, os.Stdout)
	}
	userCache, err := os.UserCacheDir()
	if err != nil {
		return err
	}
	flag.Usage = func() { printUsage(os.Stderr) }
	configPath := flag.String("config", "config.json", "existing application config JSON (CLIENT_ID / CLIENT_SECRET)")
	mountpoint := flag.String("mountpoint", "", "empty local directory to mount")
	cacheDir := flag.String("cache-dir", filepath.Join(userCache, "mount123"), "private disk cache directory")
	controlSocket := flag.String("control-socket", "", "local status socket (default: <cache-dir>/control.sock)")
	cacheGiB := flag.Int64("cache-gib", 50, "maximum disk cache size in GiB")
	cacheMinFreeGiB := flag.Int64("cache-min-free-gib", 0, "minimum free space to keep on the cache filesystem in GiB (0 disables)")
	indexBudgetMiB := flag.Int64("index-budget-mib", 0, "protected archive index cache budget in MiB (0 derives it from the cache size)")
	statsInterval := flag.Duration("stats-interval", 30*time.Second, "append aggregate I/O statistics to io-stats.jsonl (0 disables)")
	cacheDurability := flag.String("cache-durability", "durable", "cache durability: durable or ephemeral")
	downloadRequests := flag.Int("download-requests", 32, "maximum simultaneous HTTP range responses")
	downloadBytesMiB := flag.Int64("download-bytes-mib", 128, "global in-flight HTTP response byte budget in MiB")
	downloadReserveMiB := flag.Int64("download-foreground-reserve-mib", 16, "in-flight byte capacity reserved for foreground reads in MiB")
	readAheadMiB := flag.Int64("read-ahead-mib", 16, "maximum sequential per-file read-ahead window in MiB (0 disables)")
	rootID := flag.Int64("root-id", 0, "123 cloud root directory ID")
	metadataMiB := flag.Int64("metadata-mib", 64, "shared directory and archive index cache budget in MiB")
	archiveEntries := flag.Int("archive-max-entries", 100000, "maximum members per archive index")
	directoryTTL := flag.Duration("directory-ttl", 24*time.Hour, "directory snapshot freshness interval")
	sourceTTL := flag.Duration("source-ttl", 144*time.Hour, "remote reader reuse interval across opens")
	entryTTL := flag.Duration("entry-ttl", time.Hour, "kernel positive and negative directory entry cache TTL (0 disables)")
	attrTTL := flag.Duration("attr-ttl", time.Hour, "kernel file attribute cache TTL (0 disables)")
	fileInfo := flag.Bool("file-info", false, "refresh metadata for looked-up cloud files (adds a batched API request)")
	archivePageCache := flag.Bool("archive-page-cache", true, "allow the kernel to cache fully materialized archive members")
	streamMembers := flag.Bool("stream-members", true, "stream large compressed archive members while they are being verified")
	zipDirs := flag.Bool("zip-dirs", true, "expose ZIP, 7z and RAR archives as directories")
	isoDirs := flag.Bool("iso-dirs", true, "expose ISO9660 and UDF optical images as read-only directories")
	prefetchFiles := flag.Int("prefetch-files", 9, "maximum adjacent images to prefetch (0 disables)")
	prefetchWorkers := flag.Int("prefetch-workers", 2, "maximum background image reads")
	prefetchMiB := flag.Int64("prefetch-mib", 256, "maximum target image bytes per prefetch window in MiB")
	flag.Parse()
	if *archiveEntries < 1 || *archiveEntries > 1000000 {
		return fmt.Errorf("invalid archive entry limit")
	}
	if *prefetchFiles < 0 || *prefetchFiles > 64 || *prefetchWorkers < 1 || *prefetchWorkers > 16 || *prefetchMiB < 1 || *prefetchMiB > 1<<20 {
		return fmt.Errorf("invalid prefetch limits")
	}
	if *cacheDurability != "durable" && *cacheDurability != "ephemeral" {
		return fmt.Errorf("invalid cache durability: choose durable or ephemeral")
	}
	if *readAheadMiB < 0 || *readAheadMiB > 16 {
		return fmt.Errorf("read-ahead window must be between 0 and 16 MiB")
	}
	if *downloadRequests < 1 || *downloadRequests > 256 || *downloadBytesMiB < 1 || *downloadBytesMiB > 1<<20 || *downloadReserveMiB < 0 || *downloadReserveMiB >= *downloadBytesMiB {
		return fmt.Errorf("invalid download request or in-flight byte limits")
	}
	if *mountpoint == "" {
		return fmt.Errorf("-mountpoint is required (see -help)")
	}
	if *cacheGiB < 1 || *cacheGiB > 1<<20 || *cacheMinFreeGiB < 0 || *cacheMinFreeGiB > 1<<20 || *indexBudgetMiB < 0 || *indexBudgetMiB > 1<<20 || *rootID < 0 || *metadataMiB < 1 || *metadataMiB > 1<<20 || *directoryTTL <= 0 || *sourceTTL <= 0 || *entryTTL < 0 || *attrTTL < 0 || *statsInterval < 0 {
		return fmt.Errorf("invalid cache size, freshness interval or root ID")
	}
	mountAbs, err := filepath.Abs(*mountpoint)
	if err != nil {
		return err
	}
	// Resolve existing symlinks before comparing to prevent a cache inside the mount.
	mountAbs, err = filepath.EvalSymlinks(mountAbs)
	if err != nil {
		return fmt.Errorf("mountpoint must exist: %w", err)
	}
	entries, err := os.ReadDir(mountAbs)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("mountpoint must be empty")
	}
	if err = os.MkdirAll(*cacheDir, 0700); err != nil {
		return err
	}
	if err = os.Chmod(*cacheDir, 0700); err != nil {
		return err
	}
	cacheAbs, err := filepath.EvalSymlinks(*cacheDir)
	if err != nil {
		return err
	}
	cacheAbs, err = filepath.Abs(cacheAbs)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(mountAbs, cacheAbs)
	if err != nil {
		return err
	}
	if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		return fmt.Errorf("cache directory must be outside mountpoint")
	}
	socketPath := *controlSocket
	if socketPath == "" {
		socketPath = controlSocketPath(cacheAbs)
	}
	socketPath, err = filepath.Abs(socketPath)
	if err != nil {
		return err
	}
	if isWithin(socketPath, mountAbs) {
		return fmt.Errorf("control socket must be outside mountpoint")
	}
	var config struct {
		ClientID     string `json:"CLIENT_ID"`
		ClientSecret string `json:"CLIENT_SECRET"`
	}
	data, err := os.ReadFile(*configPath)
	token := os.Getenv("PAN123_ACCESS_TOKEN")
	if err != nil && token == "" {
		return fmt.Errorf("read config: %w", err)
	}
	if err == nil {
		if err = json.Unmarshal(data, &config); err != nil {
			return fmt.Errorf("invalid config JSON: %w", err)
		}
	}
	downloadConfig := storage.DownloadConfig{MaxRequests: *downloadRequests, MaxInFlightBytes: *downloadBytesMiB << 20, ForegroundReservedBytes: *downloadReserveMiB << 20}
	cache, err := openCacheWithIndexBudgetAndMinFree(cacheAbs, *cacheGiB<<30, *indexBudgetMiB<<20, *cacheMinFreeGiB<<30, *cacheDurability, downloadConfig)
	if err != nil {
		return err
	}
	defer cache.Close()
	api, err := panapi.New(panapi.Config{ClientID: config.ClientID, ClientSecret: config.ClientSecret, AccessToken: token, TokenCache: filepath.Join(cacheAbs, "token.json")})
	if err != nil {
		return err
	}
	indexBudget := *indexBudgetMiB
	if indexBudget <= 0 {
		indexBudget = (*cacheGiB << 30) / 8 >> 20
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Fail authentication/list errors before installing a mount.
	executable, _ := os.Executable()
	mountInfo := fmt.Sprintf("version=%s\ncommit=%s\nbuild_time=%s\ngo=%s\nos_arch=%s/%s\nexecutable=%s\nmountpoint=%s\ncache_dir=%s\ncache_capacity_bytes=%d\nindex_budget_mib=%d\ncache_durability=%s\ncontrol_socket=%s\nroot_id=%d\nzip_dirs=%t\niso_dirs=%t\ndirectory_ttl=%s\nsource_ttl=%s\nentry_ttl=%s\nattr_ttl=%s\nmetadata_mib=%d\nprefetch_files=%d\nprefetch_workers=%d\nprefetch_mib=%d\ndownload_requests=%d\ndownload_bytes_mib=%d\nread_ahead_mib=%d\n", version, commit, buildTime, runtime.Version(), runtime.GOOS, runtime.GOARCH, executable, mountAbs, cacheAbs, *cacheGiB<<30, indexBudget, *cacheDurability, socketPath, *rootID, *zipDirs, *isoDirs, directoryTTL.String(), sourceTTL.String(), entryTTL.String(), attrTTL.String(), *metadataMiB, *prefetchFiles, *prefetchWorkers, *prefetchMiB, *downloadRequests, *downloadBytesMiB, *readAheadMiB)
	root := mountfs.NewWithOptions(ctx, api, cache, *rootID, *zipDirs, mountfs.Options{MountInfo: mountInfo, DisableISODirs: !*isoDirs, MaxZIPEntries: *archiveEntries, MaxExpandedNodes: 2 * *archiveEntries, PrefetchFiles: *prefetchFiles, PrefetchWorkers: *prefetchWorkers, PrefetchBytes: *prefetchMiB << 20, MetadataBytes: *metadataMiB << 20, DirectoryTTL: *directoryTTL, SourceTTL: *sourceTTL, RefreshFileMetadata: *fileInfo, DisableStreamMembers: !*streamMembers, DisableArchivePageCache: !*archivePageCache, ReadAheadMaxBytes: *readAheadMiB << 20, DisableReadAhead: *readAheadMiB == 0})
	if err = root.Prepare(ctx); err != nil {
		return fmt.Errorf("cloud root: %w", err)
	}
	server, err := fs.Mount(mountAbs, root, &fs.Options{MountOptions: fuse.MountOptions{Options: []string{"ro", "nodev", "nosuid", "noexec"}, FsName: "123pan", Name: "mount123", DisableXAttrs: true}, EntryTimeout: entryTTL, NegativeTimeout: entryTTL, AttrTimeout: attrTTL})
	if err != nil {
		return fmt.Errorf("FUSE mount: %w", err)
	}
	control, err := startControlServer(socketPath, root, mountAbs)
	if err != nil {
		_ = server.Unmount()
		return fmt.Errorf("start local control socket: %w", err)
	}
	defer control.close()
	go control.serve(ctx)
	statsSampler := startStatsSampler(ctx, filepath.Join(cacheAbs, statsFileName), *statsInterval, root.IOStats, log.Printf)
	defer statsSampler.Close()
	log.Printf("read-only mount ready: %s (cache: %s, control: %s)", mountAbs, cacheAbs, socketPath)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	done := make(chan struct{})
	go func() { server.Wait(); close(done) }()
	for {
		select {
		case <-signals:
			cancel()
			if err := server.Unmount(); err != nil {
				log.Printf("unmount failed: %v; close open files and retry Ctrl+C or fusermount3 -u", err)
				continue
			}
			<-done
			return nil
		case <-done:
			return nil
		}
	}
}

func printUsage(w io.Writer) {
	_, _ = fmt.Fprintln(w, "Usage: mount123 [mount flags]")
	_, _ = fmt.Fprintln(w, "       mount123 <command> [flags]")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "Commands:")
	_, _ = fmt.Fprintln(w, "  status       query an archive index")
	_, _ = fmt.Fprintln(w, "  wait-index   wait for an archive index to finish")
	_, _ = fmt.Fprintln(w, "  refresh      refresh one cloud directory")
	_, _ = fmt.Fprintln(w, "  doctor       diagnose a mounted path")
	_, _ = fmt.Fprintln(w, "  io-stats     print I/O counters")
	_, _ = fmt.Fprintln(w, "  unlock       set a ZIP/7z/RAR password from a mounted path")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "Read-only directory controls (hidden from ls/find):")
	_, _ = fmt.Fprintln(w, "  .mount123-refresh       read to refresh the current cloud directory")
	_, _ = fmt.Fprintln(w, "  .mount123-probe         read to detect renamed archives in one level")
	_, _ = fmt.Fprintln(w, "  .mount123-probe-status  read to query the last probe without starting one")
	_, _ = fmt.Fprintln(w, "  .mount123-info          read mount version, cache and runtime configuration")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "Use 'mount123 <command> -h' for command flags. See mount123/README.md for limits and examples.")
	_, _ = fmt.Fprintln(w)
	flag.PrintDefaults()
}
