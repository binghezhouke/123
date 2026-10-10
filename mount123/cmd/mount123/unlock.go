//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/binghezhouke/123/mount123/internal/mountfs"
	"github.com/binghezhouke/123/mount123/internal/panapi"
	"github.com/binghezhouke/123/mount123/internal/storage"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

const maxUnlockPasswordBytes = 4096
const unlockCacheBytes = 256 << 20

type unlockAPI interface {
	Detail(context.Context, int64) (panapi.File, error)
	DownloadURL(context.Context, int64) (string, error)
	SaveZIPPassword(context.Context, panapi.File, []byte) (panapi.File, error)
}

type unlockDeps struct {
	newAPI   func(panapi.Config) (unlockAPI, error)
	validate func(context.Context, *storage.Remote, int64, []byte) error
}

func runUnlock(ctx context.Context, args []string, stdin io.Reader, stderr, stdout io.Writer) error {
	deps := unlockDeps{
		newAPI:   func(cfg panapi.Config) (unlockAPI, error) { return panapi.New(cfg) },
		validate: mountfs.ValidateZIPPassword,
	}
	return runUnlockWith(ctx, args, stdin, stderr, stdout, term.IsTerminal(int(os.Stdin.Fd())), func() ([]byte, error) {
		return readTerminalPassword(ctx, int(os.Stdin.Fd()))
	}, deps)
}

func readTerminalPassword(ctx context.Context, fd int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	state, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return nil, errors.New("could not save terminal state")
	}
	passwordState := *state
	passwordState.Lflag &^= unix.ECHO
	passwordState.Lflag |= unix.ICANON | unix.ISIG
	passwordState.Iflag |= unix.ICRNL
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &passwordState); err != nil {
		return nil, errors.New("could not hide terminal password input")
	}
	var restoreOnce sync.Once
	restore := func() error {
		var restoreErr error
		restoreOnce.Do(func() { restoreErr = unix.IoctlSetTermios(fd, unix.TCSETS, state) })
		return restoreErr
	}
	defer restore()
	return awaitTerminalPassword(ctx, func() ([]byte, error) {
		return readTerminalLine(fd)
	}, restore)
}

func readTerminalLine(fd int) ([]byte, error) {
	password := make([]byte, 0, 64)
	var one [1]byte
	for {
		n, err := unix.Read(fd, one[:])
		if err != nil {
			wipe(password)
			return nil, err
		}
		if n == 0 {
			wipe(password)
			return nil, errors.New("terminal closed")
		}
		switch one[0] {
		case '\n', '\r':
			return password, nil
		case '\b', 0x7f:
			if len(password) > 0 {
				password[len(password)-1] = 0
				password = password[:len(password)-1]
			}
		default:
			if len(password) >= maxUnlockPasswordBytes {
				wipe(password)
				return nil, errors.New("Archive password exceeds 4096 bytes")
			}
			password = append(password, one[0])
		}
	}
}

func awaitTerminalPassword(ctx context.Context, read func() ([]byte, error), restore func() error) ([]byte, error) {
	type result struct {
		password []byte
		err      error
	}
	finished := make(chan result, 1)
	go func() {
		password, err := read()
		finished <- result{password: password, err: err}
	}()
	select {
	case got := <-finished:
		return got.password, got.err
	case <-ctx.Done():
		// The caller disabled echo before starting the reader. Restore now so
		// Ctrl+C can exit even while the user has not pressed Enter.
		_ = restore()
		return nil, ctx.Err()
	}
}

func runUnlockWith(ctx context.Context, args []string, stdin io.Reader, stderr, stdout io.Writer, terminal bool, readTerminal func() ([]byte, error), deps unlockDeps) error {
	userCache, err := os.UserCacheDir()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("mount123 unlock", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		_, _ = io.WriteString(stderr, "Usage: mount123 unlock [flags] MOUNTED_ARCHIVE\n       mount123 unlock -all [flags] MOUNTED_DIRECTORY\n       mount123 unlock -shared [flags] MOUNTED_DIRECTORY\n       mount123 unlock -file-id ID [-config PATH] [-cache-dir PATH] [-password-stdin]\n")
		flags.PrintDefaults()
	}
	fileID := flags.Int64("file-id", 0, "cloud file ID of the encrypted ZIP")
	configPath := flags.String("config", "config.json", "existing application config JSON")
	cacheDir := flags.String("cache-dir", filepath.Join(userCache, "mount123"), "private cache root (token cache is reused)")
	passwordStdin := flags.Bool("password-stdin", false, "read the archive password from stdin")
	socketPath := flags.String("control-socket", "", "running mount control socket (default: cache-dir/control.sock)")
	all := flags.Bool("all", false, "set the same password for archives in this cloud directory only")
	shared := flags.Bool("shared", false, "write one hidden .mount123.pwd for this directory and its descendants")
	skipValidation := flags.Bool("skip-validation", false, "save password sidecars without opening archives")
	overwrite := flags.Bool("overwrite", false, "replace existing sidecars (mounted-path mode)")
	timeout := flags.Duration("timeout", 10*time.Minute, "mounted request timeout, up to 30m")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	mounted := *fileID == 0 && flags.NArg() == 1
	if (*all && *shared) || (!mounted && (*all || *shared || *skipValidation || *overwrite)) || (!mounted && (*fileID <= 0 || flags.NArg() != 0)) || (mounted && (*timeout <= 0 || *timeout > 30*time.Minute)) {
		return errors.New("expected a mounted path, or -file-id ID; flags must precede the path")
	}

	password, err := readUnlockPassword(ctx, *passwordStdin, stdin, terminal, stderr, readTerminal)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		wipe(password)
		return err
	}
	defer wipe(password)
	if mounted {
		mountPath, err := filepath.Abs(flags.Arg(0))
		if err != nil {
			return err
		}
		if *socketPath == "" {
			*socketPath = controlSocketPath(*cacheDir)
		}
		return runMountedUnlock(ctx, *socketPath, mountPath, password, mountfs.UnlockOptions{All: *all, Shared: *shared, SkipValidation: *skipValidation, Overwrite: *overwrite}, *timeout, stdout)
	}

	var appConfig struct {
		ClientID     string `json:"CLIENT_ID"`
		ClientSecret string `json:"CLIENT_SECRET"`
	}
	token := os.Getenv("PAN123_ACCESS_TOKEN")
	configData, readErr := os.ReadFile(*configPath)
	if readErr != nil && token == "" {
		return fmt.Errorf("read config: %w", readErr)
	}
	if readErr == nil {
		if err := json.Unmarshal(configData, &appConfig); err != nil {
			return fmt.Errorf("invalid config JSON: %w", err)
		}
	}
	if deps.newAPI == nil || deps.validate == nil {
		return errors.New("unlock dependencies are unavailable")
	}
	if err := os.MkdirAll(*cacheDir, 0700); err != nil {
		return err
	}
	if err := os.Chmod(*cacheDir, 0700); err != nil {
		return err
	}
	cacheRoot, err := filepath.Abs(*cacheDir)
	if err != nil {
		return err
	}
	api, err := deps.newAPI(panapi.Config{
		ClientID: appConfig.ClientID, ClientSecret: appConfig.ClientSecret,
		AccessToken: token, TokenCache: filepath.Join(cacheRoot, "token.json"),
	})
	if err != nil {
		return err
	}

	// A private, one-command cache avoids taking the mount's exclusive cache lock.
	tempCacheDir, err := os.MkdirTemp(cacheRoot, ".unlock-cache-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempCacheDir)
	cache, err := storage.NewCache(tempCacheDir, unlockCacheBytes)
	if err != nil {
		return err
	}
	defer cache.Close()
	archive, err := api.Detail(ctx, *fileID)
	if err != nil {
		return fmt.Errorf("read archive metadata: %w", err)
	}
	if archive.ID != *fileID || archive.IsDir || archive.Trashed || !strings.HasSuffix(strings.ToLower(archive.Name), ".zip") {
		return errors.New("file ID must refer to an active ZIP archive")
	}
	source, err := storage.NewRemoteContext(ctx, ctx, cache, fmt.Sprintf("unlock:%d:%s", archive.ID, archive.Version), archive.Size,
		func(ctx context.Context) (string, error) { return api.DownloadURL(ctx, archive.ID) })
	if err != nil {
		return fmt.Errorf("open ZIP source: %w", err)
	}
	if err := deps.validate(ctx, source, archive.Size, password); err != nil {
		return fmt.Errorf("ZIP password validation failed; no sidecar was uploaded: %w", err)
	}
	if _, err := api.SaveZIPPassword(ctx, archive, password); err != nil {
		return fmt.Errorf("save ZIP password sidecar: %w", err)
	}
	_, err = fmt.Fprintf(stdout, "Saved ZIP password sidecar for %s\n", archive.Name)
	return err
}

func readUnlockPassword(ctx context.Context, fromStdin bool, stdin io.Reader, terminal bool, stderr io.Writer, readTerminal func() ([]byte, error)) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var password []byte
	var err error
	if fromStdin {
		// Permit at most 4096 password bytes plus one LF/CRLF terminator, then
		// detect further input instead of silently truncating it.
		password, err = readPasswordStdin(ctx, stdin)
		if err != nil {
			wipe(password)
			return nil, errors.New("could not read password from stdin")
		}
		if len(password) > maxUnlockPasswordBytes+2 {
			wipe(password)
			return nil, errors.New("Archive password exceeds 4096 bytes")
		}
		if len(password) > 0 && password[len(password)-1] == '\n' {
			password = password[:len(password)-1]
			if len(password) > 0 && password[len(password)-1] == '\r' {
				password = password[:len(password)-1]
			}
		}
	} else {
		if !terminal || readTerminal == nil {
			return nil, errors.New("use -password-stdin when stdin is not a terminal")
		}
		_, _ = io.WriteString(stderr, "Archive password: ")
		password, err = readTerminal()
		_, _ = io.WriteString(stderr, "\n")
		if err != nil {
			wipe(password)
			return nil, errors.New("could not read archive password")
		}
	}
	if len(password) == 0 || len(password) > maxUnlockPasswordBytes || !utf8.Valid(password) {
		wipe(password)
		return nil, errors.New("Archive password must be valid UTF-8 and 1 to 4096 bytes")
	}
	return password, nil
}

func readPasswordStdin(ctx context.Context, stdin io.Reader) ([]byte, error) {
	type result struct {
		password []byte
		err      error
	}
	finished := make(chan result)
	canceled := make(chan struct{})
	go func() {
		password, err := io.ReadAll(io.LimitReader(stdin, maxUnlockPasswordBytes+3))
		select {
		case finished <- result{password: password, err: err}:
		case <-canceled:
			wipe(password)
		}
	}()
	select {
	case got := <-finished:
		return got.password, got.err
	case <-ctx.Done():
		close(canceled)
		return nil, ctx.Err()
	}
}

func wipe(value []byte) {
	for i := range value {
		value[i] = 0
	}
}
