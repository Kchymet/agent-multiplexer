//go:build linux || darwin

package main

import (
	"bufio"
	"errors"
	"io"
	"os"
	"os/signal"
	"time"

	"github.com/mattn/go-isatty"
	"golang.org/x/sys/unix"
)

func isTerminal(f *os.File) bool { return isatty.IsTerminal(f.Fd()) }

// hiddenReadSignals end or suspend the process while echo is off. Their
// default actions would leave the user's terminal without echo, so
// readHiddenLine restores it first and then lets each take effect.
var hiddenReadSignals = []os.Signal{unix.SIGINT, unix.SIGTERM, unix.SIGHUP, unix.SIGQUIT, unix.SIGTSTP}

// readHiddenLine reads one line from a terminal with echo disabled, restoring
// the terminal state afterwards on every path: a completed or failed read,
// and an interrupt, termination, hangup or quit, which then ends the process
// with that same signal without returning. A suspend restores the terminal
// while stopped and hides input again on resume.
func readHiddenLine(f *os.File) (string, error) {
	fd := int(f.Fd())
	old, err := unix.IoctlGetTermios(fd, termiosGet)
	if err != nil {
		return "", err
	}
	quiet := *old
	quiet.Lflag &^= unix.ECHO
	quiet.Lflag |= unix.ICANON | unix.ISIG
	restore := func() { _ = unix.IoctlSetTermios(fd, termiosSet, old) }

	// Subscribe before echo goes off, so no signal can arrive unobserved.
	// Signals ignored at startup (nohup, a background job) stay ignored.
	sigs := make(chan os.Signal, 1)
	for _, sig := range hiddenReadSignals {
		if !signal.Ignored(sig) {
			signal.Notify(sigs, sig)
		}
	}
	defer signal.Stop(sigs)
	if err := unix.IoctlSetTermios(fd, termiosSet, &quiet); err != nil {
		return "", err
	}
	defer restore()

	type result struct {
		line string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		line, err := bufio.NewReader(io.LimitReader(f, 16<<10)).ReadString('\n')
		done <- result{line, err}
	}()
	for {
		select {
		case r := <-done:
			if r.err != nil && !errors.Is(r.err, io.EOF) {
				return "", r.err
			}
			return r.line, nil
		case sig := <-sigs:
			restore()
			if sig != unix.SIGTSTP {
				signal.Stop(sigs)
				terminate(sig.(unix.Signal))
			}
			// Stop with echo restored. kill does not stop this thread
			// synchronously, so hide input again only once resumed.
			cont := make(chan os.Signal, 1)
			signal.Notify(cont, unix.SIGCONT)
			_ = unix.Kill(os.Getpid(), unix.SIGSTOP)
			<-cont
			signal.Stop(cont)
			_ = unix.IoctlSetTermios(fd, termiosSet, &quiet)
		}
	}
}

// terminate ends the process with sig's default action after the terminal is
// restored, and never returns. Only signals not ignored at startup reach here,
// and Reset restores their default (terminating) action. Delivery of a signal
// a process sends itself is asynchronous, so returning to the caller could let
// it exit first with its own status; wait for the signal instead, and use the
// conventional 128+signal status only if it has somehow not ended the process.
func terminate(sig unix.Signal) {
	signal.Reset(sig)
	_ = unix.Kill(os.Getpid(), sig)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	os.Exit(128 + int(sig))
}
