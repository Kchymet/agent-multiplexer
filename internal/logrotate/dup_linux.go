package logrotate

import "golang.org/x/sys/unix"

// dupOnto atomically makes fd refer to the same open file as src.
func dupOnto(src, fd int) error { return unix.Dup3(src, fd, 0) }
