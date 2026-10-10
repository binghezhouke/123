//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/binghezhouke/123/mount123/internal/iostats"
	"github.com/binghezhouke/123/mount123/internal/mountfs"
	"golang.org/x/sys/unix"
)

const (
	controlExitFailed   = 2
	controlExitTimeout  = 3
	controlExitCanceled = 130
	// maxIOStatsSamples bounds continuous sampling output so a caller cannot
	// accidentally make an unbounded diagnostic stream consume disk or memory.
	maxIOStatsSamples = 10000
)

type commandExitError struct {
	code int
	err  error
}

func (e *commandExitError) Error() string { return e.err.Error() }
func (e *commandExitError) Unwrap() error { return e.err }

type controlRequest struct {
	Command        string                `json:"command,omitempty"`
	Path           string                `json:"path,omitempty"`
	Retry          bool                  `json:"retry,omitempty"`
	Password       []byte                `json:"password,omitempty"`
	Unlock         mountfs.UnlockOptions `json:"unlock,omitempty"`
	TimeoutSeconds int                   `json:"timeout_seconds,omitempty"`
}

type controlResponse struct {
	Status         *mountfs.ArchiveIndexStatus `json:"status,omitempty"`
	Doctor         *mountfs.PathDiagnosis      `json:"doctor,omitempty"`
	Refresh        *mountfs.RefreshResult      `json:"refresh,omitempty"`
	IOStats        *iostats.Snapshot           `json:"io_stats,omitempty"`
	Error          string                      `json:"error,omitempty"`
	UnlockProgress *mountfs.UnlockProgress     `json:"unlock_progress,omitempty"`
	UnlockSummary  *mountfs.UnlockSummary      `json:"unlock_summary,omitempty"`
}

type controlServer struct {
	listener *net.UnixListener
	path     string
	identity os.FileInfo
	root     *mountfs.Node
	mount    string
}

func startControlServer(path string, root *mountfs.Node, mountpoints ...string) (*controlServer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	if !dirInfo.IsDir() || !ownedByCurrentUser(dirInfo) || dirInfo.Mode().Perm()&0077 != 0 {
		return nil, errors.New("control socket directory must be private and owned by the current user")
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 || !ownedByCurrentUser(info) {
			return nil, errors.New("control socket path exists and is not an owned socket")
		}
		conn, dialErr := net.DialTimeout("unix", path, 100*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			return nil, errors.New("another mount123 control socket is active")
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, err
	}
	identity, err := os.Lstat(path)
	if err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, err
	}
	mountpoint := ""
	if len(mountpoints) > 0 {
		mountpoint = mountpoints[0]
	}
	return &controlServer{listener: listener, path: path, identity: identity, root: root, mount: mountpoint}, nil
}

func (s *controlServer) close() {
	_ = s.listener.Close()
	if current, err := os.Lstat(s.path); err == nil && os.SameFile(current, s.identity) {
		_ = os.Remove(s.path)
	}
}

func (s *controlServer) serve(ctx context.Context) {
	go func() {
		<-ctx.Done()
		_ = s.listener.Close()
	}()
	for {
		conn, err := s.listener.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		go s.handle(ctx, conn)
	}
}

func (s *controlServer) handle(ctx context.Context, conn *net.UnixConn) {
	defer conn.Close()
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()
	if !sameUserPeer(conn) {
		_ = json.NewEncoder(conn).Encode(controlResponse{Error: "control socket accepts only the current user"})
		return
	}
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	var request controlRequest
	defer func() { wipe(request.Password) }()
	if err := json.NewDecoder(io.LimitReader(conn, 16<<10)).Decode(&request); err != nil {
		_ = json.NewEncoder(conn).Encode(controlResponse{Error: "invalid control request"})
		return
	}
	if request.Command == "unlock" {
		s.handleUnlock(ctx, conn, request)
		return
	}
	if request.Command == "io-stats" {
		snapshot := s.root.IOStats()
		_ = json.NewEncoder(conn).Encode(controlResponse{IOStats: &snapshot})
		return
	}
	if request.Command != "" && request.Command != "status" && request.Command != "refresh" {
		if request.Command != "doctor" {
			_ = json.NewEncoder(conn).Encode(controlResponse{Error: "unsupported control command"})
			return
		}
	}
	archivePath := s.mountRelativePath(request.Path)
	if request.Command == "doctor" {
		doctorCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
		defer cancel()
		report, err := s.root.DiagnosePath(doctorCtx, archivePath, request.Retry)
		if err != nil {
			message := "could not complete path diagnosis"
			if errors.Is(err, context.DeadlineExceeded) {
				message = "doctor timed out; retry with a less busy path"
			} else if errors.Is(err, context.Canceled) {
				message = "mount is shutting down"
			}
			_ = json.NewEncoder(conn).Encode(controlResponse{Error: message})
			return
		}
		_ = json.NewEncoder(conn).Encode(controlResponse{Doctor: &report})
		return
	}
	if request.Command == "refresh" {
		refreshCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
		defer cancel()
		result, err := s.root.RefreshDirectory(refreshCtx, archivePath)
		if err != nil {
			_ = json.NewEncoder(conn).Encode(controlResponse{Error: safeRefreshError(err)})
			return
		}
		_ = json.NewEncoder(conn).Encode(controlResponse{Refresh: &result})
		return
	}
	status, err := s.root.StatusArchiveIndex(ctx, archivePath)
	if err != nil {
		_ = json.NewEncoder(conn).Encode(controlResponse{Error: safeControlError(err)})
		return
	}
	_ = json.NewEncoder(conn).Encode(controlResponse{Status: &status})
}

func safeRefreshError(err error) string {
	switch {
	case errors.Is(err, syscall.ENOENT):
		return "path not found in this mount"
	case errors.Is(err, syscall.ENOTDIR):
		return "path parent is not a directory"
	case errors.Is(err, syscall.EINVAL):
		return "refresh supports cloud directories only; archive and ISO directories cannot be refreshed"
	case errors.Is(err, context.Canceled):
		return "mount is shutting down"
	case errors.Is(err, context.DeadlineExceeded):
		return "refresh timed out; try again"
	default:
		return "could not refresh cloud directory"
	}
}

func (s *controlServer) mountRelativePath(value string) string {
	if filepath.IsAbs(value) {
		absolute := filepath.Clean(value)
		if s.mount != "" && isWithin(absolute, s.mount) {
			if relative, err := filepath.Rel(s.mount, absolute); err == nil {
				return filepath.ToSlash(relative)
			}
		}
		return strings.TrimLeft(absolute, string(filepath.Separator))
	}
	return value
}

func sameUserPeer(conn *net.UnixConn) bool {
	raw, err := conn.SyscallConn()
	if err != nil {
		return false
	}
	var credentials *unix.Ucred
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		credentials, socketErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil || socketErr != nil || credentials == nil {
		return false
	}
	return credentials.Uid == uint32(os.Getuid())
}

func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Getuid())
}

func safeControlError(err error) string {
	switch {
	case errors.Is(err, syscall.ENOENT):
		return "path not found in this mount"
	case errors.Is(err, syscall.ENOTDIR):
		return "path parent is not a directory"
	case errors.Is(err, syscall.EINVAL):
		return "path does not name a supported archive directory"
	case errors.Is(err, syscall.EACCES):
		return "archive password is unavailable or invalid"
	case errors.Is(err, context.Canceled):
		return "mount is shutting down"
	case errors.Is(err, context.DeadlineExceeded):
		return "refresh timed out; try again"
	default:
		return "could not inspect archive index"
	}
}

func runControlCommand(ctx context.Context, args []string, stderr, stdout io.Writer) error {
	command := args[0]
	flags := flag.NewFlagSet("mount123 "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	userCache, err := os.UserCacheDir()
	if err != nil {
		return err
	}
	socketPath := flags.String("control-socket", filepath.Join(userCache, "mount123", "control.sock"), "running mount's local control socket")
	defaultTimeout := 10 * time.Minute
	if command == "doctor" {
		defaultTimeout = 25 * time.Second
	}
	timeout := flags.Duration("timeout", defaultTimeout, "maximum command duration")
	retry := flags.Bool("retry", false, "retry a failed archive index")
	var statsInterval time.Duration
	var statsCount int
	if command == "io-stats" {
		flags.DurationVar(&statsInterval, "interval", 0, "sample interval (0 returns one snapshot)")
		flags.IntVar(&statsCount, "count", 0, "number of samples (0 continues until canceled; requires -interval)")
	}
	if command != "status" && command != "wait-index" && command != "refresh" && command != "io-stats" && command != "doctor" {
		return errors.New("unsupported control command")
	}
	flags.Usage = func() {
		if command == "io-stats" {
			_, _ = fmt.Fprintf(stderr, "Usage: mount123 io-stats [-control-socket PATH] [-interval DURATION] [-count N]\n")
		} else if command == "wait-index" {
			_, _ = fmt.Fprintf(stderr, "Usage: mount123 %s [-control-socket PATH] [-timeout DURATION] <mounted archive path>\n", command)
		} else if command == "refresh" {
			_, _ = fmt.Fprintf(stderr, "Usage: mount123 refresh [-control-socket PATH] <mounted cloud directory path>\n")
		} else if command == "doctor" {
			_, _ = fmt.Fprintf(stderr, "Usage: mount123 doctor [-control-socket PATH] [-timeout DURATION] [--retry] <mounted path>\n")
		} else {
			_, _ = fmt.Fprintf(stderr, "Usage: mount123 %s [-control-socket PATH] <mounted archive path>\n", command)
		}
		flags.PrintDefaults()
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if command == "io-stats" {
		if flags.NArg() != 0 {
			return errors.New("io-stats does not take a path")
		}
		if statsInterval < 0 || statsCount < 0 {
			return errors.New("io-stats interval and count must be non-negative")
		}
		if statsCount > maxIOStatsSamples {
			return fmt.Errorf("io-stats count must be at most %d", maxIOStatsSamples)
		}
		if statsInterval > 0 {
			return runIOStatsSampling(ctx, *socketPath, statsInterval, statsCount, stdout)
		}
		if statsCount != 0 {
			return errors.New("io-stats -count requires -interval")
		}
		snapshot, err := requestControlIOStats(ctx, *socketPath)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(snapshot)
	}
	if flags.NArg() != 1 || (command == "wait-index" && *timeout <= 0) || (command == "doctor" && (*timeout <= 0 || *timeout > 25*time.Second)) {
		return errors.New("expected one mounted archive path; flags must precede the path")
	}
	archivePath := flags.Arg(0)
	if command == "doctor" {
		requestCtx, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		report, err := requestControlDoctor(requestCtx, *socketPath, archivePath, *retry)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(requestCtx.Err(), context.DeadlineExceeded) {
				return &commandExitError{code: controlExitTimeout, err: errors.New("doctor timed out")}
			}
			if ctx.Err() != nil {
				return &commandExitError{code: controlExitCanceled, err: ctx.Err()}
			}
			return err
		}
		if err := json.NewEncoder(stdout).Encode(report); err != nil {
			return err
		}
		if report.State == "failed" {
			return &commandExitError{code: controlExitFailed, err: errors.New("path diagnosis found a failure")}
		}
		return nil
	}
	if command == "refresh" {
		result, err := requestControlRefresh(ctx, *socketPath, archivePath)
		if err != nil {
			if ctx.Err() != nil {
				return &commandExitError{code: controlExitCanceled, err: ctx.Err()}
			}
			return err
		}
		_, err = fmt.Fprintf(stdout, "entries=%d\n", result.Entries)
		return err
	}
	deadline := time.Now().Add(*timeout)
	for {
		requestCtx := ctx
		cancelRequest := func() {}
		if command == "wait-index" {
			requestCtx, cancelRequest = context.WithDeadline(ctx, deadline)
		}
		status, err := requestControlStatus(requestCtx, *socketPath, archivePath)
		cancelRequest()
		if err != nil {
			if ctx.Err() != nil {
				return &commandExitError{code: controlExitCanceled, err: ctx.Err()}
			}
			if command == "wait-index" && errors.Is(err, context.DeadlineExceeded) {
				return &commandExitError{code: controlExitTimeout, err: errors.New("timed out waiting for archive index")}
			}
			return err
		}
		if _, err := fmt.Fprintf(stdout, "state=%s members=%d scan_offset=%d archive_size=%d download_bytes=unknown\n", status.State, status.Members, status.ScanOffset, status.ArchiveSize); err != nil {
			return err
		}
		switch status.State {
		case "complete":
			return nil
		case "failed":
			return &commandExitError{code: controlExitFailed, err: errors.New("archive index failed")}
		case "queued", "scanning":
		default:
			return errors.New("mount returned an unknown archive index state")
		}
		if command == "status" {
			return nil
		}
		if time.Now().After(deadline) {
			return &commandExitError{code: controlExitTimeout, err: errors.New("timed out waiting for archive index")}
		}
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return &commandExitError{code: controlExitCanceled, err: ctx.Err()}
		case <-timer.C:
		}
	}
}

func runIOStatsSampling(ctx context.Context, socketPath string, interval time.Duration, count int, stdout io.Writer) error {
	if interval <= 0 || count < 0 {
		return errors.New("invalid io-stats sampling interval or count")
	}
	encoder := json.NewEncoder(stdout)
	var previous *iostats.Snapshot
	for emitted := 0; count == 0 || emitted < count; emitted++ {
		if err := ctx.Err(); err != nil {
			return &commandExitError{code: controlExitCanceled, err: err}
		}
		snapshot, err := requestControlIOStats(ctx, socketPath)
		if err != nil {
			if ctx.Err() != nil {
				err = ctx.Err()
				return &commandExitError{code: controlExitCanceled, err: err}
			}
			return err
		}
		record := newStatsRecord(previous, snapshot)
		if err := encoder.Encode(record); err != nil {
			return err
		}
		current := snapshot
		previous = &current
		if count > 0 && emitted+1 >= count {
			return nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return &commandExitError{code: controlExitCanceled, err: ctx.Err()}
		case <-timer.C:
		}
	}
	return nil
}

func requestControlStatus(ctx context.Context, socketPath, archivePath string) (mountfs.ArchiveIndexStatus, error) {
	conn, err := dialControl(ctx, socketPath)
	if err != nil {
		return mountfs.ArchiveIndexStatus{}, err
	}
	defer conn.Close()
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()
	// Resolving a deep path can require restoring several directory snapshots
	// before the archive status request is queued. Keep the wire deadline in
	// line with the server's 30-second handler deadline instead of failing
	// healthy requests after the old fixed five-second window.
	deadline := time.Now().Add(30 * time.Second)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	_ = conn.SetDeadline(deadline)
	if err := json.NewEncoder(conn).Encode(controlRequest{Command: "status", Path: archivePath}); err != nil {
		return mountfs.ArchiveIndexStatus{}, err
	}
	var response controlResponse
	if err := json.NewDecoder(io.LimitReader(conn, 16<<10)).Decode(&response); err != nil {
		return mountfs.ArchiveIndexStatus{}, err
	}
	if response.Error != "" {
		return mountfs.ArchiveIndexStatus{}, errors.New(response.Error)
	}
	if response.Status == nil {
		return mountfs.ArchiveIndexStatus{}, errors.New("mount returned an empty archive status")
	}
	return *response.Status, nil
}

func requestControlDoctor(ctx context.Context, socketPath, mountPath string, retry bool) (mountfs.PathDiagnosis, error) {
	conn, err := dialControl(ctx, socketPath)
	if err != nil {
		return mountfs.PathDiagnosis{}, err
	}
	defer conn.Close()
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	if err := json.NewEncoder(conn).Encode(controlRequest{Command: "doctor", Path: mountPath, Retry: retry}); err != nil {
		return mountfs.PathDiagnosis{}, err
	}
	var response controlResponse
	if err := json.NewDecoder(io.LimitReader(conn, 16<<10)).Decode(&response); err != nil {
		return mountfs.PathDiagnosis{}, err
	}
	if response.Error != "" {
		return mountfs.PathDiagnosis{}, errors.New(response.Error)
	}
	if response.Doctor == nil {
		return mountfs.PathDiagnosis{}, errors.New("mount returned an empty doctor result")
	}
	return *response.Doctor, nil
}

func requestControlRefresh(ctx context.Context, socketPath, directoryPath string) (mountfs.RefreshResult, error) {
	conn, err := dialControl(ctx, socketPath)
	if err != nil {
		return mountfs.RefreshResult{}, err
	}
	defer conn.Close()
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	if err := json.NewEncoder(conn).Encode(controlRequest{Command: "refresh", Path: directoryPath}); err != nil {
		return mountfs.RefreshResult{}, err
	}
	var response controlResponse
	if err := json.NewDecoder(io.LimitReader(conn, 16<<10)).Decode(&response); err != nil {
		return mountfs.RefreshResult{}, err
	}
	if response.Error != "" {
		return mountfs.RefreshResult{}, errors.New(response.Error)
	}
	if response.Refresh == nil {
		return mountfs.RefreshResult{}, errors.New("mount returned an empty refresh result")
	}
	return *response.Refresh, nil
}

func requestControlIOStats(ctx context.Context, socketPath string) (iostats.Snapshot, error) {
	conn, err := dialControl(ctx, socketPath)
	if err != nil {
		return iostats.Snapshot{}, err
	}
	defer conn.Close()
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := json.NewEncoder(conn).Encode(controlRequest{Command: "io-stats"}); err != nil {
		return iostats.Snapshot{}, err
	}
	var response controlResponse
	if err := json.NewDecoder(io.LimitReader(conn, 16<<10)).Decode(&response); err != nil {
		return iostats.Snapshot{}, err
	}
	if response.Error != "" {
		return iostats.Snapshot{}, errors.New(response.Error)
	}
	if response.IOStats == nil {
		return iostats.Snapshot{}, errors.New("mount returned empty I/O statistics")
	}
	return *response.IOStats, nil
}

func dialControl(ctx context.Context, socketPath string) (net.Conn, error) {
	info, err := os.Lstat(socketPath)
	if err != nil {
		return nil, fmt.Errorf("mount control socket unavailable: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 || !ownedByCurrentUser(info) || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("control socket is not a private socket owned by the current user")
	}
	dialer := net.Dialer{Timeout: 2 * time.Second}
	conn, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func controlSocketPath(cacheDir string) string { return filepath.Join(cacheDir, "control.sock") }

func isWithin(path, base string) bool {
	rel, err := filepath.Rel(base, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func notifyContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}
