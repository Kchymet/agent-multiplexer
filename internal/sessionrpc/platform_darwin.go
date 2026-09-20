//go:build darwin

package sessionrpc

import (
	"time"

	"golang.org/x/sys/unix"
)

// Intermediate descriptors are only for anchored traversal. O_SEARCH avoids
// asking Seatbelt for directory contents outside the granted mailbox. Darwin's
// sys/fcntl.h defines O_SEARCH as O_EXEC | O_DIRECTORY; x/sys omits O_EXEC.
const traversalOpenFlag = 0x40000000 | unix.O_DIRECTORY

func renameNoReplace(oldDir int, oldName string, newDir int, newName string) error {
	return unix.RenameatxNp(oldDir, oldName, newDir, newName, unix.RENAME_EXCL)
}

func statModTime(stat unix.Stat_t) time.Time {
	return time.Unix(stat.Mtim.Sec, stat.Mtim.Nsec)
}

func statChangeTime(stat unix.Stat_t) time.Time {
	return time.Unix(stat.Ctim.Sec, stat.Ctim.Nsec)
}
