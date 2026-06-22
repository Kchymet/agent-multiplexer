// Package nativetui is amux's native Bubble Tea front-end: a sidebar switcher
// (workspaces / repos / detached) on the left and the selected agent embedded
// on the right. tmux still hosts the agents (so they survive the TUI closing) —
// this is a custom client that renders one agent via an embedded vterm and adds
// our own chrome + keybindings instead of tmux's panes/windows.
package nativetui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"amux/internal/core"
	"amux/internal/daemon"
	"amux/internal/tmuxctl"
	"amux/internal/vterm"
)

const (
	sidebarWidth = 26
	// viewSession is a private, grouped session the TUI attaches its embedded
	// client to. Grouped with main, it shares the agent windows but keeps its
	// own current window, size, and (session-scoped) status bar — so nothing the
	// TUI does leaks into the user's real `main` client.
	viewSession = "amux-view"
)

// Run starts the native TUI and blocks until the user quits (which detaches —
// agents keep running in the tmux server). It refuses to run inside the amux
// tmux server, where embedding a tmux client would nest and hang.
func Run() error {
	if sock, inside := insideAmux(); inside {
		return fmt.Errorf("can't run the native TUI from inside amux (TMUX=%s) — detach first (Alt-q / C-a d), then run `amux` from your normal shell", sock)
	}
	defer killView()
	m := &model{dataCh: make(chan struct{}, 1), status: "connecting…"}
	_, err := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion()).Run()
	if m.term != nil {
		_ = m.term.Close()
	}
	return err
}

// insideAmux reports whether we're running inside the isolated amux tmux server
// (so we must not embed a nested client). $TMUX is "<socket>,<pid>,<session>".
func insideAmux() (string, bool) {
	t := os.Getenv("TMUX")
	if t == "" {
		return "", false
	}
	sock := t
	if i := strings.IndexByte(t, ','); i >= 0 {
		sock = t[:i]
	}
	return sock, sock == core.TmuxSocket || strings.HasSuffix(sock, "/"+core.TmuxSocket)
}

type model struct {
	client   *daemon.Client
	sessions []core.Session
	cursor   int
	focus    focus // sidebar or agent pane
	term     *vterm.Terminal
	attached string // session id currently embedded
	dataCh   chan struct{}
	w, h     int
	status   string
}

type focus int

const (
	focusSidebar focus = iota
	focusAgent
)

// ---- messages ----
type connectedMsg struct{ c *daemon.Client }
type frameMsg struct{ f daemon.Frame }
type disconnectedMsg struct{}
type termDataMsg struct{}

func (m *model) Init() tea.Cmd { return tea.Batch(connectCmd, waitData(m.dataCh)) }

func connectCmd() tea.Msg {
	c, err := daemon.Dial()
	if err != nil {
		return disconnectedMsg{}
	}
	return connectedMsg{c}
}

func readCmd(c *daemon.Client) tea.Cmd {
	return func() tea.Msg {
		f, err := c.Next()
		if err != nil {
			return disconnectedMsg{}
		}
		return frameMsg{f}
	}
}

func waitData(ch chan struct{}) tea.Cmd {
	return func() tea.Msg { <-ch; return termDataMsg{} }
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		if m.term != nil {
			m.term.Resize(m.mainWidth(), m.paneRows())
		}
		return m, nil

	case connectedMsg:
		m.client = msg.c
		m.status = ""
		return m, readCmd(msg.c)

	case disconnectedMsg:
		m.client = nil
		m.status = "daemon offline — reconnecting…"
		return m, tea.Tick(time.Second, func(time.Time) tea.Msg { return connectCmd() })

	case frameMsg:
		if s := msg.f.Snapshot; s != nil {
			m.sessions = s.Sessions
			if m.cursor >= len(m.sessions) {
				m.cursor = len(m.sessions) - 1
			}
			if m.cursor < 0 {
				m.cursor = 0
			}
		}
		if m.client != nil {
			return m, readCmd(m.client)
		}
		return m, nil

	case termDataMsg:
		if m.term != nil && m.term.Closed() {
			m.term = nil
			m.attached = ""
			m.focus = focusSidebar
		}
		return m, waitData(m.dataCh)

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *model) handleKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	// When the agent pane is focused, forward keys to it — except the escape
	// hatch that returns focus to the sidebar.
	if m.focus == focusAgent && m.term != nil {
		if k.Type == tea.KeyCtrlA { // prefix: pop back to the switcher
			m.focus = focusSidebar
			return m, nil
		}
		if b := keyToBytes(k); len(b) > 0 {
			_, _ = m.term.Write(b)
		}
		return m, nil
	}

	switch k.String() {
	case "q", "ctrl+c", "alt+q":
		return m, tea.Quit
	case "up", "k":
		m.move(-1)
	case "down", "j":
		m.move(1)
	case "enter", "l", "right":
		return m, m.attachSelected()
	case "tab":
		if m.term != nil {
			m.focus = focusAgent
		}
	}
	return m, nil
}

func (m *model) move(d int) {
	n := len(m.sessions)
	if n == 0 {
		return
	}
	m.cursor = (m.cursor + d + n) % n
}

func (m *model) selected() *core.Session {
	if m.cursor < 0 || m.cursor >= len(m.sessions) {
		return nil
	}
	return &m.sessions[m.cursor]
}

// attachSelected points the embedded tmux client at the selected agent's window
// (creating the embedded client on first use) and focuses the agent pane.
func (m *model) attachSelected() tea.Cmd {
	s := m.selected()
	if s == nil || s.WindowID == "" {
		m.status = "not a running agent"
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if m.term == nil {
		// Create the isolated, grouped view session and embed a client to it.
		_, _ = tmuxctl.Run(ctx, "kill-session", "-t", viewSession) // clear any stale one
		if _, err := tmuxctl.Run(ctx, "new-session", "-d", "-s", viewSession, "-t", core.SessionName); err != nil {
			m.status = "view session failed: " + err.Error()
			return nil
		}
		_, _ = tmuxctl.Run(ctx, "set", "-t", viewSession, "status", "off")
		_, _ = tmuxctl.Run(ctx, "set", "-t", viewSession, "aggressive-resize", "on")

		t := vterm.New(m.mainWidth(), m.paneRows())
		t.OnData(func() {
			select {
			case m.dataCh <- struct{}{}:
			default:
			}
		})
		cmd := exec.Command("tmux", "-L", core.TmuxSocket, "attach", "-t", viewSession)
		cmd.Env = append(envWithout(os.Environ(), "TMUX"), "TERM=xterm-256color")
		if err := t.Start(cmd); err != nil {
			m.status = "attach failed: " + err.Error()
			return nil
		}
		m.term = t
	}

	// Point the view session at this agent's window (isolated: doesn't move the
	// user's real `main` client) and focus the agent's work pane.
	_, _ = tmuxctl.Run(ctx, "select-window", "-t", viewSession+":"+s.WindowID)
	if pane := tmuxctl.WorkPane(ctx, s.WindowID); pane != "" {
		_, _ = tmuxctl.Run(ctx, "select-pane", "-t", pane)
	}
	m.attached = s.ID
	m.focus = focusAgent
	return nil
}

func (m *model) mainWidth() int {
	w := m.w - sidebarWidth - 1
	if w < 1 {
		return 1
	}
	return w
}

func (m *model) paneRows() int {
	if m.h > 1 {
		return m.h - 1
	}
	return 24
}

// ---- tmux side-effects ----

// killView tears down the private view session on exit. Grouped sessions are
// independent, so this never touches main or the agents.
func killView() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = tmuxctl.Run(ctx, "kill-session", "-t", viewSession)
}

func envWithout(env []string, key string) []string {
	out := env[:0:0]
	for _, e := range env {
		if !strings.HasPrefix(e, key+"=") {
			out = append(out, e)
		}
	}
	return out
}
