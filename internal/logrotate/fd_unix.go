//go:build linux || darwin

package logrotate

import (
	"os"
	"syscall"
)

// fdIsFile reports whether descriptor fd refers to the file want describes. It
// uses fstat directly rather than wrapping fd in an *os.File, whose finalizer
// would close the descriptor (stderr!) out from under its owner.
func fdIsFile(fd int, want os.FileInfo) bool {
	ws, ok := want.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return false
	}
	return st.Dev == ws.Dev && st.Ino == ws.Ino
}
