//go:build linux || darwin

package nativetui

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"amux/internal/vterm"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/creack/pty"
)

type accountsInputFrame struct {
	harness, selected         string
	confirming, active        bool
	event                     string
	events, scroll, secretLen int
	open, hidden              bool
	focus                     focus
	view                      string
}

// Only Init is substituted: it avoids a live daemon. Update, command dispatch,
// rendering, terminal modes, and raw PTY input decoding use production code.
type accountsInputModel struct {
	*model
	frames chan accountsInputFrame
	event  string
	events int
}

func (m *accountsInputModel) Init() tea.Cmd { return nil }
func (m *accountsInputModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch v := msg.(type) {
	case tea.KeyMsg:
		m.event = "key:" + v.String()
		m.events++
	case tea.MouseMsg:
		m.event = "mouse:" + v.String()
		m.events++
	}
	_, cmd := m.model.Update(msg)
	return m, cmd
}
func (m *accountsInputModel) View() string {
	view := m.model.View()
	f := accountsInputFrame{event: m.event, events: m.events, focus: m.focus, view: view, open: m.accounts != nil}
	if m.accounts != nil {
		f.harness, f.selected = m.accounts.harness, m.accounts.selected
		f.confirming, f.active = m.accounts.confirm != nil, m.accounts.attempt.active()
		f.scroll = m.accounts.scroll
		f.secretLen = len(m.accounts.secret)
		f.hidden = m.accounts.secretInput()
	}
	m.frames <- f
	return view
}

type accountsPTY struct {
	input       *os.File
	program     *tea.Program
	frames      chan accountsInputFrame
	screen      *vterm.Terminal
	mu          sync.Mutex
	output      bytes.Buffer
	paneInput   chan []byte
	initial     accountsInputFrame
	blockStatus chan (<-chan struct{})
	requests    chan accountRequest
}

func startAccountsPTY(t *testing.T, w, h int) *accountsPTY {
	t.Helper()
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := pty.Setsize(master, &pty.Winsize{Cols: uint16(w), Rows: uint16(h)}); err != nil {
		t.Fatal(err)
	}
	a := &accountsPTY{input: master, frames: make(chan accountsInputFrame, 512), paneInput: make(chan []byte, 64), blockStatus: make(chan (<-chan struct{}), 1), requests: make(chan accountRequest, 64), screen: vterm.NewRemote(w, h, nil, nil)}
	m := popupFixture(t, w, h)
	m.focus = focusSidebar
	spy := vterm.NewRemote(m.mainWidth(), m.paneRows(), func(b []byte) { a.paneInput <- append([]byte(nil), b...) }, nil)
	pane := m.cur().Render()
	spy.Feed([]byte("\x1b[?1002h\x1b[?1006h" + strings.ReplaceAll(pane, "\n", "\r\n")))
	// Feed is parsed asynchronously. Start the program only once the spy shows
	// the pane, or its first frame (captured below as the initial viewport) can
	// show an empty pane that a later frame then replaces on screen.
	for deadline := time.Now().Add(3 * time.Second); terminalText(spy.Render()) != terminalText(pane); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("pane fixture did not render")
		}
	}
	m.terms[paneKey{m.attached, m.tab}] = spy
	snapshot := m.accountOverview
	snapshot.profiles[0].canAPI = true
	m.accountCall = func(_ context.Context, r accountRequest) (accountResponse, error) {
		a.requests <- r
		if r.op == "status" {
			select {
			case gate := <-a.blockStatus:
				<-gate
			default:
			}
		}
		if r.op == "login" {
			method := "setup-token"
			if r.harness == "codex" {
				method = "browser"
			}
			if r.loginMode == "api" {
				method = "api-key"
			}
			return accountResponse{attempt: &accountAttempt{id: "synthetic", state: "pending", method: method, mode: r.loginMode, expiresAt: time.Now().Add(time.Hour)}}, nil
		}
		return accountResponse{snapshot: snapshot}, nil
	}
	fixture := &accountsInputModel{model: m, frames: a.frames}
	a.program = newProgram(fixture, tea.WithInput(slave), tea.WithOutput(slave), tea.WithoutSignalHandler(), tea.WithEnvironment([]string{"TERM=xterm-256color"}))
	done := make(chan error, 1)
	go func() { _, err := a.program.Run(); done <- err }()
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		buf := make([]byte, 8192)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				a.mu.Lock()
				a.output.Write(buf[:n])
				a.mu.Unlock()
				a.screen.Feed(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		a.program.Quit()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			a.program.Kill()
			t.Error("Bubble Tea did not stop")
		}
		slave.Close()
		master.Close()
		<-readerDone
		a.screen.Close()
		spy.Close()
	})
	a.initial = a.wait(t, func(f accountsInputFrame) bool { return !f.open && strings.Contains(f.view, "Accounts") })
	deadline := time.Now().Add(3 * time.Second)
	for {
		a.mu.Lock()
		out := a.output.String()
		a.mu.Unlock()
		if strings.Contains(out, "\x1b[?1002h") && strings.Contains(out, "\x1b[?1006h") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("production terminal mouse modes not enabled")
		}
		time.Sleep(time.Millisecond)
	}
	return a
}
func (a *accountsPTY) send(t *testing.T, raw string) {
	t.Helper()
	if _, err := a.input.Write([]byte(raw)); err != nil {
		t.Fatal(err)
	}
}
func (a *accountsPTY) wait(t *testing.T, predicate func(accountsInputFrame) bool) accountsInputFrame {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	var last accountsInputFrame
	for {
		select {
		case last = <-a.frames:
			if predicate(last) {
				return last
			}
		case <-timer.C:
			t.Fatalf("terminal input did not reach expected viewport: event=%s events=%d scroll=%d open=%v hidden=%v", last.event, last.events, last.scroll, last.open, last.hidden)
		}
	}
}
func terminalText(s string) string {
	lines := strings.Split(ansi.Strip(s), "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
	}
	return strings.Join(lines, "\n")
}
func (a *accountsPTY) rendered(t *testing.T, f accountsInputFrame, name string, w, h int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for terminalText(a.screen.Render()) != terminalText(f.view) {
		if time.Now().After(deadline) {
			t.Fatalf("PTY renderer did not show the model viewport:\n%s\nwant:\n%s", terminalText(a.screen.Render()), terminalText(f.view))
		}
		time.Sleep(time.Millisecond)
	}
	if dir := os.Getenv("AMUX_ACCOUNTS_NAV_RENDER_DIR"); dir != "" {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("accounts-%dx%d-%s.txt", w, h, name)), []byte(terminalText(a.screen.Render())+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAccountsPTYScrolling(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {120, 32}, {52, 12}} {
		for _, input := range []struct {
			name, down, up string
			step, burst    int
		}{
			{"sgr-wheel", "\x1b[<65;50;10M", "\x1b[<64;50;10M", 3, 1},
			{"x10-wheel", "\x1b[Mab*", "\x1b[M`b*", 3, 1},
			{"trackpad-burst", strings.Repeat("\x1b[<65;50;10M", 3), strings.Repeat("\x1b[<64;50;10M", 3), 9, 3},
		} {
			t.Run(fmt.Sprintf("%dx%d/%s", size[0], size[1], input.name), func(t *testing.T) {
				a := startAccountsPTY(t, size[0], size[1])
				budgetRows := 0
				for _, line := range strings.Split(ansi.Strip(a.initial.view), "\n") {
					if strings.HasPrefix(line, "   5h ") {
						budgetRows++
					}
				}
				if budgetRows != 2 {
					t.Fatal("rail percentages are not indented three spaces")
				}
				a.rendered(t, a.initial, "rail", size[0], size[1])
				a.send(t, "s")
				initial := a.wait(t, func(f accountsInputFrame) bool { return f.open && strings.Contains(f.view, "Claude · subscription") })
				a.rendered(t, initial, "open", size[0], size[1])
				a.send(t, input.down)
				down := a.wait(t, func(f accountsInputFrame) bool { return f.events >= initial.events+input.burst && f.scroll > 0 })
				if down.harness != initial.harness || down.selected != initial.selected {
					t.Fatal("wheel changed provider selection")
				}
				if down.scroll > input.step {
					t.Fatalf("unexpected scroll distance %d", down.scroll)
				}
				a.rendered(t, down, input.name, size[0], size[1])
				if terminalText(initial.view) == terminalText(down.view) {
					t.Fatal("input changed no visible content")
				}
				a.send(t, input.up)
				up := a.wait(t, func(f accountsInputFrame) bool { return f.events >= down.events+input.burst && f.scroll == 0 })
				if up.view != initial.view {
					t.Fatal("scroll back did not restore popup")
				}
				a.send(t, "\x1b[6~")
				page := a.wait(t, func(f accountsInputFrame) bool { return f.event == "key:pgdown" && f.scroll > 0 })
				a.rendered(t, page, "page-down", size[0], size[1])
				a.send(t, "\x1b[5~")
				a.wait(t, func(f accountsInputFrame) bool { return f.event == "key:pgup" && f.scroll == 0 })
				a.send(t, "\x1b")
				closed := a.wait(t, func(f accountsInputFrame) bool { return !f.open && f.focus == focusSidebar })
				a.rendered(t, closed, "closed", size[0], size[1])
				if len(a.paneInput) != 0 {
					t.Fatal("modal scroll leaked input to the underlying pane")
				}
			})
		}
	}
}

func TestAccountsPTYSecretSelectionIsolation(t *testing.T) {
	for _, loginKey := range []string{"e", "a"} {
		t.Run(loginKey, func(t *testing.T) {
			a := startAccountsPTY(t, 52, 12)
			a.send(t, "s")
			a.wait(t, func(f accountsInputFrame) bool { return f.open && strings.Contains(f.view, "Claude · subscription") })
			a.send(t, loginKey)
			a.wait(t, func(f accountsInputFrame) bool { return strings.Contains(f.view, "Start ") })
			a.send(t, "\r")
			a.wait(t, func(f accountsInputFrame) bool { return f.hidden })
			a.send(t, "jk")
			typed := a.wait(t, func(f accountsInputFrame) bool { return f.secretLen == 2 })
			if strings.Contains(typed.view, "jk select") {
				t.Fatal("protected input advertises provider selection")
			}
			if typed.scroll != 0 {
				t.Fatal("credential characters became navigation")
			}
			a.send(t, "\x1b[B")
			arrow := a.wait(t, func(f accountsInputFrame) bool { return f.event == "key:down" })
			if arrow.secretLen != 2 || arrow.harness != "claude" || arrow.selected != "claude:central" {
				t.Fatal("arrow corrupted protected input")
			}
			a.send(t, "\x1b[200~jk\x1b[201~")
			pasted := a.wait(t, func(f accountsInputFrame) bool { return f.secretLen == 4 })
			if pasted.scroll != arrow.scroll {
				t.Fatal("credential paste scrolled popup")
			}
			a.send(t, "\x15")
			a.wait(t, func(f accountsInputFrame) bool { return f.secretLen == 0 })
			a.send(t, "\x1b")
			a.wait(t, func(f accountsInputFrame) bool { return !f.open })
			a.mu.Lock()
			out := a.output.String()
			a.mu.Unlock()
			if strings.Contains(out, "jk") || len(a.paneInput) != 0 {
				t.Fatal("protected input escaped the modal")
			}
		})
	}
}

func (a *accountsPTY) key(t *testing.T, before accountsInputFrame, raw string) accountsInputFrame {
	t.Helper()
	a.send(t, raw)
	return a.wait(t, func(f accountsInputFrame) bool { return f.events > before.events })
}
func (a *accountsPTY) noMutations(t *testing.T) {
	t.Helper()
	for len(a.requests) > 0 {
		if r := <-a.requests; r.op != "status" {
			t.Fatalf("navigation dispatched %s", r.op)
		}
	}
}

func TestAccountsPTYProviderSelection(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {120, 32}, {52, 12}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			a := startAccountsPTY(t, size[0], size[1])
			a.send(t, "s")
			f := a.wait(t, func(f accountsInputFrame) bool { return f.selected == "claude:central" })
			for _, nav := range []struct{ raw, provider string }{
				{"\x1b[B", "codex"}, {"\x1b[B", "codex"}, {"\x1bOA", "claude"}, {"\x1b[A", "claude"},
				{"j", "codex"}, {"j", "codex"}, {"k", "claude"}, {"k", "claude"},
				{"\t", "codex"}, {"\x1b[Z", "claude"}, {"\x1bOB", "codex"},
			} {
				f = a.key(t, f, nav.raw)
				if f.harness != nav.provider || f.selected != nav.provider+":central" || f.scroll != 0 {
					t.Fatalf("%q did not select %s: harness=%s profile=%s scroll=%d", nav.raw, nav.provider, f.harness, f.selected, f.scroll)
				}
				if !strings.Contains(ansi.Strip(f.view), "› "+accountHarnessName(nav.provider)) {
					t.Fatal("provider selection has no visible highlight")
				}
				a.rendered(t, f, "selected-"+nav.provider, size[0], size[1])
			}
			a.noMutations(t)
			var pages strings.Builder
			identityCaptured := false
			for i := 0; i < 100; i++ {
				before := f.scroll
				f = a.key(t, f, "\x1b[6~")
				pages.WriteString(ansi.Strip(f.view))
				if !identityCaptured && strings.Contains(f.view, "fixture@example.test") {
					a.rendered(t, f, "codex-identity", size[0], size[1])
					identityCaptured = true
				}
				if f.scroll == before {
					break
				}
			}
			if !strings.Contains(pages.String(), "Selected: Codex") || !strings.Contains(pages.String(), "fixture@example.test") {
				t.Fatal("selected provider details not reachable")
			}
			a.rendered(t, f, "codex-details", size[0], size[1])
			// Clamping at the selected end must not reset its details viewport.
			end := f.scroll
			f = a.key(t, f, "j")
			if f.harness != "codex" || f.scroll != end {
				t.Fatal("clamped navigation moved details")
			}
			f = a.key(t, f, "k")
			if f.harness != "claude" || f.selected != "claude:central" || f.scroll != 0 {
				t.Fatal("provider change did not reset details")
			}
			a.noMutations(t)
		})
	}
}

func TestAccountsPTYSelectionGuards(t *testing.T) {
	a := startAccountsPTY(t, 80, 24)
	a.send(t, "s")
	f := a.wait(t, func(f accountsInputFrame) bool { return f.selected == "claude:central" })
	f = a.key(t, f, "j")
	if f.harness != "codex" {
		t.Fatal("failed to select Codex")
	}
	release := make(chan struct{})
	defer close(release)
	a.blockStatus <- release
	a.send(t, "r")
	f = a.wait(t, func(f accountsInputFrame) bool { return strings.Contains(f.view, "Waiting for machine: status") })
	guard := func() {
		for _, raw := range []string{"\x1b[A", "\x1b[B", "j", "k", "\t", "\x1b[Z"} {
			f = a.key(t, f, raw)
			if f.harness != "codex" || f.selected != "codex:central" {
				t.Fatal("guarded navigation retargeted account")
			}
		}
	}
	guard()
	a.noMutations(t)
	// Wheel remains view-only while refresh is blocked.
	f = a.key(t, f, "\x1b[<65;50;10M")
	if f.scroll == 0 {
		t.Fatal("pending refresh blocked wheel scrolling")
	}
	a.rendered(t, f, "pending-wheel", 80, 24)
	release <- struct{}{}
	f = a.wait(t, func(f accountsInputFrame) bool { return !strings.Contains(f.view, "Waiting for machine:") })
	f = a.key(t, f, "e")
	if !f.confirming {
		t.Fatal("login did not require confirmation")
	}
	guard()
	a.noMutations(t)
	a.send(t, "\r")
	f = a.wait(t, func(f accountsInputFrame) bool { return f.active && !f.hidden })
	guard()
	select {
	case r := <-a.requests:
		if r.op != "login" || r.harness != "codex" || r.profile != "codex:central" {
			t.Fatalf("confirmed wrong target: %s/%s/%s", r.op, r.harness, r.profile)
		}
	case <-time.After(time.Second):
		t.Fatal("confirmed login missing")
	}
	f = a.key(t, f, "\x1b")
	if f.open {
		t.Fatal("Escape did not cancel ceremony and close")
	}
	if len(a.paneInput) != 0 {
		t.Fatal("guarded selection reached underlying pane")
	}
}
