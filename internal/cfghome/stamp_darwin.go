//go:build darwin

package cfghome

import (
	"io/fs"
	"syscall"
)

// stampOf captures the identity and change times a reused hash must match.
func stampOf(fi fs.FileInfo) (fileStamp, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || !fi.Mode().IsRegular() {
		return fileStamp{}, false
	}
	return fileStamp{
		Dev: uint64(st.Dev), Ino: uint64(st.Ino), Nlink: uint64(st.Nlink), Size: st.Size, Mode: uint32(st.Mode),
		Mtime: syscall.TimespecToNsec(st.Mtimespec), Ctime: syscall.TimespecToNsec(st.Ctimespec),
	}, true
}
