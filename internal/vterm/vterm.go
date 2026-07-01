// Package vterm renders a terminal screen through a VT emulator so an
// interactive full-screen program (an agent, an editor, a shell) can be drawn
// inside a Bubble Tea pane. The byte transport is pluggable: Start runs a local
// child in a PTY, while NewRemote drives the emulator from a stream the embedder
// feeds (Feed) and routes input back through callbacks. The native TUI uses the
// remote mode so the agent process lives in the daemon's engine — surviving the
// TUI closing — and only its screen is mirrored here.
package vterm

import (
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
	"github.com/creack/pty"
)

// altScrollLines is how many cursor-key presses one wheel notch turns into when
// falling back to alternate-scroll (see MouseEvent). Three lines per notch
// matches what xterm and most terminals send.
const altScrollLines = 3

// mouseTrackModes are the DEC private modes a child sets to ask for mouse
// reporting. If any is on, the child wants raw mouse events and we forward them
// verbatim; if none is, wheel scrolling falls back to alternate-scroll keys.
var mouseTrackModes = []ansi.Mode{
	ansi.ModeMouseX10,
	ansi.ModeMouseNormal,
	ansi.ModeMouseHighlight,
	ansi.ModeMouseButtonEvent,
	ansi.ModeMouseAnyEvent,
}

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

	// Mouse/cursor mode state the child has requested, tracked so MouseEvent can
	// tell whether to forward raw mouse events or fall back to alternate-scroll.
	// Mutated only from the emulator's mode callbacks, which fire synchronously
	// inside emu.Write while pump/Feed hold t.mu — so they're guarded by the same
	// lock without re-locking (the mutex isn't reentrant).
	mouseModes    map[ansi.Mode]bool // mouse-tracking DEC modes currently enabled
	appCursorKeys bool               // DECCKM: cursor keys use the SS3 (application) form

	// Remote mode (set by NewRemote): there is no local PTY; input and the
	// emulator's query replies are routed through onInput, and resizes are
	// propagated through onResize.
	onInput  func([]byte)
	onResize func(cols, rows int)
}

// New creates a terminal sized to cols×rows (falling back to 80×24).
func New(cols, rows int) *Terminal {
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	t := &Terminal{
		emu:        vt.NewEmulator(cols, rows),
		cols:       cols,
		rows:       rows,
		mouseModes: make(map[ansi.Mode]bool),
	}
	// Watch the child's private-mode changes so MouseEvent knows whether it wants
	// raw mouse events (any mouse-tracking mode) and which cursor-key form it
	// expects (DECCKM). These callbacks run under t.mu (see field docs).
	t.emu.SetCallbacks(vt.Callbacks{
		EnableMode:  func(m ansi.Mode) { t.noteMode(m, true) },
		DisableMode: func(m ansi.Mode) { t.noteMode(m, false) },
	})
	return t
}

// noteMode records a child mode change relevant to input translation. It runs
// synchronously from emu.Write (via the emulator's mode callbacks), which
// pump/Feed already hold t.mu across — so it must not re-lock.
func (t *Terminal) noteMode(m ansi.Mode, on bool) {
	switch m {
	case ansi.ModeMouseX10, ansi.ModeMouseNormal, ansi.ModeMouseHighlight,
		ansi.ModeMouseButtonEvent, ansi.ModeMouseAnyEvent:
		t.mouseModes[m] = on
	case ansi.ModeCursorKeys:
		t.appCursorKeys = on
	}
}

// mouseTracked reports whether the child has any mouse-reporting mode enabled,
// meaning it wants raw mouse events forwarded rather than alternate-scroll keys.
func (t *Terminal) mouseTracked() bool {
	for _, m := range mouseTrackModes {
		if t.mouseModes[m] {
			return true
		}
	}
	return false
}

// NewRemote creates a terminal with no local process: output is supplied by the
// embedder via Feed, input (keystrokes, mouse encodings, and the emulator's own
// query replies) is delivered to onInput, and resizes are reported to onResize.
// This is how the native TUI mirrors a pane the daemon's engine actually hosts.
func NewRemote(cols, rows int, onInput func([]byte), onResize func(cols, rows int)) *Terminal {
	t := New(cols, rows)
	t.onInput = onInput
	t.onResize = onResize
	go t.drainResponses() // emulator replies (DA, cursor reports, mouse) -> agent
	return t
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

// Feed writes output bytes into the emulator (remote mode): the embedder calls
// it with bytes streamed from the daemon, just as pump does for a local PTY.
func (t *Terminal) Feed(p []byte) {
	t.mu.Lock()
	_, _ = t.emu.Write(p)
	t.mu.Unlock()
	t.notify()
}

// MarkClosed records that the remote process has exited, so Closed reports true
// and the embedder can reap the pane.
func (t *Terminal) MarkClosed() {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	t.notify()
}

// drainResponses copies the emulator's replies to terminal queries to onInput
// (remote mode), the equivalent of forwardResponses for a local PTY. It ends
// when the emulator is closed.
func (t *Terminal) drainResponses() {
	buf := make([]byte, 4096)
	for {
		n, err := t.emu.Read(buf)
		if n > 0 && t.onInput != nil {
			t.onInput(append([]byte(nil), buf[:n]...))
		}
		if err != nil {
			return
		}
	}
}

// Render returns the current screen as a styled (ANSI) string.
func (t *Terminal) Render() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.emu.Render()
}

// Write sends input bytes to the agent (e.g. translated keystrokes): to onInput
// in remote mode, or the local PTY otherwise.
func (t *Terminal) Write(p []byte) (int, error) {
	if t.onInput != nil {
		t.onInput(p)
		return len(p), nil
	}
	if t.ptmx == nil {
		return 0, os.ErrClosed
	}
	return t.ptmx.Write(p)
}

// MouseEvent forwards a mouse event (wheel scroll, click, or motion) to the
// child. If the child has enabled mouse reporting, the emulator encodes the raw
// event — exactly like a real terminal: mouse-aware programs (the agent TUI, a
// pager, an editor with mouse on) receive it. The encoded bytes flow to the
// child via the same reply pipe forwardResponses already drains.
//
// Otherwise, a vertical wheel over the alternate screen falls back to
// alternate-scroll: like xterm's DECSET 1007 (on by default), it translates the
// notch into cursor-key presses so full-screen programs that don't track the
// mouse — less, man, vim, or tmux without `mouse on` — still scroll. Without
// this the emulator silently drops the wheel and nothing scrolls.
//
// Coordinates are 0-based from the terminal's top-left. We hold t.mu because
// SendMouse and the mode state both read fields pump mutates under the lock; the
// synthesized keys are written after unlocking, on the normal input path.
func (t *Terminal) MouseEvent(ev vt.Mouse) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	var keys []byte
	if wheel, ok := ev.(vt.MouseWheel); ok && !t.mouseTracked() && t.emu.IsAltScreen() {
		keys = altScrollKeys(wheel.Button, t.appCursorKeys)
	} else {
		t.emu.SendMouse(ev)
	}
	t.mu.Unlock()
	if keys != nil {
		_, _ = t.Write(keys)
	}
}

// altScrollKeys returns the cursor-key bytes a vertical wheel notch maps to
// under alternate-scroll, repeated altScrollLines times. app selects the SS3
// (application cursor keys / DECCKM) form over the default CSI form. Horizontal
// wheels have no standard mapping and return nil.
func altScrollKeys(btn vt.MouseButton, app bool) []byte {
	var arrow byte
	switch btn {
	case vt.MouseWheelUp:
		arrow = 'A'
	case vt.MouseWheelDown:
		arrow = 'B'
	default:
		return nil
	}
	intro := byte('[')
	if app {
		intro = 'O'
	}
	seq := make([]byte, 0, altScrollLines*3)
	for i := 0; i < altScrollLines; i++ {
		seq = append(seq, 0x1b, intro, arrow)
	}
	return seq
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
	if t.onResize != nil {
		t.onResize(cols, rows)
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
