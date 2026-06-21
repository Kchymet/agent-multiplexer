// Package tui renders the amux dashboard with Bubble Tea. A single model
// drives two layouts: the compact side-pane "rail" and the full-screen "dash".
// It subscribes to the daemon's snapshot stream and turns keystrokes into
// control actions.
package tui

import (
	"fmt"
	"os"
	"os/exec"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"amux/internal/core"
	"amux/internal/daemon"
)

// Run starts the TUI. full selects the dashboard layout (alt-screen); otherwise
// it renders the inline rail.
func Run(full bool) error {
	m := &model{full: full, status: "connecting…"}
	var opts []tea.ProgramOption
	if full {
		opts = append(opts, tea.WithAltScreen())
	}
	p := tea.NewProgram(m, opts...)
	_, err := p.Run()
	return err
}

type model struct {
	full          bool
	client        *daemon.Client
	sessions      []core.Session
	cursor        int
	width, height int
	status        string
	connected     bool
	confirmDelete string // workspace name awaiting a second `x` to confirm deletion
}

// ---- messages ------------------------------------------------------------

type connectedMsg struct{ c *daemon.Client }
type disconnectedMsg struct{ err error }
type frameMsg struct{ f daemon.Frame }
type actionDoneMsg struct{ err error }

func (m *model) Init() tea.Cmd { return connectCmd }

func connectCmd() tea.Msg {
	c, err := daemon.Dial()
	if err != nil {
		return disconnectedMsg{err}
	}
	return connectedMsg{c}
}

func readCmd(c *daemon.Client) tea.Cmd {
	return func() tea.Msg {
		f, err := c.Next()
		if err != nil {
			return disconnectedMsg{err}
		}
		return frameMsg{f}
	}
}

func reconnectCmd() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return connectCmd() })
}

func (m *model) sendCmd(a core.Action) tea.Cmd {
	c := m.client
	return func() tea.Msg {
		if c == nil {
			return actionDoneMsg{fmt.Errorf("not connected")}
		}
		return actionDoneMsg{c.Send(a)}
	}
}

// ---- update --------------------------------------------------------------

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case connectedMsg:
		m.client = msg.c
		m.connected = true
		m.status = "connected"
		return m, readCmd(msg.c)

	case disconnectedMsg:
		m.client = nil
		m.connected = false
		m.status = "daemon offline — reconnecting…"
		return m, reconnectCmd()

	case frameMsg:
		if s := msg.f.Snapshot; s != nil {
			m.sessions = s.Sessions
			m.clampCursor()
		}
		if r := msg.f.Result; r != nil {
			if r.OK {
				m.status = "ok"
			} else {
				m.status = "✗ " + r.Error
			}
		}
		if m.client != nil {
			return m, readCmd(m.client)
		}
		return m, nil

	case actionDoneMsg:
		if msg.err != nil {
			m.status = "✗ " + msg.err.Error()
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *model) handleKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := k.String()
	// Any key other than a second `x` cancels a pending delete confirmation.
	if key != "x" {
		m.confirmDelete = ""
	}
	switch key {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.sessions)-1 {
			m.cursor++
		}
	case "enter":
		if s := m.selected(); s != nil {
			m.status = "opening " + s.ID + "…"
			return m, m.sendCmd(core.Action{Action: "open", ID: s.ID})
		}
	case "n":
		return m, newSessionCmd
	case "a":
		if s := m.selected(); s != nil && s.ID != "console" {
			root := s.RootID
			if s.IsRoot {
				root = s.ID
			}
			if root != "" {
				return m, addAgentCmd(root)
			}
		}
		m.status = "select a session to add an agent to"
	case "x":
		s := m.selected()
		if s == nil {
			return m, nil
		}
		if m.confirmDelete == s.ID {
			m.confirmDelete = ""
			m.status = "deleting " + s.ID + "…"
			return m, m.sendCmd(core.Action{Action: "delete", ID: s.ID})
		}
		m.confirmDelete = s.ID
		m.status = "delete " + s.ID + "? press x again"
	case "R":
		return m, m.sendCmd(core.Action{Action: "refresh"})
	}
	return m, nil
}

// newSessionCmd opens the interactive session creator in a tmux popup.
func newSessionCmd() tea.Msg { return popup("session", "new") }

// addAgentCmd opens the add-agent flow for a root in a tmux popup.
func addAgentCmd(rootID string) tea.Cmd {
	return func() tea.Msg { return popup("session", "add", rootID) }
}

// popup runs `amux <args...>` in a tmux popup (its own TTY for fzf/prompts); the
// rail keeps running beneath. Fire-and-forget, reaped to avoid zombies.
func popup(args ...string) tea.Msg {
	self, err := os.Executable()
	if err != nil {
		return actionDoneMsg{err}
	}
	full := append([]string{"-L", core.TmuxSocket, "display-popup", "-E", "-w", "80%", "-h", "80%", "--", self}, args...)
	c := exec.Command("tmux", full...)
	if err := c.Start(); err != nil {
		return actionDoneMsg{err}
	}
	go func() { _ = c.Wait() }()
	return actionDoneMsg{nil}
}

func (m *model) clampCursor() {
	if m.cursor >= len(m.sessions) {
		m.cursor = len(m.sessions) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

func (m *model) selected() *core.Session {
	if m.cursor < 0 || m.cursor >= len(m.sessions) {
		return nil
	}
	return &m.sessions[m.cursor]
}

// ---- styles --------------------------------------------------------------

var (
	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	dimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	selStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("231")).Background(lipgloss.Color("24"))
	errStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("231")).Background(lipgloss.Color("24"))
)

// stateColor styles a session's status sub-line by its activity state: green
// while working, amber when blocked on the user, blue when ready, dim when idle.
func stateColor(state string) lipgloss.Style {
	switch state {
	case core.StateRunning:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("114")) // green: working
	case core.StateWaiting:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("141")) // purple: prompt awaiting you
	case core.StateReady:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("39")) // blue: ready
	case core.StateUnknown:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("220")) // yellow: live, status unknown
	default:
		return dimStyle // idle
	}
}

// glyph encodes the row: console ⚙, root ▸, loop ∞; otherwise the agent's
// activity state — running/waiting ●, ready ◐, idle ○.
func glyph(s core.Session) string {
	switch {
	case s.Mode == "console":
		return "⚙"
	case s.IsRoot:
		return "▸"
	case s.Mode == "loop":
		return "∞"
	case s.Mode == "external":
		return "◇" // a Claude session amux didn't launch
	}
	switch s.State {
	case core.StateRunning, core.StateWaiting, core.StateUnknown:
		return "●"
	case core.StateReady:
		return "◐"
	default:
		return "○" // idle
	}
}

func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n == 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}
