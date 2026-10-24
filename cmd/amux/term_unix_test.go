//go:build linux || darwin

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

const (
	hiddenReadChildEnv = "AMUX_TEST_HIDDEN_READ_CHILD"
	hiddenReadReady    = "hidden-read-ready\n"
)

// TestHiddenReadChild is the subprocess the PTY tests drive: it reads one
// hidden line from its controlling terminal and records what it read.
func TestHiddenReadChild(t *testing.T) {
	out := os.Getenv(hiddenReadChildEnv)
	if out == "" {
		t.Skip("PTY test helper")
	}
	// The marker orders the parent after the binary's startup terminal queries.
	_, _ = os.Stdout.WriteString(hiddenReadReady)
	line, err := readHiddenLine(os.Stdin)
	if err != nil {
		_ = os.WriteFile(out, []byte("error: "+err.Error()), 0o600)
		os.Exit(3)
	}
	_ = os.WriteFile(out, []byte(line), 0o600)
	os.Exit(0)
}

type hiddenReadPTY struct {
	t      *testing.T
	master *os.File
	slave  *os.File
	cmd    *exec.Cmd
	result string

	mu   sync.Mutex
	echo bytes.Buffer // everything the terminal displayed
}

// startHiddenRead runs the helper as the session leader of a fresh PTY and
// waits until it has turned echo off.
func startHiddenRead(t *testing.T) *hiddenReadPTY {
	t.Helper()
	master, slave, err := pty.Open()
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	p := &hiddenReadPTY{t: t, master: master, slave: slave, result: filepath.Join(t.TempDir(), "line")}
	t.Cleanup(func() { _ = master.Close(); _ = slave.Close() })
	p.cmd = exec.Command(os.Args[0], "-test.run=^TestHiddenReadChild$")
	p.cmd.Env = append(os.Environ(), hiddenReadChildEnv+"="+p.result)
	p.cmd.Stdin, p.cmd.Stdout, p.cmd.Stderr = slave, slave, slave
	p.cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.cmd.Process.Kill() })
	go func() {
		b := make([]byte, 4096)
		for {
			n, err := master.Read(b)
			// Answer the binary's startup terminal queries (background color,
			// cursor position) as a real terminal would.
			if bytes.Contains(b[:n], []byte("\x1b]11;?")) {
				_, _ = master.Write([]byte("\x1b]11;rgb:0000/0000/0000\x1b\\"))
			}
			if bytes.Contains(b[:n], []byte("\x1b[6n")) {
				_, _ = master.Write([]byte("\x1b[1;1R"))
			}
			p.mu.Lock()
			p.echo.Write(b[:n])
			p.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(p.displayed(), strings.TrimSpace(hiddenReadReady)); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("helper never reached the prompt; terminal showed %q", p.displayed())
		}
	}
	p.waitEcho(false)
	return p
}

func (p *hiddenReadPTY) echoOn() bool {
	tio, err := unix.IoctlGetTermios(int(p.slave.Fd()), termiosGet)
	if err != nil {
		p.t.Fatal(err)
	}
	return tio.Lflag&unix.ECHO != 0
}

func (p *hiddenReadPTY) waitEcho(on bool) {
	p.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if p.echoOn() == on {
			return
		}
	}
	p.t.Fatalf("terminal echo never became %v", on)
}

// wait4 reaps (or, with WUNTRACED, observes a stop of) the helper.
func (p *hiddenReadPTY) wait4(options int) syscall.WaitStatus {
	p.t.Helper()
	var ws syscall.WaitStatus
	done := make(chan error, 1)
	go func() {
		_, err := syscall.Wait4(p.cmd.Process.Pid, &ws, options, nil)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			p.t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		p.t.Fatalf("helper did not change state; terminal showed %q", p.displayed())
	}
	return ws
}

func (p *hiddenReadPTY) displayed() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.echo.String()
}

func TestReadHiddenLineReadsWithoutEchoAndRestores(t *testing.T) {
	p := startHiddenRead(t)
	const secret = "sk-ant-oat01-hidden-0123456789"
	if _, err := p.master.Write([]byte(secret + "\n")); err != nil {
		t.Fatal(err)
	}
	if ws := p.wait4(0); !ws.Exited() || ws.ExitStatus() != 0 {
		t.Fatalf("helper status %v", ws)
	}
	got, _ := os.ReadFile(p.result)
	if strings.TrimSpace(string(got)) != secret {
		t.Fatalf("read %q", got)
	}
	if !p.echoOn() {
		t.Fatal("echo not restored after a completed read")
	}
	time.Sleep(50 * time.Millisecond)
	if strings.Contains(p.displayed(), "hidden-0123456789") {
		t.Fatalf("token was echoed: %q", p.displayed())
	}
}

func TestReadHiddenLineInterruptRestoresAndEndsWithTheSignal(t *testing.T) {
	p := startHiddenRead(t)
	if _, err := p.master.Write([]byte("partial-secret\x03")); err != nil { // typed text, then Ctrl-C
		t.Fatal(err)
	}
	ws := p.wait4(0)
	if !ws.Signaled() || ws.Signal() != syscall.SIGINT {
		t.Fatalf("helper status %v, want killed by SIGINT", ws)
	}
	if !p.echoOn() {
		t.Fatal("Ctrl-C left the terminal without echo")
	}
	// The re-raised signal ends the helper asynchronously, so it may record
	// an empty or error result first; it must never return the typed text.
	if b, _ := os.ReadFile(p.result); strings.Contains(string(b), "partial-secret") {
		t.Fatalf("an interrupted read returned the typed text: %q", b)
	}
}

func TestReadHiddenLineTerminationAndHangupRestore(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT} {
		t.Run(sig.String(), func(t *testing.T) {
			p := startHiddenRead(t)
			if err := p.cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			ws := p.wait4(0)
			// Go's default SIGQUIT action dumps goroutines and exits 2.
			if killed := ws.Signaled() && ws.Signal() == sig; !killed && !(sig == syscall.SIGQUIT && ws.Exited() && ws.ExitStatus() == 2) {
				t.Fatalf("helper status %v, want ended by %v", ws, sig)
			}
			if !p.echoOn() {
				t.Fatalf("%v left the terminal without echo", sig)
			}
		})
	}
}

func TestReadHiddenLineSuspendRestoresThenHidesOnResume(t *testing.T) {
	p := startHiddenRead(t)
	if err := p.cmd.Process.Signal(syscall.SIGTSTP); err != nil {
		t.Fatal(err)
	}
	ws := p.wait4(syscall.WUNTRACED)
	if !ws.Stopped() {
		t.Fatalf("helper status %v, want stopped", ws)
	}
	if !p.echoOn() {
		t.Fatal("a suspended token prompt left the terminal without echo")
	}
	if err := p.cmd.Process.Signal(syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	p.waitEcho(false)
	if _, err := p.master.Write([]byte("after-resume\n")); err != nil {
		t.Fatal(err)
	}
	if ws := p.wait4(0); !ws.Exited() || ws.ExitStatus() != 0 {
		t.Fatalf("helper status %v", ws)
	}
	if got, _ := os.ReadFile(p.result); strings.TrimSpace(string(got)) != "after-resume" {
		t.Fatalf("read %q after resume", got)
	}
	if !p.echoOn() {
		t.Fatal("echo not restored after the resumed read")
	}
}
