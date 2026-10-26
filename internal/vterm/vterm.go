// Package vterm renders a terminal screen through a VT emulator so an
// interactive full-screen program (an agent, an editor, a shell) can be drawn
// inside a Bubble Tea pane. The byte transport is pluggable: Start runs a local
// child in a PTY, while NewRemote drives the emulator from a stream the embedder
// feeds (Feed) and routes input back through callbacks. The native TUI uses the
// remote mode so the agent process lives in the daemon's engine — surviving the
// TUI closing — and only its screen is mirrored here.
//
// A Terminal also keeps a bounded pane-local history of the primary screen and
// a viewport that can scroll back into it; see history.go for the policy and
// the ScrollLines/Snapshot family of methods.
package vterm

import (
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
	"github.com/creack/pty"

	"amux/internal/clipboard"
)

// mouseTrackModes are the DEC private modes a child sets to ask for mouse
// reporting. If any is on, the child wants raw mouse events and we forward them
// verbatim; if none is, wheel scrolling stays in the pane-local history.
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

	// onClipboard receives the text of each OSC 52 clipboard write the child
	// makes (see OnClipboard). Read from the emulator's OSC handler, under t.mu.
	onClipboard func([]byte)

	// Mouse/cursor mode state the child has requested, tracked so MouseEvent can
	// tell whether to forward raw mouse events or handle them locally.
	// Mutated only from the emulator's mode callbacks, which fire synchronously
	// inside emu.Write while pump/Feed hold t.mu — so they're guarded by the same
	// lock without re-locking (the mutex isn't reentrant).
	mouseModes map[ansi.Mode]bool // mouse-tracking DEC modes currently enabled

	// Remote mode (set by NewRemote): there is no local PTY; input and the
	// emulator's query replies are routed through onInput, and resizes are
	// propagated through onResize.
	onInput  func([]byte)
	onResize func(cols, rows int)

	// Streamed output (remote mode) is coalesced into fbuf and parsed into the
	// emulator by a dedicated goroutine, so the caller of Feed — the Bubble Tea
	// Update goroutine — never blocks inside emu.Write and no bytes are dropped.
	// Terminal output is a stateful byte stream: drop any of it (e.g. an erase
	// sequence) and the emulator keeps stale cells that ghost through as "text
	// shadowing". The emulator can stall mid-write when it answers a query (it
	// writes the reply into a pipe drainResponses drains); keeping emu.Write off
	// the UI goroutine means that backpressure can't wedge render or input. fwake
	// nudges the goroutine; done stops it on Close.
	fmu   sync.Mutex
	fbuf  []byte
	fwake chan struct{}
	done  chan struct{}
	once  sync.Once

	// scrub sanitizes string-sequence data before emu.Write (see scrubber). Its
	// state spans chunk boundaries; pump and feedLoop both mutate it under t.mu.
	scrub scrubber

	// Pane-local history and viewport (history.go). offset is how many rows
	// the viewport is scrolled back into the active screen's history (0 =
	// live). alt is the alternate screen's completed-frame history; the
	// primary screen's history is the emulator's scrollback. rev counts every
	// change to what Render/Snapshot would show.
	offset int
	alt    frameHistory
	rev    uint64
}

// feedCap bounds the coalesced backlog before the client, hopelessly behind,
// resyncs rather than growing without bound: it keeps only the recent tail and
// resets the emulator first (a full-screen agent repaints within a frame, so the
// brief partial beats corruption). This fills only if emu.Write stalls
// persistently, which the response-pipe drain is designed to prevent.
const (
	feedCap  = 4 << 20
	feedKeep = 256 << 10
)

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
	// Replayed output may have been produced in a taller viewport. The pinned
	// emulator accepts an out-of-range bottom margin and later panics in scroll
	// operations. Consume those invalid bounds before its default handler; valid
	// margins still use the normal implementation.
	t.emu.RegisterCsiHandler('r', func(params ansi.Params) bool {
		top, _, _ := params.Param(0, 1)
		bottom, _, _ := params.Param(1, t.emu.Height())
		return top > t.emu.Height() || bottom > t.emu.Height()
	})
	// Watch the child's private-mode changes so MouseEvent knows whether it wants
	// raw mouse events (any mouse-tracking mode) instead of local history.
	// These callbacks run under t.mu (see field docs).
	t.emu.SetCallbacks(vt.Callbacks{
		EnableMode:  func(m ansi.Mode) { t.noteMode(m, true) },
		DisableMode: func(m ansi.Mode) { t.noteMode(m, false) },
	})
	// The emulator has no OSC 52 handler and silently drops the sequence, which
	// is how agents copy a mouse selection; hand writes to the embedder, which
	// owns the real terminal. Queries are swallowed unanswered: replying would
	// hand the user's clipboard to the child.
	t.emu.RegisterOscHandler(52, func(data []byte) bool {
		if text, ok := clipboard.ParseOSC52(data); ok && t.onClipboard != nil {
			t.onClipboard(text)
		}
		return true
	})
	t.installHistoryHooks()
	return t
}

// OnClipboard registers f to receive the text of every OSC 52 clipboard write
// the child emits. f runs on the parsing goroutine with t.mu held: it must not
// block or call back into t, and must copy the text to keep it.
func (t *Terminal) OnClipboard(f func(text []byte)) {
	t.mu.Lock()
	t.onClipboard = f
	t.mu.Unlock()
}

// noteMode records a child mode change relevant to input translation. It runs
// synchronously from emu.Write (via the emulator's mode callbacks), which
// pump/Feed already hold t.mu across — so it must not re-lock.
func (t *Terminal) noteMode(m ansi.Mode, on bool) {
	switch m {
	case ansi.ModeMouseX10, ansi.ModeMouseNormal, ansi.ModeMouseHighlight,
		ansi.ModeMouseButtonEvent, ansi.ModeMouseAnyEvent:
		t.mouseModes[m] = on
	}
}

// MouseTracked reports whether the child has any mouse-reporting mode enabled,
// meaning it wants raw mouse events forwarded (MouseEvent does so) rather than
// having the wheel scroll the pane-local history.
func (t *Terminal) MouseTracked() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.mouseTracked()
}

// mouseTracked is MouseTracked for callers already holding t.mu.
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
	t.fwake = make(chan struct{}, 1)
	t.done = make(chan struct{})
	go t.feedLoop()       // streamed output -> emulator, off the UI goroutine
	go t.drainResponses() // emulator replies (DA, cursor reports, mouse) -> agent
	return t
}

// feedLoop parses streamed output into the emulator on its own goroutine, so a
// query reply that momentarily blocks emu.Write can never stall Feed's caller. It
// drains the whole coalesced backlog per wake, in order.
func (t *Terminal) feedLoop() {
	for {
		select {
		case <-t.done:
			return
		case <-t.fwake:
			for {
				t.fmu.Lock()
				b := t.fbuf
				t.fbuf = nil
				t.fmu.Unlock()
				if len(b) == 0 {
					break
				}
				t.ingest(b)
				t.notify()
			}
		}
	}
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
			t.ingest(buf[:n])
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

// ingest is the one path output takes into the emulator, for a local PTY (pump)
// and a remote stream (feedLoop) alike: scrub, parse, then let the history and
// viewport bookkeeping observe the result. It holds t.mu throughout so Render,
// Snapshot and the scroll methods never see a half-applied chunk.
func (t *Terminal) ingest(b []byte) {
	t.mu.Lock()
	t.scrub.scrub(b)
	_, _ = t.emu.Write(b)
	t.afterWrite(len(b))
	t.mu.Unlock()
}

// Feed hands output bytes to the feed goroutine, which writes them into the
// emulator (remote mode): the embedder calls it with bytes streamed from the
// daemon, just as pump does for a local PTY. It never blocks the caller and never
// drops — bytes are appended to the coalesced backlog in order. Only a backlog
// past feedCap (a client hopelessly behind) is trimmed to its recent tail, with a
// RIS prepended so the emulator clears before applying the retained repaint.
func (t *Terminal) Feed(p []byte) {
	t.fmu.Lock()
	t.fbuf = append(t.fbuf, p...)
	if len(t.fbuf) > feedCap {
		tail := t.fbuf[len(t.fbuf)-feedKeep:]
		kept := make([]byte, 0, feedKeep+2)
		kept = append(kept, 0x1b, 'c') // RIS: reset before the retained tail
		t.fbuf = append(kept, tail...)
	}
	t.fmu.Unlock()
	select {
	case t.fwake <- struct{}{}:
	default:
	}
}

// Reset clears the emulator to a blank screen via RIS (ESC c), discarding all
// current cell state, modes, and the pane-local history (see the RIS hook in
// history.go: the replay that follows is a tail of output the history already
// absorbed, so keeping it would duplicate every line it scrolls). The daemon
// sends a pane.reset before replaying a fresh repaint when a client fell too far
// behind to stream output losslessly; without the clear, cells the repaint
// doesn't overwrite would ghost (stale text showing through). It routes through
// the feed queue so it stays ordered with the output that follows.
func (t *Terminal) Reset() { t.Feed([]byte("\x1bc")) }

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

// Render returns the current screen as a styled (ANSI) string, normalized to an
// exact cols×rows block: every line is space-padded to the full width and there
// are exactly rows lines.
//
// The emulator's own Render right-trims trailing blank cells (and renders an
// all-blank line as ""), so raw frames have a ragged right edge. When those are
// composited (lipgloss.JoinHorizontal pads only to a block's widest line, not
// the pane width) and diffed by Bubble Tea's line renderer (which erases to end
// of line only when a line is shorter than the whole terminal, and skips lines
// unchanged since the previous frame), cells the new frame doesn't cover keep
// stale content — an earlier frame's text, or the terminal's pre-amux
// scrollback. Emitting a solid rectangle makes every frame fully overwrite its
// pane, which is what a real terminal does.
//
// While the viewport is scrolled back (ScrollLines), the block shows history
// rows above the live screen's top rows, cell for cell what Snapshot returns;
// the scroll indicator is not drawn into it (Indicator). See history.go.
func (t *Terminal) Render() string {
	t.mu.Lock()
	cols, rows := t.cols, t.rows
	var raw string
	if t.offset > 0 {
		raw = t.renderScrolled()
	} else {
		raw = t.emu.Render()
	}
	t.mu.Unlock()
	return pad(raw, cols, rows)
}

// pad forces s into an exact cols×rows block. It relies on the emulator having
// already reset any pen style before a line's trailing blanks, so the spaces we
// append render on the default background.
func pad(s string, cols, rows int) string {
	if cols <= 0 || rows <= 0 {
		return s
	}
	lines := strings.Split(s, "\n")
	var b strings.Builder
	for y := 0; y < rows; y++ {
		if y > 0 {
			b.WriteByte('\n')
		}
		line := ""
		if y < len(lines) {
			line = lines[y]
		}
		w := ansi.StringWidth(line)
		if w > cols {
			line = ansi.Truncate(line, cols, "")
			w = ansi.StringWidth(line)
		}
		b.WriteString(line)
		if w < cols {
			b.WriteString(strings.Repeat(" ", cols-w))
		}
	}
	return b.String()
}

// Write sends input bytes to the agent (e.g. translated keystrokes): to onInput
// in remote mode, or the local PTY otherwise. Input returns a scrolled-back
// viewport to the live screen first (ReturnLive), the way typing into a
// terminal jumps its scrollback to the bottom; the emulator's own query replies
// bypass Write and never do.
func (t *Terminal) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	t.ReturnLive()
	if t.onInput != nil {
		t.onInput(p)
		return len(p), nil
	}
	if t.ptmx == nil {
		return 0, os.ErrClosed
	}
	return t.ptmx.Write(p)
}

// MouseEvent routes pane-local mouse events atomically with the child's mode
// state. Shift always forces local handling. Otherwise reporting-enabled
// children receive the original event; a vertical wheel without reporting
// scrolls pane history. At history edges (including empty history) it remains
// local and never synthesizes child keys. Non-wheel local events are reserved
// for the embedder's selection handling.
func (t *Terminal) MouseEvent(ev vt.Mouse) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	local := ev.Mouse().Mod.Contains(vt.ModShift) || !t.mouseTracked()
	if local {
		if wheel, ok := ev.(vt.MouseWheel); ok {
			switch wheel.Button {
			case vt.MouseWheelUp:
				t.scrollTo(t.offset + 3)
			case vt.MouseWheelDown:
				t.scrollTo(t.offset - 3)
			}
		}
		return
	}
	t.scrollTo(0)
	t.emu.SendMouse(ev)
}

// Resize resizes both the emulator and the underlying PTY (SIGWINCH). The
// viewport returns to live: the child is about to repaint for the new size, and
// history rows keep the width they were captured at (see history.go).
func (t *Terminal) Resize(cols, rows int) {
	if cols <= 0 || rows <= 0 {
		return
	}
	t.mu.Lock()
	t.cols, t.rows = cols, rows
	t.emu.Resize(cols, rows)
	t.offset = 0
	t.rev++
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
// emulator unblocks the forwardResponses goroutine (emu.Read returns EOF); the
// done signal stops the remote-mode feed goroutine.
func (t *Terminal) Close() error {
	if t.done != nil {
		t.once.Do(func() { close(t.done) })
	}
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
