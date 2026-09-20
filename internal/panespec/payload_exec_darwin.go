//go:build darwin

package panespec

import (
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

func closePayloadDescriptors() error {
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		fd, err := strconv.Atoi(entry.Name())
		if err == nil && fd >= 3 {
			unix.CloseOnExec(fd)
		}
	}
	return nil
}
