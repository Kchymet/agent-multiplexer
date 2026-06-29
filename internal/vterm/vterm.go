// Package vterm embeds a child process running in a PTY and renders its screen
// through a VT emulator, so an interactive full-screen program (e.g. a tmux
// client showing an agent) can be drawn inside a Bubble Tea pane. It is the
// foundation of amux's native TUI: tmux still hosts the agents (so they survive
// the TUI closing), and this renders a tmux client into our own chrome.
package vterm

import (
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/charmbracelet/x/vt"
	"github.com/creack/pty"
)

// Terminal runs one child process in a PTY and maintains a rendered screen.
// Its methods are safe to call from multiple goroutines: a background reader
// pumps PTY output into the emulator while the UI goroutine calls Render.
type Terminal struct {
	mu     sync.Mutex
	emu    *vt.Emulator
	ptmx   *os.File
	cmd    *exec.Cmd
	cols   int
	rows   int
	closed bool

	onData func() // fired when new output is parsed, to prompt a re-render
}

// New creates a terminal sized to cols×rows (falling back to 80×24).
func New(cols, rows int) *Terminal {
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	return &Terminal{emu: vt.NewEmulator(cols, rows), cols: cols, rows: rows}
}

// OnData registers a callback fired (on the reader goroutine) whenever new
// output arrives or the child exits — an embedder uses it to trigger a redraw.
func (t *Terminal) OnData(f func()) { t.onData = f }

// Start launches cmd in a PTY sized to the terminal and streams its output into
// the emulator until the child exits.
func (t *Terminal) Start(cmd *exec.Cmd) error {
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(t.cols), Rows: uint16(t.rows)})
	if err != nil {
		return err
	}
	t.cmd = cmd
	t.ptmx = ptmx
	go t.pump()
	go t.forwardResponses()
	return nil
}

// forwardResponses copies the emulator's replies to terminal queries (Device
// Attributes, cursor-position reports, etc.) back to the child via the PTY.
//
// The emulator answers such queries by writing into an internal io.Pipe. That
// pipe is unbuffered, so if nobody drains it the very first query (tmux/Claude
// both send a Primary Device Attributes request on startup) blocks the parser
// inside emu.Write — and pump holds t.mu across emu.Write, so a blocked write
// wedges Render and freezes the whole UI. emu.Read only touches the response
// pipe (never the screen state), so this runs lock-free alongside pump. It ends
// when the emulator or PTY is closed.
func (t *Terminal) forwardResponses() {
	_, _ = io.Copy(t.ptmx, t.emu)
}

func (t *Terminal) pump() {
	buf := make([]byte, 32*1024)
	for {
		n, err := t.ptmx.Read(buf)
		if n > 0 {
			t.mu.Lock()
			_, _ = t.emu.Write(buf[:n])
			t.mu.Unlock()
			t.notify()
		}
		if err != nil {
			t.mu.Lock()
			t.closed = true
			t.mu.Unlock()
			t.notify()
			return
		}
	}
}

func (t *Terminal) notify() {
	if t.onData != nil {
		t.onData()
	}
}

// Render returns the current screen as a styled (ANSI) string.
func (t *Terminal) Render() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.emu.Render()
}

// Write sends input bytes to the child process (e.g. translated keystrokes).
func (t *Terminal) Write(p []byte) (int, error) {
	if t.ptmx == nil {
		return 0, os.ErrClosed
	}
	return t.ptmx.Write(p)
}

// MouseEvent forwards a mouse event (wheel scroll, click, or motion) to the
// child. The emulator encodes it only if the child has enabled mouse reporting —
// exactly like a real terminal: mouse-aware programs (the agent TUI, a pager, an
// editor with mouse on) receive it; others are unaffected. Coordinates are
// 0-based from the terminal's top-left. The encoded bytes flow to the child via
// the same reply pipe forwardResponses already drains. We hold t.mu because
// SendMouse reads emulator mode state that pump mutates under the same lock.
func (t *Terminal) MouseEvent(ev vt.Mouse) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.emu.SendMouse(ev)
}

// Resize resizes both the emulator and the underlying PTY (SIGWINCH).
func (t *Terminal) Resize(cols, rows int) {
	if cols <= 0 || rows <= 0 {
		return
	}
	t.mu.Lock()
	t.cols, t.rows = cols, rows
	t.emu.Resize(cols, rows)
	ptmx := t.ptmx
	t.mu.Unlock()
	if ptmx != nil {
		_ = pty.Setsize(ptmx, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	}
}

// Closed reports whether the child process has exited (its PTY hit EOF).
func (t *Terminal) Closed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed
}

// Close tears down the PTY and kills the child if still running. Closing the
// emulator unblocks the forwardResponses goroutine (emu.Read returns EOF).
func (t *Terminal) Close() error {
	if t.emu != nil {
		_ = t.emu.Close()
	}
	if t.ptmx != nil {
		_ = t.ptmx.Close()
	}
	if t.cmd != nil && t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
	}
	return nil
}
