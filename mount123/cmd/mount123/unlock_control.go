//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"syscall"
	"time"

	"github.com/binghezhouke/123/mount123/internal/mountfs"
)

func (s *controlServer) handleUnlock(ctx context.Context, conn *net.UnixConn, request controlRequest) {
	encoder := json.NewEncoder(conn)
	// Mutations must never interpret an unrelated absolute path as a cloud path.
	if request.Path == "" || len(request.Path) > 4096 || (filepath.IsAbs(request.Path) && (s.mount == "" || !isWithin(filepath.Clean(request.Path), s.mount))) {
		_ = encoder.Encode(controlResponse{Error: "unlock path must belong to this mount"})
		return
	}
	seconds := request.TimeoutSeconds
	if seconds == 0 {
		seconds = 600
	}
	if seconds < 1 || seconds > 1800 {
		_ = encoder.Encode(controlResponse{Error: "unlock timeout must be between 1s and 30m"})
		return
	}
	duration := time.Duration(seconds) * time.Second
	_ = conn.SetDeadline(time.Now().Add(duration + 30*time.Second))
	workCtx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	// The client keeps its write half open. Disconnect/Ctrl+C stops subsequent
	// uploads; the backend still refreshes any already-saved sidecars.
	go func() {
		var extra [1]byte
		_, _ = conn.Read(extra[:])
		cancel()
	}()
	summary, err := s.root.UnlockArchives(workCtx, s.mountRelativePath(request.Path), request.Password, request.Unlock, func(progress mountfs.UnlockProgress) error {
		if err := encoder.Encode(controlResponse{UnlockProgress: &progress}); err != nil {
			cancel()
			return err
		}
		return nil
	})
	response := controlResponse{UnlockSummary: &summary}
	if err != nil {
		response.Error = safeUnlockError(err)
	}
	_ = encoder.Encode(response)
}

func safeUnlockError(err error) string {
	switch {
	case errors.Is(err, syscall.ENOENT):
		return "path not found in this mount"
	case errors.Is(err, syscall.EINVAL):
		return "expected a cloud archive or directory, and a single-line UTF-8 password"
	case errors.Is(err, syscall.EFBIG):
		return "too many archives; at most 1000 are supported per batch"
	case errors.Is(err, syscall.EOPNOTSUPP):
		return "this mount cannot upload password sidecars"
	case errors.Is(err, context.Canceled):
		return "unlock cancelled; already-saved sidecars are retained"
	case errors.Is(err, context.DeadlineExceeded):
		return "unlock timed out; already-saved sidecars are retained"
	default:
		return "could not set archive passwords"
	}
}

func runMountedUnlock(ctx context.Context, socketPath, mountPath string, password []byte, options mountfs.UnlockOptions, timeout time.Duration, stdout io.Writer) error {
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	summary, err := requestMountedUnlock(requestCtx, socketPath, mountPath, password, options, timeout, func(progress mountfs.UnlockProgress) error {
		if progress.Reason != "" {
			_, err := fmt.Fprintf(stdout, "%s %q reason=%s\n", progress.State, progress.Name, progress.Reason)
			return err
		}
		_, err := fmt.Fprintf(stdout, "%s %q\n", progress.State, progress.Name)
		return err
	})
	if summary != nil {
		if _, writeErr := fmt.Fprintf(stdout, "total=%d saved=%d skipped=%d failed=%d refreshed=%t\n", summary.Total, summary.Saved, summary.Skipped, summary.Failed, summary.Refreshed); writeErr != nil {
			return writeErr
		}
	}
	if ctx.Err() != nil {
		return &commandExitError{code: controlExitCanceled, err: ctx.Err()}
	}
	if errors.Is(requestCtx.Err(), context.DeadlineExceeded) {
		return &commandExitError{code: controlExitTimeout, err: errors.New("unlock timed out; check saved sidecars before retrying")}
	}
	if err != nil {
		return err
	}
	if summary == nil {
		return errors.New("mount returned no unlock summary")
	}
	if summary.Failed > 0 || !summary.Refreshed {
		return &commandExitError{code: controlExitFailed, err: errors.New("some passwords failed or refresh failed; saved sidecars are retained")}
	}
	return nil
}

func requestMountedUnlock(ctx context.Context, socketPath, mountPath string, password []byte, options mountfs.UnlockOptions, timeout time.Duration, progress func(mountfs.UnlockProgress) error) (*mountfs.UnlockSummary, error) {
	conn, err := dialControl(ctx, socketPath)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()
	_ = conn.SetDeadline(time.Now().Add(timeout + 30*time.Second))
	request := controlRequest{Command: "unlock", Path: mountPath, Password: password, Unlock: options, TimeoutSeconds: int((timeout + time.Second - 1) / time.Second)}
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(io.LimitReader(conn, 1<<20))
	for {
		var response controlResponse
		if err := decoder.Decode(&response); err != nil {
			return nil, errors.New("mount unlock connection closed; already-saved sidecars are retained")
		}
		if response.UnlockProgress != nil && progress != nil {
			if err := progress(*response.UnlockProgress); err != nil {
				return nil, err
			}
		}
		if response.Error != "" {
			return response.UnlockSummary, errors.New(response.Error)
		}
		if response.UnlockSummary != nil {
			return response.UnlockSummary, nil
		}
		if response.UnlockProgress == nil {
			return nil, errors.New("mount returned an empty unlock response")
		}
	}
}
