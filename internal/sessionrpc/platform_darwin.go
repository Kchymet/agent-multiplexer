//go:build darwin

package sessionrpc

import (
	"time"

	"golang.org/x/sys/unix"
)

func renameNoReplace(oldDir int, oldName string, newDir int, newName string) error {
	return unix.RenameatxNp(oldDir, oldName, newDir, newName, unix.RENAME_EXCL)
}

func statModTime(stat unix.Stat_t) time.Time {
	return time.Unix(stat.Mtim.Sec, stat.Mtim.Nsec)
}

func statChangeTime(stat unix.Stat_t) time.Time {
	return time.Unix(stat.Ctim.Sec, stat.Ctim.Nsec)
}
