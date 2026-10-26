package core

import (
	"os"
	"syscall"
)

// fileIdentity returns the device and inode of info, or zeros when the
// platform does not expose them (forcing full captures).
func fileIdentity(info os.FileInfo) (dev, ino uint64) {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Dev), uint64(st.Ino)
	}
	return 0, 0
}
