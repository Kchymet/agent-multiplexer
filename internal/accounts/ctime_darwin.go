package accounts

import (
	"os"
	"syscall"
)

// changeTime is the inode change time, which an in-place write moves even on
// a filesystem whose mtime is too coarse to show it.
func changeTime(info os.FileInfo) (int64, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Ctimespec.Nano(), true
}
