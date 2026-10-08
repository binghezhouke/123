//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/binghezhouke/123/mount123/internal/iostats"
)

const (
	statsFileName       = "io-stats.jsonl"
	maxStatsFileBytes   = int64(8 << 20)
	statsErrorLogPeriod = time.Minute
)

type statsRecord struct {
	Snapshot iostats.Snapshot      `json:"snapshot"`
	Interval *iostats.TuningReport `json:"interval"`
}

func newStatsRecord(previous *iostats.Snapshot, current iostats.Snapshot) statsRecord {
	record := statsRecord{Snapshot: current}
	if previous != nil {
		report := iostats.Analyze(*previous, current)
		record.Interval = &report
	}
	return record
}

type statsSampler struct {
	cancel   context.CancelFunc
	done     chan struct{}
	path     string
	interval time.Duration
	snapshot func() iostats.Snapshot
	logf     func(string, ...any)
}

func startStatsSampler(parent context.Context, path string, interval time.Duration, snapshot func() iostats.Snapshot, logf func(string, ...any)) *statsSampler {
	if interval <= 0 || snapshot == nil {
		return nil
	}
	if parent == nil {
		parent = context.Background()
	}
	if logf == nil {
		logf = log.Printf
	}
	ctx, cancel := context.WithCancel(parent)
	sampler := &statsSampler{cancel: cancel, done: make(chan struct{}), path: path, interval: interval, snapshot: snapshot, logf: logf}
	go sampler.run(ctx)
	return sampler
}

func (s *statsSampler) Close() {
	if s == nil {
		return
	}
	s.cancel()
	<-s.done
}

func (s *statsSampler) run(ctx context.Context) {
	defer close(s.done)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	var previous *iostats.Snapshot
	var lastErrorLog time.Time
	loggedError := false
	record := func(syncFile bool) {
		current := s.snapshot()
		encoded, err := json.Marshal(newStatsRecord(previous, current))
		if err == nil {
			encoded = append(encoded, '\n')
			err = appendStatsLine(s.path, encoded, syncFile)
		}
		if err != nil {
			now := time.Now()
			if !loggedError || now.Sub(lastErrorLog) >= statsErrorLogPeriod {
				s.logf("could not append mount I/O statistics: %v", err)
				loggedError = true
				lastErrorLog = now
			}
			return
		}
		copy := current
		previous = &copy
	}

	// Capture startup state immediately. On any shutdown path, capture and sync
	// one final sample before the cache or mount tree is closed.
	record(false)
	for {
		select {
		case <-ctx.Done():
			record(true)
			return
		case <-ticker.C:
			record(false)
		}
	}
}

func appendStatsRecord(path string, record statsRecord, syncFile bool) error {
	line, err := json.Marshal(record)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	return appendStatsLine(path, line, syncFile)
}

func appendStatsLine(path string, line []byte, syncFile bool) error {
	if path == "" || len(line) == 0 {
		return errors.New("invalid statistics output")
	}
	if err := validateStatsDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	file, err := openStatsFile(path)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	if info.Size() > 0 && info.Size()+int64(len(line)) > maxStatsFileBytes {
		if err := file.Close(); err != nil {
			return err
		}
		if err := rotateStatsFile(path); err != nil {
			return err
		}
		file, err = openStatsFile(path)
		if err != nil {
			return err
		}
	}
	if err := writeStatsLine(file, line); err != nil {
		_ = file.Close()
		return err
	}
	if syncFile {
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return err
		}
	}
	return file.Close()
}

func validateStatsDirectory(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || !ownedByCurrentUser(info) || info.Mode().Perm()&0077 != 0 {
		return errors.New("statistics directory must be private and owned by the current user")
	}
	return nil
}

func openStatsFile(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() || !ownedByCurrentUser(info) {
		_ = file.Close()
		return nil, errors.New("statistics output must be a regular file owned by the current user")
	}
	if err := file.Chmod(0600); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func rotateStatsFile(path string) error {
	previous := path + ".1"
	if info, err := os.Lstat(previous); err == nil {
		if !info.Mode().IsRegular() || !ownedByCurrentUser(info) {
			return errors.New("rotated statistics output must be a regular file owned by the current user")
		}
		if err := os.Remove(previous); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(path, previous); err != nil {
		return err
	}
	return nil
}

func writeStatsLine(file *os.File, line []byte) error {
	for len(line) != 0 {
		n, err := file.Write(line)
		if n > 0 {
			line = line[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
