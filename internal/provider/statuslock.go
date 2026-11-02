package provider

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// ErrStatusHeld means another `amux provide` on this host already owns the
// status file.
var ErrStatusHeld = errors.New("another amux provide is already running on this host")

// LockStatus makes the calling process the only provider writing the status
// file at path. The status file is single-writer state: two providers would
// overwrite each other's registration (and with it the owner and provider id
// that `amux direct cloud enable` defaults its pins to, and what `amux doctor`
// reports). The flock is released when the process exits, however it exits, so
// a crashed provider never blocks its successor. A second caller fails with
// ErrStatusHeld naming the holder's pid.
func LockStatus(path string) (release func(), err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder, _ := os.ReadFile(path + ".lock")
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			if pid, perr := strconv.Atoi(strings.TrimSpace(string(holder))); perr == nil && pid > 0 {
				return nil, fmt.Errorf("%w (pid %d); stop it first", ErrStatusHeld, pid)
			}
			return nil, fmt.Errorf("%w; stop it first", ErrStatusHeld)
		}
		return nil, err
	}
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}
