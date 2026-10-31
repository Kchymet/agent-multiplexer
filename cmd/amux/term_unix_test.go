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
	_, _ = os.Stdout.WriteString(hiddenReadReady)
	line, err := readHiddenLine(os.Stdin)
	if err != nil {
		_ = os.WriteFile(out, []byte("error: "+err.Error()), 0o600)
		os.Exit(3)
	}
	_ = os.WriteFile(out, []byte(line), 0o600)
	os.Exit(0)
}

const startupQueryChildEnv = "AMUX_TEST_STARTUP_QUERY_CHILD"

// TestStartupQueryChild is the subprocess TestNoStartupTerminalQuery runs on a
// terminal: by the time a test body runs, every package init has.
func TestStartupQueryChild(t *testing.T) {
	if os.Getenv(startupQueryChildEnv) == "" {
		t.Skip("PTY test helper")
	}
	_, _ = os.Stdout.WriteString("startup-done\n")
	os.Exit(0)
}

// Bubble Tea's package init asks the terminal for its background color and
// waits up to 5s for an answer. amux settles that answer itself (package
// termbg), so a binary started on a terminal that never answers neither sends
// the query nor stalls.
func TestNoStartupTerminalQuery(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	defer master.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestStartupQueryChild$")
	cmd.Env = append(os.Environ(), startupQueryChildEnv+"=1", "TERM=xterm-256color")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	start := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = slave.Close()
	var out bytes.Buffer
	b := make([]byte, 4096)
	for !strings.Contains(out.String(), "startup-done") {
		n, err := master.Read(b) // never answered, as by a terminal that ignores queries
		out.Write(b[:n])
		if err != nil {
			break
		}
	}
	elapsed := time.Since(start)
	_ = cmd.Wait()
	if strings.Contains(out.String(), "\x1b]11;?") {
		t.Errorf("binary queried the terminal's background color at startup: %q", out.String())
	}
	if !strings.Contains(out.String(), "startup-done") || elapsed > 3*time.Second {
		t.Errorf("startup on an unanswering terminal took %v (output %q)", elapsed, out.String())
	}
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

// echoOn reads the terminal's flags through the master. Darwin revokes the
// slave when its session leader (the helper) exits, so a slave descriptor
// cannot observe what the helper left behind; the master shares the same tty
// state on both kernels (Linux redirects master termios ioctls to the slave,
// XNU's pty master falls through to the shared tty) and stays valid.
func (p *hiddenReadPTY) echoOn() bool {
	tio, err := unix.IoctlGetTermios(int(p.master.Fd()), termiosGet)
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
	// An interrupt never returns to the caller: nothing, least of all the
	// typed text, may be recorded.
	if b, err := os.ReadFile(p.result); err == nil {
		t.Fatalf("an interrupted read returned to its caller: %q", b)
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
			if b, err := os.ReadFile(p.result); err == nil {
				t.Fatalf("%v returned to its caller: %q", sig, b)
			}
		})
	}
}

func TestReadHiddenLineSuspendRestoresThenHidesOnResume(t *testing.T) {
	p := startHiddenRead(t)
	if err := p.cmd.Process.Signal(syscall.SIGTSTP); err != nil {
		t.Fatal(err)
	}
	// readHiddenLine suspends with SIGSTOP. Decode the stop directly:
	// syscall's BSD WaitStatus.Stopped excludes SIGSTOP stops, unlike Linux,
	// while the wait status encoding (0x7f, signal in bits 8-15) is shared.
	ws := p.wait4(syscall.WUNTRACED)
	if raw := uint32(ws); raw&0xff != 0x7f || syscall.Signal(raw>>8&0xff) != syscall.SIGSTOP {
		t.Fatalf("helper status %#x, want stopped by SIGSTOP", raw)
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
