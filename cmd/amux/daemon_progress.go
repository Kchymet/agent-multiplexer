package main

import (
	"fmt"
	"io"
	"os"
	"time"

	"amux/internal/daemonstatus"
)

// lifecycleProgress renders what a stopping or starting daemon is waiting on,
// read from the status file the daemon publishes (see package daemonstatus).
// On a terminal it keeps one line updated in place; otherwise it prints a
// plain line only when the daemon's state changes, so logs stay readable.
type lifecycleProgress struct {
	out    io.Writer
	tty    bool
	width  func() int
	label  string // "daemon stopping"
	op     string // the daemonstatus op this wait expects
	start  time.Time
	budget time.Duration
	// quiet is how long to wait before the first output, so a fast stop or
	// start prints nothing but its result.
	quiet time.Duration
	// read loads the status file; alive filters a file left by a dead daemon.
	read  func() (daemonstatus.Status, bool)
	alive func(pid int) bool

	drawn     bool // a TTY line is on screen and must be cleared
	lastKey   string
	lastLine  string
	lastPlain time.Time
}

// plainHeartbeat bounds how often a non-terminal repeats a line whose state is
// unchanged but whose detail (a mailbox counter) has moved.
const plainHeartbeat = 10 * time.Second

func newLifecycleProgress(label, op string, budget time.Duration) *lifecycleProgress {
	tty := isTerminal(os.Stdout)
	quiet := 2 * time.Second
	if tty {
		quiet = 500 * time.Millisecond
	}
	return &lifecycleProgress{
		out:    os.Stdout,
		tty:    tty,
		width:  func() int { return terminalWidth(os.Stdout) },
		label:  label,
		op:     op,
		start:  time.Now(),
		budget: budget,
		quiet:  quiet,
		read:   func() (daemonstatus.Status, bool) { return daemonstatus.Read(daemonstatus.Path()) },
		alive:  processAlive,
	}
}

// status returns the daemon's published state when it belongs to a live
// daemon performing this wait's operation.
func (p *lifecycleProgress) status() (daemonstatus.Status, bool) {
	s, ok := p.read()
	if !ok || s.Op != p.op || !p.alive(s.PID) {
		return daemonstatus.Status{}, false
	}
	return s, true
}

// line renders the current progress line at now.
func (p *lifecycleProgress) line(s daemonstatus.Status, ok bool, now time.Time) string {
	elapsed := now.Sub(p.start)
	left := p.budget - elapsed
	if left < 0 {
		left = 0
	}
	head := fmt.Sprintf("%s [%s elapsed, %s left]", p.label, elapsed.Round(time.Second), left.Round(time.Second))
	if !ok {
		if p.op == daemonstatus.OpStopping {
			return head + ": waiting for the daemon to exit"
		}
		return head + ": waiting for the daemon to accept connections"
	}
	return head + ": " + daemonstatus.Summary(s, now)
}

// update reads the status file and redraws. Call it on every poll.
func (p *lifecycleProgress) update(now time.Time) {
	if p == nil || now.Sub(p.start) < p.quiet {
		return
	}
	s, ok := p.status()
	line := p.line(s, ok, now)
	if p.tty {
		if w := p.width(); w > 1 && len([]rune(line)) >= w {
			line = string([]rune(line)[:w-2]) + "…"
		}
		fmt.Fprint(p.out, "\r\x1b[K"+line)
		p.drawn = true
		return
	}
	key := "none"
	if ok {
		key = daemonstatus.Key(s)
	}
	detail := ""
	if ok {
		detail = daemonstatus.Summary(s, time.UnixMilli(s.Updated))
	}
	if key == p.lastKey && (detail == p.lastLine || now.Sub(p.lastPlain) < plainHeartbeat) {
		return
	}
	p.lastKey, p.lastLine, p.lastPlain = key, detail, now
	fmt.Fprintln(p.out, line)
}

// finish clears an in-place line so the caller's result prints cleanly.
func (p *lifecycleProgress) finish() {
	if p != nil && p.drawn {
		fmt.Fprint(p.out, "\r\x1b[K")
		p.drawn = false
	}
}
