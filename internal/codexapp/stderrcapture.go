package codexapp

import (
	"strings"
	"sync"
)

// stderrcapture.go keeps a bounded, concurrency-safe tail of the supervised App
// Server's stderr so a startup failure (a sandbox execvp ENOENT, a bwrap mount
// error) surfaces in the launch/dial/handshake error instead of only a generic
// "cx.sock not found" dial timeout. Without it, os/exec discards the child's stderr
// (cmd.Stderr == nil ⇒ /dev/null) and the real cause is invisible — ROOT had to
// build a throwaway cmd.Stderr=os.Stderr binary to read the bwrap error.
//
// The child's stderr is copied here by os/exec's own copier goroutine (Stderr is an
// arbitrary io.Writer), so Write is called concurrently with tail() — hence the
// mutex. The ring holds only the last maxStderrCapture bytes: runtime stderr never
// grows memory without bound, and normal streaming/control (over the WS protocol) is
// untouched.

// maxStderrCapture bounds the retained stderr tail. Large enough for a multi-line
// bwrap/execvp diagnostic, small enough to never matter for memory.
const maxStderrCapture = 8 << 10 // 8 KiB

// stderrRing is an io.Writer that retains only the last maxStderrCapture bytes
// written to it. Safe for concurrent Write/tail.
type stderrRing struct {
	mu  sync.Mutex
	buf []byte // len is always <= max
	max int
}

func newStderrRing(max int) *stderrRing {
	return &stderrRing{buf: make([]byte, 0, max), max: max}
}

// Write appends p, dropping the oldest bytes so the retained content never exceeds
// max. It always reports the full len(p) as written (a diagnostics sink must never
// stall or fail the copier that feeds it).
func (r *stderrRing) Write(p []byte) (int, error) {
	n := len(p)
	r.mu.Lock()
	defer r.mu.Unlock()
	// Only the last max bytes of a single oversized write can survive.
	if len(p) > r.max {
		p = p[len(p)-r.max:]
	}
	if len(r.buf)+len(p) > r.max {
		drop := len(r.buf) + len(p) - r.max
		r.buf = r.buf[drop:]
	}
	r.buf = append(r.buf, p...)
	return n, nil
}

// tail returns the retained stderr with surrounding whitespace trimmed, or "" when
// nothing was captured.
func (r *stderrRing) tail() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.TrimSpace(string(r.buf))
}
