//go:build darwin

package hostprep

import (
	"os"

	"golang.org/x/sys/unix"
)

// Keep the destination anchored to the directory opened by Root, as on Linux.
func linkExternal(source string, dstDir *os.File, dstName string) error {
	return unix.Linkat(unix.AT_FDCWD, source, int(dstDir.Fd()), dstName, 0)
}
