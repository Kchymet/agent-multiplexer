// Package vtdemo is a throwaway harness to eyeball embedded-terminal fidelity:
// it runs a child command inside a vterm rendered in a full-screen Bubble Tea
// pane, forwarding keystrokes and resizes. It's the Phase 0 go/no-go for the
// native TUI — does an interactive program render acceptably inside Bubble Tea?
package vtdemo

import (
	"os"
	"os/exec"

	tea "github.com/charmbracelet/bubbletea"

	"amux/internal/vterm"
)

// Run launches the demo embedding args (default: bash). Quit with Ctrl+\.
func Run(args []string) error {
	if len(args) == 0 {
		args = []string{"bash"}
	}
	m := &model{args: args, dataCh: make(chan struct{}, 1)}
	_, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	if m.term != nil {
		_ = m.term.Close()
	}
	return err
}

type dataMsg struct{}

type model struct {
	args   []string
	term   *vterm.Terminal
	dataCh chan struct{}
	w, h   int
	err    error
}

func (m *model) Init() tea.Cmd { return waitData(m.dataCh) }

func waitData(ch chan struct{}) tea.Cmd {
	return func() tea.Msg { <-ch; return dataMsg{} }
}

// paneRows is the child height: full screen minus a one-line footer.
func (m *model) paneRows() int {
	if m.h > 1 {
		return m.h - 1
	}
	return 24
}

func (m *model) ensureStarted() {
	if m.term != nil || m.w == 0 {
		return
	}
	m.term = vterm.New(m.w, m.paneRows())
	m.term.OnData(func() {
		select {
		case m.dataCh <- struct{}{}:
		default:
		}
	})
	cmd := exec.Command(m.args[0], m.args[1:]...)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	if err := m.term.Start(cmd); err != nil {
		m.err = err
	}
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.ensureStarted()
		if m.term != nil {
			m.term.Resize(m.w, m.paneRows())
		}
		return m, nil

	case dataMsg:
		if m.term != nil && m.term.Closed() {
			return m, tea.Quit
		}
		return m, waitData(m.dataCh)

	case tea.KeyMsg:
		if msg.Type == tea.KeyCtrlBackslash { // demo escape hatch
			return m, tea.Quit
		}
		if m.term != nil {
			if b := keyToBytes(msg); len(b) > 0 {
				_, _ = m.term.Write(b)
			}
		}
		return m, nil
	}
	return m, nil
}

func (m *model) View() string {
	if m.err != nil {
		return "vtdemo error: " + m.err.Error() + "\n"
	}
	if m.term == nil {
		return "starting…"
	}
	return m.term.Render() + "\n  [embedded terminal — Ctrl+\\ to quit demo]"
}

// keyToBytes translates a Bubble Tea key event into the bytes a terminal would
// send to a child process. Covers the common cases — enough to drive an
// interactive program for the fidelity check.
func keyToBytes(k tea.KeyMsg) []byte {
	switch k.Type {
	case tea.KeyRunes:
		return []byte(string(k.Runes))
	case tea.KeySpace:
		return []byte(" ")
	case tea.KeyEnter:
		return []byte("\r")
	case tea.KeyTab:
		return []byte("\t")
	case tea.KeyEsc:
		return []byte{0x1b}
	case tea.KeyBackspace:
		return []byte{0x7f}
	case tea.KeyDelete:
		return []byte("\x1b[3~")
	case tea.KeyUp:
		return []byte("\x1b[A")
	case tea.KeyDown:
		return []byte("\x1b[B")
	case tea.KeyRight:
		return []byte("\x1b[C")
	case tea.KeyLeft:
		return []byte("\x1b[D")
	case tea.KeyHome:
		return []byte("\x1b[H")
	case tea.KeyEnd:
		return []byte("\x1b[F")
	case tea.KeyPgUp:
		return []byte("\x1b[5~")
	case tea.KeyPgDown:
		return []byte("\x1b[6~")
	case tea.KeyCtrlC:
		return []byte{0x03}
	case tea.KeyCtrlD:
		return []byte{0x04}
	case tea.KeyCtrlU:
		return []byte{0x15}
	case tea.KeyCtrlK:
		return []byte{0x0b}
	case tea.KeyCtrlW:
		return []byte{0x17}
	case tea.KeyCtrlA:
		return []byte{0x01}
	case tea.KeyCtrlE:
		return []byte{0x05}
	case tea.KeyCtrlL:
		return []byte{0x0c}
	case tea.KeyCtrlR:
		return []byte{0x12}
	}
	return nil
}
