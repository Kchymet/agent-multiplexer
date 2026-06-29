// Package harness is amux's agent harness: it owns the actual PTY-backed
// processes for agent panes (the Claude agent, an editor, a shell) and speaks the
// Server <-> Harness protocol (harnessproto). The multiplexer server tells it to
// spawn/feed/resize/kill panes; it streams their output back. Running pane
// execution behind this protocol is what lets a server orchestrate agents
// locally, in a jail, or on a different host.
package harness

import (
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/creack/pty"

	"amux/internal/harnessproto"
)

type pane struct {
	ptmx *os.File
	cmd  *exec.Cmd
}

type harness struct {
	conn  *harnessproto.Conn
	mu    sync.Mutex
	panes map[string]*pane
}

// Serve runs the harness loop over conn until it closes: it announces `ready`,
// then handles spawn/input/resize/kill and streams pane output back. Blocks.
func Serve(conn *harnessproto.Conn) error {
	h := &harness{conn: conn, panes: map[string]*pane{}}
	_ = conn.WriteHarness(harnessproto.HarnessMsg{Type: harnessproto.HReady, Version: harnessproto.Version})
	for {
		m, err := conn.ReadMux()
		if err != nil {
			h.closeAll()
			return err
		}
		h.handle(m)
	}
}

func (h *harness) handle(m harnessproto.MuxMsg) {
	switch m.Type {
	case harnessproto.MSpawn:
		h.spawn(m)
	case harnessproto.MInput:
		if p := h.get(m.PaneID); p != nil {
			_, _ = p.ptmx.Write(m.Data)
		}
	case harnessproto.MResize:
		if p := h.get(m.PaneID); p != nil && m.Cols > 0 && m.Rows > 0 {
			_ = pty.Setsize(p.ptmx, &pty.Winsize{Cols: uint16(m.Cols), Rows: uint16(m.Rows)})
		}
	case harnessproto.MKill:
		h.kill(m.PaneID)
	}
}

func (h *harness) spawn(m harnessproto.MuxMsg) {
	if len(m.Argv) == 0 {
		h.sendExit(m.PaneID, "empty argv")
		return
	}
	cmd := exec.Command(m.Argv[0], m.Argv[1:]...)
	cmd.Dir = m.Dir
	// The harness supplies the local execution environment (PATH etc.); the mux
	// supplies the agent-specific vars. $TMUX/$TERM are reset so a fresh terminal
	// is presented regardless of where the harness runs.
	cmd.Env = append(stripEnv(os.Environ(), "TMUX", "TERM"), m.Env...)
	cols, rows := m.Cols, m.Rows
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		h.sendExit(m.PaneID, err.Error())
		return
	}
	p := &pane{ptmx: ptmx, cmd: cmd}
	h.mu.Lock()
	h.panes[m.PaneID] = p
	h.mu.Unlock()
	go h.pump(m.PaneID, p)
}

// pump streams a pane's PTY output back as `output` frames until EOF, then waits
// the process and reports `exit`.
func (h *harness) pump(id string, p *pane) {
	buf := make([]byte, 32*1024)
	for {
		n, err := p.ptmx.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			_ = h.conn.WriteHarness(harnessproto.HarnessMsg{Type: harnessproto.HOutput, PaneID: id, Data: data})
		}
		if err != nil {
			werr := ""
			if e := p.cmd.Wait(); e != nil {
				werr = e.Error()
			}
			_ = p.ptmx.Close()
			h.mu.Lock()
			delete(h.panes, id)
			h.mu.Unlock()
			h.sendExit(id, werr)
			return
		}
	}
}

func (h *harness) get(id string) *pane {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.panes[id]
}

func (h *harness) kill(id string) {
	h.mu.Lock()
	p := h.panes[id]
	delete(h.panes, id)
	h.mu.Unlock()
	if p != nil {
		_ = p.ptmx.Close()
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
	}
}

func (h *harness) closeAll() {
	h.mu.Lock()
	ps := h.panes
	h.panes = map[string]*pane{}
	h.mu.Unlock()
	for _, p := range ps {
		_ = p.ptmx.Close()
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
	}
}

func (h *harness) sendExit(id, errMsg string) {
	_ = h.conn.WriteHarness(harnessproto.HarnessMsg{Type: harnessproto.HExit, PaneID: id, Error: errMsg})
}

// stripEnv removes any KEY=... entries for the given keys.
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
