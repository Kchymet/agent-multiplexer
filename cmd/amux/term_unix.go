//go:build linux || darwin

package main

import (
	"bufio"
	"errors"
	"io"
	"os"

	"github.com/mattn/go-isatty"
	"golang.org/x/sys/unix"
)

func isTerminal(f *os.File) bool { return isatty.IsTerminal(f.Fd()) }

// readHiddenLine reads one line from a terminal with echo disabled, restoring
// the terminal state afterwards even if the read fails.
func readHiddenLine(f *os.File) (string, error) {
	fd := int(f.Fd())
	old, err := unix.IoctlGetTermios(fd, termiosGet)
	if err != nil {
		return "", err
	}
	quiet := *old
	quiet.Lflag &^= unix.ECHO
	quiet.Lflag |= unix.ICANON | unix.ISIG
	if err := unix.IoctlSetTermios(fd, termiosSet, &quiet); err != nil {
		return "", err
	}
	defer unix.IoctlSetTermios(fd, termiosSet, old)
	line, err := bufio.NewReader(io.LimitReader(f, 16<<10)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return line, nil
}
