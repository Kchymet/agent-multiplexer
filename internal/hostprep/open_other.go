//go:build !unix

package hostprep

import "os"

// Non-Unix platforms do not expose Unix FIFOs. os.Root still supplies anchored
// containment; descriptor identity and regular-file validation happen after
// this open as on Unix.
var openReadFile = func(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY, 0)
}
