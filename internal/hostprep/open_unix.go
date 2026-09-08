//go:build unix

package hostprep

import (
	"os"

	"golang.org/x/sys/unix"
)

// openReadFile is a variable solely to make check/open replacement tests
// deterministic. O_NONBLOCK prevents a regular-to-FIFO swap from hanging the
// launcher; O_NOFOLLOW rejects a final-component symlink at the actual open.
var openReadFile = func(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
}
