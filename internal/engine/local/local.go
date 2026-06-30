// Package local is the local Engine: it runs each agent instance as a
// PTY-backed process on this machine. Instances are owned by the engine (and
// thus by the daemon that holds it), not by any client connection, so they keep
// running when a UI detaches or quits. Each instance keeps a scrollback ring
// buffer and fans live output out to every attached subscriber, so multiple UIs
// can watch the same agent and a reconnecting one repaints from the buffer.
package local

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/creack/pty"

	"amux/internal/engine"
)

// scrollbackBytes bounds the per-instance replay buffer. It's enough to repaint
// a reattaching client's screen (a full-screen TUI redraw is a few KB); the live
// agent keeps redrawing, so the buffer only needs to bridge the gap until the
// next repaint, not hold full history.
const scrollbackBytes = 256 * 1024

// Engine is the local PTY-backed engine.
type Engine struct {
	mu    sync.Mutex
	insts map[engine.Key]*instance
}

// New creates a local engine.
func New() *Engine { return &Engine{insts: map[engine.Key]*instance{}} }

func (e *Engine) Name() string { return "local" }

// Ensure returns the running instance for spec.Key, spawning it if absent or if
// the previous one has exited (so attaching to a dead pane restarts it).
func (e *Engine) Ensure(_ context.Context, spec engine.Spec) (engine.Instance, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if in, ok := e.insts[spec.Key]; ok && in.Alive() {
		return in, nil
	}
	in, err := spawn(spec, e.remove)
	if err != nil {
		return nil, err
	}
	e.insts[spec.Key] = in
	return in, nil
}

func (e *Engine) Lookup(key engine.Key) (engine.Instance, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	in, ok := e.insts[key]
	return in, ok
}

func (e *Engine) Live() []engine.Key {
	e.mu.Lock()
	defer e.mu.Unlock()
	keys := make([]engine.Key, 0, len(e.insts))
	for k, in := range e.insts {
		if in.Alive() {
			keys = append(keys, k)
		}
	}
	return keys
}

func (e *Engine) Kill(key engine.Key) {
	e.mu.Lock()
	in := e.insts[key]
	e.mu.Unlock()
	if in != nil {
		in.kill()
	}
}

func (e *Engine) Shutdown() {
	e.mu.Lock()
	insts := make([]*instance, 0, len(e.insts))
	for _, in := range e.insts {
		insts = append(insts, in)
	}
	e.insts = map[engine.Key]*instance{}
	e.mu.Unlock()
	for _, in := range insts {
		in.kill()
	}
}

// remove drops an exited instance from the table (called by the instance's pump
// when its process ends), but only if it's still the current one for that key —
// a respawn may already have replaced it.
func (e *Engine) remove(key engine.Key, in *instance) {
	e.mu.Lock()
	if e.insts[key] == in {
		delete(e.insts, key)
	}
	e.mu.Unlock()
}

// ---- instance ----

type subscriber struct {
	sink engine.Sink
}

type instance struct {
	key  engine.Key
	ptmx *os.File
	cmd  *exec.Cmd

	mu         sync.Mutex
	ring       []byte
	subs       map[int]*subscriber
	nextSub    int
	exited     bool
	exitErr    string
	ptmxClosed bool

	onExit func(engine.Key, *instance)
}

func spawn(spec engine.Spec, onExit func(engine.Key, *instance)) (*instance, error) {
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = spec.Dir
	cmd.Env = buildEnv(spec.Env)
	cols, rows := spec.Cols, spec.Rows
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return nil, err
	}
	in := &instance{
		key:    spec.Key,
		ptmx:   ptmx,
		cmd:    cmd,
		subs:   map[int]*subscriber{},
		onExit: onExit,
	}
	go in.pump()
	return in, nil
}

func (in *instance) Key() engine.Key { return in.key }

func (in *instance) Subscribe(sink engine.Sink) func() {
	in.mu.Lock()
	defer in.mu.Unlock()
	// Replay the scrollback first so the subscriber paints the current screen,
	// then register so live output (serialized on in.mu by pump) follows in order.
	if len(in.ring) > 0 && sink.Output != nil {
		sink.Output(append([]byte(nil), in.ring...))
	}
	if in.exited {
		if sink.Exit != nil {
			sink.Exit(in.exitErr)
		}
		return func() {}
	}
	id := in.nextSub
	in.nextSub++
	in.subs[id] = &subscriber{sink: sink}
	return func() {
		in.mu.Lock()
		delete(in.subs, id)
		in.mu.Unlock()
	}
}

// Input and Resize touch the PTY fd, so they take in.mu and bail once the fd is
// closed — otherwise an ioctl/Fd() could race the close in pump/kill.
func (in *instance) Input(p []byte) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.ptmxClosed || in.ptmx == nil {
		return
	}
	_, _ = in.ptmx.Write(p)
}

func (in *instance) Resize(cols, rows int) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.ptmxClosed || in.ptmx == nil || cols <= 0 || rows <= 0 {
		return
	}
	_ = pty.Setsize(in.ptmx, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

// closePtmx closes the PTY at most once. Caller holds in.mu.
func (in *instance) closePtmx() {
	if in.ptmx != nil && !in.ptmxClosed {
		in.ptmxClosed = true
		_ = in.ptmx.Close()
	}
}

func (in *instance) Alive() bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	return !in.exited
}

func (in *instance) pump() {
	buf := make([]byte, 32*1024)
	for {
		n, err := in.ptmx.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			in.mu.Lock()
			in.appendRing(chunk)
			for _, s := range in.subs {
				if s.sink.Output != nil {
					s.sink.Output(chunk)
				}
			}
			in.mu.Unlock()
		}
		if err != nil {
			werr := ""
			if e := in.cmd.Wait(); e != nil {
				werr = e.Error()
			}
			in.mu.Lock()
			in.closePtmx()
			in.exited = true
			in.exitErr = werr
			subs := make([]*subscriber, 0, len(in.subs))
			for _, s := range in.subs {
				subs = append(subs, s)
			}
			in.subs = map[int]*subscriber{}
			in.mu.Unlock()
			for _, s := range subs {
				if s.sink.Exit != nil {
					s.sink.Exit(werr)
				}
			}
			if in.onExit != nil {
				in.onExit(in.key, in)
			}
			return
		}
	}
}

// appendRing appends to the scrollback, trimming the oldest bytes past the cap.
// Caller holds in.mu.
func (in *instance) appendRing(p []byte) {
	in.ring = append(in.ring, p...)
	if over := len(in.ring) - scrollbackBytes; over > 0 {
		in.ring = in.ring[over:]
	}
}

func (in *instance) kill() {
	in.mu.Lock()
	in.closePtmx() // unblocks pump's Read; pump then reaps and notifies
	in.mu.Unlock()
	if in.cmd.Process != nil {
		_ = in.cmd.Process.Kill()
	}
}

// buildEnv is the child's environment: this process's environment minus $TMUX
// and $TERM (so a fresh terminal is presented regardless of where the engine
// runs), then the spec's additions, and a default $TERM if the spec didn't set
// one.
func buildEnv(extra []string) []string {
	env := stripEnv(os.Environ(), "TMUX", "TERM")
	env = append(env, extra...)
	for _, e := range extra {
		if strings.HasPrefix(e, "TERM=") {
			return env
		}
	}
	return append(env, "TERM=xterm-256color")
}

func stripEnv(env []string, keys ...string) []string {
	out := env[:0:0]
	for _, e := range env {
		drop := false
		for _, k := range keys {
			if strings.HasPrefix(e, k+"=") {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, e)
		}
	}
	return out
}
