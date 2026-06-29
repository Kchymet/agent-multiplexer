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
	"amux/internal/panespec"
	"amux/internal/vterm"
)

const sidebarWidth = 26

// Run starts the native TUI and blocks until the user quits. Quitting closes
// every embedded agent terminal (they're hosted in-process, not by tmux).
func Run() error {
	if sock, inside := insideAmux(); inside {
		return fmt.Errorf("can't run the native TUI from inside amux (TMUX=%s) — detach first (Alt-q / C-a d), then run `amux` from your normal shell", sock)
	}
	m := &model{terms: map[paneKey]*vterm.Terminal{}, dataCh: make(chan struct{}, 1), status: "connecting…"}
	_, err := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion()).Run()
	for _, t := range m.terms {
		_ = t.Close()
	}
	return err
}

// Each agent owns a row of tabs, switched with Alt+1/2/3: the agent itself, an
// editor (nvim by default, $AMUX_EDITOR), and a shell — the latter two run in the
// agent's worktree dir. paneKey identifies one tab of one agent.
const (
	tabAgent = iota
	tabEditor
	tabTerminal
	tabCount
)

var tabNames = [tabCount]string{"agent", "editor", "term"}

type paneKey struct {
	id  string
	tab int
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
	terms    map[paneKey]*vterm.Terminal // live panes, keyed by (agent id, tab)
	attached string                      // agent id currently shown in the main pane
	tab      int                         // which tab of the attached agent is shown
	confirm  *confirmState               // a pending confirmation modal, or nil
	form     *formState                  // a pending form modal, or nil
	dataCh   chan struct{}
	w, h     int
	status   string
}

// confirmState is a pending yes/no confirmation modal: the question shown, and
// the daemon action to dispatch if the user confirms.
type confirmState struct {
	message string
	action  core.Action
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
type actionSentMsg struct{ err error }

// agentReadyMsg carries a resolved agent launch spec back to the UI goroutine,
// where the vterm is actually started. Resolving the spec (trust dir, resume
// detection, binary lookup) can touch disk, so it runs off-thread.
type agentReadyMsg struct {
	id   string
	tab  int
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

// cur returns the terminal for the currently shown agent+tab, or nil.
func (m *model) cur() *vterm.Terminal { return m.terms[paneKey{m.attached, m.tab}] }

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

	case actionSentMsg:
		if msg.err != nil {
			m.status = "action failed: " + msg.err.Error()
		}
		return m, nil

	case agentReadyMsg:
		if msg.err != nil {
			m.status = "launch failed: " + msg.err.Error()
			return m, nil
		}
		m.embed(msg)
		return m, nil

	case tea.MouseMsg:
		return m.handleMouse(msg)

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

// handleMouse routes a mouse event. Wheel scrolling over the sidebar moves the
// workspace selection; anything landing in the main pane is forwarded to the
// embedded agent (translated into that pane's own coordinates) so you can scroll
// or click an agent just by pointing at it — focus follows a button press. Events
// over the borders/help line, or while a modal is up, are ignored.
func (m *model) handleMouse(ev tea.MouseMsg) (tea.Model, tea.Cmd) {
	if m.form != nil || m.confirm != nil {
		return m, nil
	}
	if tea.MouseEvent(ev).IsWheel() && ev.X < sidebarWidth {
		switch ev.Button {
		case tea.MouseButtonWheelUp:
			m.move(-1)
		case tea.MouseButtonWheelDown:
			m.move(1)
		}
		return m, nil
	}
	t := m.cur()
	if t == nil {
		return m, nil
	}
	// Strip the sidebar+divider (left) and the top border (above) to get the
	// agent pane's own 0-based coordinates.
	x, y := ev.X-(sidebarWidth+1), ev.Y-1
	if x < 0 || y < 0 || x >= m.mainWidth() || y >= m.paneRows() {
		return m, nil
	}
	if ev.Action == tea.MouseActionPress && !tea.MouseEvent(ev).IsWheel() {
		m.focus = focusAgent
	}
	t.MouseEvent(mouseToVT(ev, x, y))
	return m, nil
}

// reapClosed drops any pane whose process has exited. If the pane currently
// shown died, fall back to the agent tab, else detach to the sidebar.
func (m *model) reapClosed() {
	for k, t := range m.terms {
		if t.Closed() {
			delete(m.terms, k)
		}
	}
	if m.attached == "" || m.cur() != nil {
		return
	}
	if t := m.terms[paneKey{m.attached, tabAgent}]; t != nil && !t.Closed() {
		m.tab = tabAgent
		return
	}
	m.attached = ""
	m.tab = tabAgent
	m.focus = focusSidebar
}

func (m *model) handleKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	// A form modal captures every key until submitted or cancelled.
	if m.form != nil {
		return m.handleForm(k)
	}
	// A confirmation modal is modal: it captures every key until answered.
	if m.confirm != nil {
		switch k.String() {
		case "y", "enter":
			a := m.confirm.action
			m.confirm = nil
			m.status = ""
			return m, m.sendCmd(a)
		case "n", "esc", "q", "ctrl+c":
			m.confirm = nil
			m.status = "cancelled"
		}
		return m, nil
	}

	// Alt/Option jumps work everywhere — even with the agent focused — so you can
	// always move between the rail and the agent without a prefix.
	switch k.String() {
	case "alt+h":
		m.focus = focusSidebar
		return m, nil
	case "alt+l":
		m.focusAgent()
		return m, nil
	case "alt+a":
		m.toggleFocus()
		return m, nil
	case "alt+q":
		return m, tea.Quit
	case "alt+1":
		return m, m.switchTab(tabAgent)
	case "alt+2":
		return m, m.switchTab(tabEditor)
	case "alt+3":
		return m, m.switchTab(tabTerminal)
	}

	// Agent focused: forward every other key straight to the agent.
	if m.focus == focusAgent && m.cur() != nil {
		if b := keyToBytes(k); len(b) > 0 {
			_, _ = m.cur().Write(b)
		}
		return m, nil
	}

	// Sidebar focus.
	switch k.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "up", "k":
		m.move(-1)
	case "down", "j":
		m.move(1)
	case "enter", "l", "right":
		return m, m.attachSelected()
	case "a": // add an agent — on a repo header, a repo-scoped agent; on a workgroup, another agent
		s := m.selected()
		switch {
		case s == nil:
			m.status = "select a repo or workgroup to add an agent"
		case s.Kind == "repo":
			m.openNewRepoAgentForm(s.ID, s.Title)
		case s.Section == core.SectionWorkgroups:
			if root := m.rootOf(s); root != nil {
				m.openAddAgentForm(root.ID, root.Title)
			}
		default:
			m.status = "select a repo or workgroup to add an agent"
		}
		return m, nil
	case "w": // new work-scoped workgroup (opens a settings form)
		m.openNewWorkgroupForm()
		return m, nil
	case "R": // track a new repository (opens a one-field form)
		m.openAddRepoForm()
		return m, nil
	case "ctrl+r": // force a state refresh (the daemon also auto-polls)
		return m, m.sendCmd(core.Action{Action: "refresh"})
	case "m": // move the selected agent into a new work-scoped workgroup (confirm first)
		if s := m.selected(); s != nil && attachable(s) && s.ID != console.ID && s.Section != core.SectionArchived {
			m.confirm = &confirmState{
				message: "Move " + s.Title + " into a new work-scoped workgroup?",
				action:  core.Action{Action: "move", ID: s.ID},
			}
			return m, nil
		}
		m.status = "select an agent to move"
	case "x": // mark the selected agent done/archived (or restore an archived one)
		if s := m.selected(); s != nil && s.ID != console.ID && (attachable(s) || s.IsRoot) {
			if s.Section == core.SectionArchived {
				m.status = "restoring " + s.Title + "…"
			} else {
				m.status = "archiving " + s.Title + "…"
			}
			return m, m.sendCmd(core.Action{Action: "archive", ID: s.ID})
		}
		m.status = "select an agent to archive"
	case "D": // permanently delete the selected agent/workgroup (worktrees + branch), with a confirm
		if s := m.selected(); s != nil && s.ID != console.ID && (attachable(s) || s.IsRoot) {
			what := "agent"
			if s.IsRoot {
				what = "workgroup"
			}
			m.confirm = &confirmState{
				message: "Permanently delete " + what + " " + s.Title + "?\nThis removes its worktrees and branch — it can't be undone.",
				action:  core.Action{Action: "delete", ID: s.ID},
			}
			return m, nil
		}
		m.status = "select an agent or workgroup to delete"
	case "r": // rename the selected agent/workgroup (set a display name; id is unchanged)
		if s := m.selected(); s != nil && s.ID != console.ID && (attachable(s) || s.IsRoot) {
			m.openRenameForm(s.ID, s.Title)
			return m, nil
		}
		m.status = "select a session to rename"
	case "tab":
		m.focusAgent()
	}
	return m, nil
}

// sendCmd dispatches a daemon action (create/move/…). Writes are serialized on
// the client; the result returns as a frame the read loop already handles, and
// the resulting poll refreshes the rail.
func (m *model) sendCmd(a core.Action) tea.Cmd {
	c := m.client
	return func() tea.Msg {
		if c == nil {
			return actionSentMsg{fmt.Errorf("not connected")}
		}
		return actionSentMsg{c.Send(a)}
	}
}

// focusAgent moves focus to the agent pane if one is embedded.
func (m *model) focusAgent() {
	if m.cur() != nil {
		m.focus = focusAgent
	}
}

// toggleFocus flips between the sidebar and the agent pane.
func (m *model) toggleFocus() {
	if m.focus == focusAgent {
		m.focus = focusSidebar
		return
	}
	m.focusAgent()
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

// rootOf returns the workgroup root row for a selected workgroup-section row:
// the row itself if it is the root, otherwise the root its RootID points at.
// Returns nil if no matching root is in the snapshot.
func (m *model) rootOf(s *core.Session) *core.Session {
	if s.IsRoot {
		return s
	}
	if s.RootID == "" {
		return nil
	}
	for i := range m.sessions {
		if m.sessions[i].ID == s.RootID {
			return &m.sessions[i]
		}
	}
	return nil
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
	// A repo header is a container for its repo-scoped agents: open the first one,
	// or create one if it has none (auto-creates the repo-scoped workgroup).
	if s.Kind == "repo" {
		if sub := m.firstChild(s.ID); sub != nil {
			s = sub
		} else {
			m.openNewRepoAgentForm(s.ID, s.Title)
			return nil
		}
	} else if s.IsRoot {
		// A workgroup root isn't itself attachable — open its first agent so the
		// natural choice of selecting the workgroup row "just works".
		if sub := m.firstChild(s.ID); sub != nil {
			s = sub
		}
	}
	if !attachable(s) {
		m.status = "select an agent"
		return nil
	}
	m.attached = s.ID
	m.tab = tabAgent
	if t := m.terms[paneKey{s.ID, tabAgent}]; t != nil && !t.Closed() { // already running
		m.focus = focusAgent
		m.status = ""
		return nil
	}
	m.status = "launching " + s.Title + "…"
	return m.launchPane(s.ID, tabAgent)
}

// switchTab shows tab t of the attached agent, launching its pane (editor/term)
// on first use. Works even while the agent is focused (Alt+1/2/3).
func (m *model) switchTab(t int) tea.Cmd {
	if m.attached == "" {
		m.status = "open an agent first"
		return nil
	}
	m.tab = t
	m.focus = focusAgent
	if term := m.terms[paneKey{m.attached, t}]; term != nil && !term.Closed() {
		m.status = ""
		return nil
	}
	m.status = "opening " + tabNames[t] + "…"
	return m.launchPane(m.attached, t)
}

// launchPane resolves a pane's launch spec off-thread (it touches disk), then an
// agentReadyMsg embeds it on the UI goroutine.
func (m *model) launchPane(id string, tab int) tea.Cmd {
	return func() tea.Msg {
		dir, env, argv, err := panespec.Resolve(id, tab)
		return agentReadyMsg{id: id, tab: tab, dir: dir, env: env, argv: argv, err: err}
	}
}

// embed starts a pane's process directly in a new vterm and shows it. The child
// inherits the app's environment (PATH etc.), minus $TMUX, plus the agent's
// AMUX_* vars — the same environment a manual shell launch gets.
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
	m.terms[paneKey{msg.id, msg.tab}] = t
	m.attached = msg.id
	m.tab = msg.tab
	m.focus = focusAgent
	m.status = ""
}

// attachable reports whether a row hosts an agent we can embed: the console, a
// work-scoped workgroup's sub-agent, or a repo-scoped agent nested under a repo
// header — but not a workgroup container, a repo header, or a detached row.
func attachable(s *core.Session) bool {
	if s.ID == console.ID {
		return true
	}
	if s.Section == core.SectionArchived {
		return true // archived agents can still be opened to review or resume
	}
	return !s.IsRoot && s.RootID != "" && s.Kind != "repo"
}

func (m *model) mainWidth() int {
	w := m.w - sidebarWidth - 1
	if w < 1 {
		return 1
	}
	return w
}

// paneRows is the height of the body (sidebar / agent pane), leaving room for
// the top header bar (1) and the footer rule + help (2).
func (m *model) paneRows() int {
	if r := m.h - 3; r > 1 {
		return r
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
