//go:build linux

// mount123 mounts a 123 cloud directory read-only using Linux FUSE.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/binghezhouke/123/mount123/internal/mountfs"
	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) > 1 && os.Args[1] == "unlock" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runUnlock(ctx, os.Args[2:], os.Stdin, os.Stderr, os.Stdout)
	}
	userCache, err := os.UserCacheDir()
	if err != nil {
		return err
	}
	configPath := flag.String("config", "config.json", "existing application config JSON (CLIENT_ID / CLIENT_SECRET)")
	mountpoint := flag.String("mountpoint", "", "empty local directory to mount")
	cacheDir := flag.String("cache-dir", filepath.Join(userCache, "mount123"), "private disk cache directory")
	cacheGiB := flag.Int64("cache-gib", 20, "maximum disk cache size in GiB")
	rootID := flag.Int64("root-id", 0, "123 cloud root directory ID")
	metadataMiB := flag.Int64("metadata-mib", 64, "shared directory and ZIP index cache budget in MiB")
	archiveEntries := flag.Int("archive-max-entries", 100000, "maximum members per archive index")
	directoryTTL := flag.Duration("directory-ttl", 30*time.Second, "directory snapshot freshness interval")
	sourceTTL := flag.Duration("source-ttl", 30*time.Second, "remote reader reuse interval across opens")
	fileInfo := flag.Bool("file-info", false, "refresh metadata for looked-up cloud files (adds a batched API request)")
	zipDirs := flag.Bool("zip-dirs", true, "expose ZIP, 7z and RAR archives as directories")
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
	if *mountpoint == "" {
		return fmt.Errorf("-mountpoint is required (see -help)")
	}
	if *cacheGiB < 1 || *cacheGiB > 1<<20 || *rootID < 0 || *metadataMiB < 1 || *metadataMiB > 1<<20 || *directoryTTL <= 0 || *sourceTTL <= 0 {
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
	cache, err := storage.NewCache(cacheAbs, *cacheGiB<<30)
	if err != nil {
		return err
	}
	defer cache.Close()
	api, err := panapi.New(panapi.Config{ClientID: config.ClientID, ClientSecret: config.ClientSecret, AccessToken: token, TokenCache: filepath.Join(cacheAbs, "token.json")})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Fail authentication/list errors before installing a mount.
	root := mountfs.NewWithOptions(ctx, api, cache, *rootID, *zipDirs, mountfs.Options{MaxZIPEntries: *archiveEntries, MaxExpandedNodes: 2 * *archiveEntries, PrefetchFiles: *prefetchFiles, PrefetchWorkers: *prefetchWorkers, PrefetchBytes: *prefetchMiB << 20, MetadataBytes: *metadataMiB << 20, DirectoryTTL: *directoryTTL, SourceTTL: *sourceTTL, RefreshFileMetadata: *fileInfo})
	if err = root.Prepare(ctx); err != nil {
		return fmt.Errorf("cloud root: %w", err)
	}
	timeout := time.Second
	server, err := fs.Mount(mountAbs, root, &fs.Options{MountOptions: fuse.MountOptions{Options: []string{"ro", "nodev", "nosuid", "noexec"}, FsName: "123pan", Name: "mount123", DisableXAttrs: true}, EntryTimeout: &timeout, AttrTimeout: &timeout})
	if err != nil {
		return fmt.Errorf("FUSE mount: %w", err)
	}
	log.Printf("read-only mount ready: %s (cache: %s)", mountAbs, cacheAbs)
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
