// Package logrotate bounds the daemon log by size.
//
// The daemon does not own its log as a Go writer: the process that spawns it
// opens ~/.local/state/amux/daemon.log and hands it over as stdout and stderr,
// so every log line, child-process inheritance and — crucially — a Go runtime
// panic trace go straight to that file descriptor. Wrapping os.Stderr in a
// writer would lose the panic trace. Instead, Watch periodically checks the
// file behind the descriptors and, once it outgrows the limit, shifts it to
// daemon.log.1 (… .N), opens a fresh daemon.log and dup()s it onto the same
// descriptor numbers. Writers never notice; the old generation stays complete.
package logrotate

import (
	"context"
	"fmt"
	"os"
	"time"
)

// Defaults: at most DefaultMaxBytes in the live log plus DefaultKeep rotated
// generations, so the daemon log never takes more than ~256 MiB of disk.
const (
	DefaultMaxBytes = 64 << 20
	DefaultKeep     = 3
	DefaultInterval = time.Minute
)

// Policy describes one rotated log.
type Policy struct {
	// Path is the live log file.
	Path string
	// MaxBytes is the size at or above which the live log is rotated.
	MaxBytes int64
	// Keep is how many rotated generations (Path.1 … Path.Keep) are kept.
	Keep int
	// FDs are the descriptors that may point at Path (stdout and stderr in
	// production). Only those currently referring to Path are redirected.
	FDs []int
}

// Watch checks the policy immediately and then every interval until ctx ends.
// Rotation failures are reported through logf and retried on the next tick.
func Watch(ctx context.Context, p Policy, interval time.Duration, logf func(string, ...any)) {
	if interval <= 0 {
		interval = DefaultInterval
	}
	check := func() {
		if rotated, err := RotateIfLarge(p); err != nil {
			logf("daemon log rotation failed: %v", err)
		} else if rotated {
			logf("daemon log rotated: previous log is %s.1 (keeping %d generations of %d MiB)", p.Path, p.Keep, p.MaxBytes>>20)
		}
	}
	check()
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				check()
			}
		}
	}()
}

// RotateIfLarge rotates p.Path when it is at least p.MaxBytes and at least one
// of p.FDs writes to it. It reports whether it rotated. A log that is missing,
// small, or not ours (the daemon runs in a terminal) is left alone.
func RotateIfLarge(p Policy) (bool, error) {
	if p.MaxBytes <= 0 || p.Keep < 1 {
		return false, nil
	}
	fi, err := os.Stat(p.Path)
	if err != nil || fi.Size() < p.MaxBytes {
		return false, nil
	}
	targets := fdsWritingTo(p.FDs, fi)
	if len(targets) == 0 {
		return false, nil
	}
	for i := p.Keep - 1; i >= 1; i-- {
		older := fmt.Sprintf("%s.%d", p.Path, i)
		if _, err := os.Lstat(older); err == nil {
			if err := os.Rename(older, fmt.Sprintf("%s.%d", p.Path, i+1)); err != nil {
				return false, err
			}
		}
	}
	if err := os.Rename(p.Path, p.Path+".1"); err != nil {
		return false, err
	}
	f, err := os.OpenFile(p.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, fi.Mode().Perm())
	if err != nil {
		return false, err
	}
	defer f.Close()
	for _, fd := range targets {
		if err := dupOnto(int(f.Fd()), fd); err != nil {
			return false, fmt.Errorf("redirect fd %d: %w", fd, err)
		}
	}
	return true, nil
}

func fdsWritingTo(fds []int, want os.FileInfo) []int {
	var out []int
	for _, fd := range fds {
		if fdIsFile(fd, want) {
			out = append(out, fd)
		}
	}
	return out
}
