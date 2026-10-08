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

	"github.com/binghezhouke/123/mount123/internal/mountfs"
	"golang.org/x/sys/unix"
)

const (
	controlExitFailed   = 2
	controlExitTimeout  = 3
	controlExitCanceled = 130
)

type commandExitError struct {
	code int
	err  error
}

func (e *commandExitError) Error() string { return e.err.Error() }
func (e *commandExitError) Unwrap() error { return e.err }

type controlRequest struct {
	Path string `json:"path"`
}

type controlResponse struct {
	Status *mountfs.ArchiveIndexStatus `json:"status,omitempty"`
	Error  string                      `json:"error,omitempty"`
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
	if err := json.NewDecoder(io.LimitReader(conn, 16<<10)).Decode(&request); err != nil {
		_ = json.NewEncoder(conn).Encode(controlResponse{Error: "invalid control request"})
		return
	}
	archivePath := request.Path
	if filepath.IsAbs(archivePath) {
		absolute := filepath.Clean(archivePath)
		if s.mount != "" && isWithin(absolute, s.mount) {
			if relative, err := filepath.Rel(s.mount, absolute); err == nil {
				archivePath = filepath.ToSlash(relative)
			}
		} else {
			archivePath = strings.TrimLeft(archivePath, string(filepath.Separator))
		}
	}
	status, err := s.root.StatusArchiveIndex(ctx, archivePath)
	if err != nil {
		_ = json.NewEncoder(conn).Encode(controlResponse{Error: safeControlError(err)})
		return
	}
	_ = json.NewEncoder(conn).Encode(controlResponse{Status: &status})
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
	timeout := flags.Duration("timeout", 10*time.Minute, "maximum wait-index duration")
	flags.Usage = func() {
		if command == "wait-index" {
			_, _ = fmt.Fprintf(stderr, "Usage: mount123 %s [-control-socket PATH] [-timeout DURATION] <mounted archive path>\n", command)
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
	if flags.NArg() != 1 || (command == "wait-index" && *timeout <= 0) {
		return errors.New("expected one mounted archive path; flags must precede the path")
	}
	archivePath := flags.Arg(0)
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

func requestControlStatus(ctx context.Context, socketPath, archivePath string) (mountfs.ArchiveIndexStatus, error) {
	info, err := os.Lstat(socketPath)
	if err != nil {
		return mountfs.ArchiveIndexStatus{}, fmt.Errorf("mount control socket unavailable: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 || !ownedByCurrentUser(info) || info.Mode().Perm()&0077 != 0 {
		return mountfs.ArchiveIndexStatus{}, errors.New("control socket is not a private socket owned by the current user")
	}
	dialer := net.Dialer{Timeout: 2 * time.Second}
	conn, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return mountfs.ArchiveIndexStatus{}, err
	}
	defer conn.Close()
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := json.NewEncoder(conn).Encode(controlRequest{Path: archivePath}); err != nil {
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

func controlSocketPath(cacheDir string) string { return filepath.Join(cacheDir, "control.sock") }

func isWithin(path, base string) bool {
	rel, err := filepath.Rel(base, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func notifyContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}
