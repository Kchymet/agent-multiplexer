//go:build linux

package hostprep

import (
	"os"

	"golang.org/x/sys/unix"
)

// linkat names the destination relative to the already-open directory, so a
// replaced session pathname cannot redirect it. Linux is the production
// isolation target; other platforms fail closed for this special grant.
func linkExternal(source string, dstDir *os.File, dstName string) error {
	return unix.Linkat(unix.AT_FDCWD, source, int(dstDir.Fd()), dstName, 0)
}
