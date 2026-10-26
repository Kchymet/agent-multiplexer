//go:build darwin

package panespec

import (
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

func closePayloadDescriptors() error {
	dir, err := os.Open("/dev/fd")
	if err != nil {
		return err
	}
	// Names only: os.ReadDir stats each entry afterwards, and an entry closed
	// in between (the listing's own descriptor among them) fails that stat
	// with EBADF, which aborted the protected launch.
	names, err := dir.Readdirnames(-1)
	_ = dir.Close()
	if err != nil {
		return err
	}
	for _, name := range names {
		fd, err := strconv.Atoi(name)
		if err == nil && fd >= 3 {
			unix.CloseOnExec(fd)
		}
	}
	return nil
}
