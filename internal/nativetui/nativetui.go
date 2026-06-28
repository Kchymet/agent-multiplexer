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

	"amux/internal/console"
	"amux/internal/core"
	"amux/internal/daemon"
	"amux/internal/store"
	"amux/internal/vterm"
	"amux/internal/wsops"
)

const sidebarWidth = 26

// Run starts the native TUI and blocks until the user quits (which detaches —
// agents keep running in the tmux server). It refuses to run inside the amux
// tmux server, where embedding a tmux client would nest and hang.
func Run() error {
	if sock, inside := insideAmux(); inside {
		return fmt.Errorf("can't run the native TUI from inside amux (TMUX=%s) — detach first (Alt-q / C-a d), then run `amux` from your normal shell", sock)
	}
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
type agentStartedMsg struct {
	id, target string
	err        error
}

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

	case agentStartedMsg:
		if msg.err != nil {
			m.status = "launch failed: " + msg.err.Error()
			return m, nil
		}
		return m, m.embed(msg.id, msg.target)

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

// firstChild returns the first sub-agent belonging to the given root, or nil if
// it has none. Subs always follow their root in the emitted session order.
func (m *model) firstChild(rootID string) *core.Session {
	for i := range m.sessions {
		if m.sessions[i].RootID == rootID {
			return &m.sessions[i]
		}
	}
	return nil
}

// attachSelected embeds the selected agent in the main pane and focuses it,
// launching its dedicated session first if it isn't running. The session is
// private to that agent (rail-free, sized to this client), so there's nothing to
// negotiate or leak.
func (m *model) attachSelected() tea.Cmd {
	s := m.selected()
	if s == nil {
		return nil
	}
	// A workspace root isn't itself attachable — it's a container. Opening it
	// should open its agent (roots always have ≥1), so the natural choice of
	// selecting the workspace row "just works" instead of dead-ending.
	if s.IsRoot {
		if sub := m.firstChild(s.ID); sub != nil {
			s = sub
		}
	}
	if !attachable(s) {
		m.status = "select an agent under a workspace"
		return nil
	}
	if m.attached == s.ID && m.term != nil { // already showing it — just focus
		m.focus = focusAgent
		return nil
	}

	if s.WindowID != "" { // already live — embed immediately (fast)
		return m.embed(s.ID, s.WindowID)
	}
	// Not running yet: cold-starting the dedicated tmux session + agent takes
	// seconds, so launch it off the UI thread. Running it inline froze the whole
	// switcher until the agent was up. The agentStartedMsg embeds it when ready.
	m.status = "launching " + s.Title + "…"
	id := s.ID
	return func() tea.Msg {
		target, err := startAgent(id)
		return agentStartedMsg{id: id, target: target, err: err}
	}
}

// embed attaches the given live tmux session in the main pane and focuses it.
// It only spawns a client in a PTY, so it's fast enough to run on the UI thread.
func (m *model) embed(id, target string) tea.Cmd {
	if m.term != nil { // switch: drop the previous embed
		_ = m.term.Close()
		m.term = nil
	}
	t := vterm.New(m.mainWidth(), m.paneRows())
	t.OnData(func() {
		select {
		case m.dataCh <- struct{}{}:
		default:
		}
	})
	cmd := exec.Command("tmux", "-L", core.TmuxSocket, "attach", "-t", "="+target)
	cmd.Env = append(envWithout(os.Environ(), "TMUX"), "TERM=xterm-256color")
	if err := t.Start(cmd); err != nil {
		m.status = "attach failed: " + err.Error()
		return nil
	}
	m.term = t
	m.attached = id
	m.focus = focusAgent
	m.status = ""
	return nil
}

// attachable reports whether a row hosts an agent we can embed (the console or a
// workspace's sub-agent) — not a workspace container, repo, or detached row.
func attachable(s *core.Session) bool {
	if s.ID == console.ID {
		return true
	}
	return s.Section == core.SectionWorkspaces && !s.IsRoot && s.RootID != ""
}

// startAgent ensures the agent's dedicated session is running and returns its
// name (launching/resuming it if needed).
func startAgent(id string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	if id == console.ID {
		if err := console.Ensure(); err != nil {
			return "", err
		}
		return wsops.EnsureAgentSession(ctx, console.Session())
	}
	db, err := store.Open()
	if err != nil {
		return "", err
	}
	defer db.Close()
	s, ok, err := db.GetSession(id)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("no such agent %q", id)
	}
	return wsops.EnsureAgentSession(ctx, s)
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

func envWithout(env []string, key string) []string {
	out := env[:0:0]
	for _, e := range env {
		if !strings.HasPrefix(e, key+"=") {
			out = append(out, e)
		}
	}
	return out
}
