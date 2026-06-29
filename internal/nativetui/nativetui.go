// Package nativetui is amux's native Bubble Tea front-end: a sidebar switcher
// (workspaces / repos / detached) on the left and the selected agent embedded
// on the right. Each agent runs directly in an embedded vterm (its own PTY) —
// no tmux. Switching agents keeps the others running in the background; quitting
// the app ends the live terminals (the Claude conversation is preserved and
// resumes on reopen).
package nativetui

import (
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

// Run starts the native TUI and blocks until the user quits. Quitting closes
// every embedded agent terminal (they're hosted in-process, not by tmux).
func Run() error {
	if sock, inside := insideAmux(); inside {
		return fmt.Errorf("can't run the native TUI from inside amux (TMUX=%s) — detach first (Alt-q / C-a d), then run `amux` from your normal shell", sock)
	}
	m := &model{terms: map[string]*vterm.Terminal{}, dataCh: make(chan struct{}, 1), status: "connecting…"}
	_, err := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion()).Run()
	for _, t := range m.terms {
		_ = t.Close()
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
	focus    focus                       // sidebar or agent pane
	terms    map[string]*vterm.Terminal  // live agent terminals, keyed by session id
	attached string                      // session id currently shown in the main pane
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

// agentReadyMsg carries a resolved agent launch spec back to the UI goroutine,
// where the vterm is actually started. Resolving the spec (trust dir, resume
// detection, binary lookup) can touch disk, so it runs off-thread.
type agentReadyMsg struct {
	id   string
	dir  string
	env  []string
	argv []string
	err  error
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

// cur returns the terminal currently shown in the main pane, or nil.
func (m *model) cur() *vterm.Terminal { return m.terms[m.attached] }

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		for _, t := range m.terms {
			t.Resize(m.mainWidth(), m.paneRows())
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
		m.reapClosed()
		return m, waitData(m.dataCh)

	case agentReadyMsg:
		if msg.err != nil {
			m.status = "launch failed: " + msg.err.Error()
			return m, nil
		}
		m.embed(msg)
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

// reapClosed drops any agent terminal whose process has exited.
func (m *model) reapClosed() {
	for id, t := range m.terms {
		if t.Closed() {
			delete(m.terms, id)
			if m.attached == id {
				m.attached = ""
				m.focus = focusSidebar
			}
		}
	}
}

func (m *model) handleKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	// When the agent pane is focused, forward keys to it — except the escape
	// hatch that returns focus to the sidebar.
	if m.focus == focusAgent && m.cur() != nil {
		if k.Type == tea.KeyCtrlA { // prefix: pop back to the switcher
			m.focus = focusSidebar
			return m, nil
		}
		if b := keyToBytes(k); len(b) > 0 {
			_, _ = m.cur().Write(b)
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
		if m.cur() != nil {
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

// attachSelected shows the selected agent in the main pane. If it's already
// running in the background it just refocuses it; otherwise it resolves the
// launch spec off-thread and embeds a fresh terminal when ready.
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
	if t := m.terms[s.ID]; t != nil && !t.Closed() { // already running — just show it
		m.attached = s.ID
		m.focus = focusAgent
		m.status = ""
		return nil
	}

	m.status = "launching " + s.Title + "…"
	id := s.ID
	return func() tea.Msg {
		dir, env, argv, err := agentCommand(id)
		return agentReadyMsg{id: id, dir: dir, env: env, argv: argv, err: err}
	}
}

// embed starts the agent process directly in a new vterm and shows it. The child
// inherits the app's environment (PATH etc.), minus $TMUX, plus the agent's
// AMUX_* vars — this is the same environment a manual shell launch gets.
func (m *model) embed(msg agentReadyMsg) {
	t := vterm.New(m.mainWidth(), m.paneRows())
	t.OnData(func() {
		select {
		case m.dataCh <- struct{}{}:
		default:
		}
	})
	cmd := exec.Command(msg.argv[0], msg.argv[1:]...)
	cmd.Dir = msg.dir
	base := envWithout(envWithout(os.Environ(), "TMUX"), "TERM")
	cmd.Env = append(append(base, msg.env...), "TERM=xterm-256color")
	if err := t.Start(cmd); err != nil {
		m.status = "launch failed: " + err.Error()
		return
	}
	m.terms[msg.id] = t
	m.attached = msg.id
	m.focus = focusAgent
	m.status = ""
}

// attachable reports whether a row hosts an agent we can embed (the console or a
// workspace's sub-agent) — not a workspace container, repo, or detached row.
func attachable(s *core.Session) bool {
	if s.ID == console.ID {
		return true
	}
	return s.Section == core.SectionWorkspaces && !s.IsRoot && s.RootID != ""
}

// agentCommand resolves an agent's launch spec (working dir, extra env, argv) by
// id, preparing the console first when needed.
func agentCommand(id string) (dir string, env, argv []string, err error) {
	if id == console.ID {
		if err = console.Ensure(); err != nil {
			return "", nil, nil, err
		}
		return wsops.AgentCommand(console.Session())
	}
	db, err := store.Open()
	if err != nil {
		return "", nil, nil, err
	}
	defer db.Close()
	s, ok, err := db.GetSession(id)
	if err != nil {
		return "", nil, nil, err
	}
	if !ok {
		return "", nil, nil, fmt.Errorf("no such agent %q", id)
	}
	return wsops.AgentCommand(s)
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
