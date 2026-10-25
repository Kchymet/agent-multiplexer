package nativetui

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"amux/internal/vterm"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	vt "github.com/charmbracelet/x/vt"
	"github.com/muesli/termenv"
)

func TestAccountRemainingTruth(t *testing.T) {
	now := time.Date(2030, 1, 2, 3, 4, 0, 0, time.UTC)
	for _, tc := range []struct {
		name            string
		used            float64
		missing, stale  bool
		observed, fresh time.Time
		state           string
		want            float64
		status          string
	}{
		{"unused", 0, false, false, now, now.Add(time.Minute), "available", 100, "fresh"},
		{"exhausted", 100, false, false, now, now.Add(time.Minute), "available", 0, "fresh"},
		{"missing", 0, true, false, now, now.Add(time.Minute), "available", 0, "unknown"},
		{"expired", 23, false, false, now.Add(-time.Hour), now, "available", 0, "stale"},
		{"future", 23, false, false, now.Add(time.Minute), now.Add(time.Hour), "available", 0, "stale"},
		{"unobserved", 23, false, false, time.Time{}, now.Add(time.Hour), "available", 0, "stale"},
		{"no freshness", 23, false, false, now, time.Time{}, "available", 0, "stale"},
		{"refresh failed", 23, false, true, now, now.Add(time.Hour), "available", 0, "stale"},
		{"reported stale", 23, false, false, now, now.Add(time.Hour), "stale", 0, "stale"},
		{"unavailable", 23, false, false, now, now.Add(time.Hour), "unavailable", 0, "unknown"},
		{"invalid", math.NaN(), false, false, now, now.Add(time.Hour), "available", 0, "unknown"},
		{"over limit", 101, false, false, now, now.Add(time.Hour), "available", 0, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := accountLimit{name: "five_hour", state: tc.state, usedPercent: &tc.used, observedAt: tc.observed, freshUntil: tc.fresh}
			if tc.missing {
				l.usedPercent = nil
			}
			got, state := accountRemaining(l, now, tc.stale)
			if got != tc.want || state != tc.status {
				t.Fatalf("got %g %s, want %g %s", got, state, tc.want, tc.status)
			}
			summary := ansi.Strip(accountUsageSummary(l, now, tc.stale))
			rail := accountRailBudget(accountProfile{limits: []accountLimit{l}}, now, tc.stale)
			for _, out := range []string{summary, rail} {
				if state != "fresh" && (strings.Contains(out, "%") || !strings.Contains(out, state)) {
					t.Fatalf("untruthful budget: %s", out)
				}
				if state == "fresh" && !strings.Contains(out, fmt.Sprintf("%g%%", tc.want)) {
					t.Fatalf("missing remaining budget: %s", out)
				}
			}
		})
	}
	p := accountProfile{authMode: "api", limits: []accountLimit{{name: "five_hour", state: "available"}}}
	if got := accountRailBudget(p, now, false); got != "API · not applicable" {
		t.Fatal(got)
	}
	if got := accountRailBudget(p, now, true); !strings.Contains(got, "stale") {
		t.Fatal(got)
	}
	if got := accountRailBudget(accountProfile{}, now, false); got != "budget unknown" {
		t.Fatal(got)
	}
	used := 72.0
	l := accountLimit{name: "custom", state: "available", usedPercent: &used, observedAt: now, freshUntil: now.Add(time.Minute)}
	if got := accountRailBudget(accountProfile{limits: []accountLimit{l}}, now, false); got != "limiting window 28%" {
		t.Fatal(got)
	}
	for name, want := range map[string]string{"primary (300 minutes)": "5h", "secondary (10080 minutes)": "7d", "seven_day_sonnet": "seven_day_sonnet"} {
		if got := accountWindowName(accountLimit{name: name}); got != want {
			t.Fatalf("%s became %s", name, got)
		}
	}
}

func TestOverlayCellsPreservesANSIAndWideGlyphPositions(t *testing.T) {
	base := "\x1b[31mA界BC界DEF\x1b[0m"
	got := overlayCells(base, "\x1b[36mBOX!\x1b[0m", 2, 0, 12, 1)
	if plain := ansi.Strip(got); plain != "A BOX! DEF  " {
		t.Fatalf("wide edges moved: %q", plain)
	}
	emu := vt.NewEmulator(12, 1)
	defer emu.Close()
	emu.Write([]byte(got))
	original := vt.NewEmulator(12, 1)
	defer original.Close()
	original.Write([]byte(base))
	for _, x := range []int{0, 7, 8, 9} {
		if !reflect.DeepEqual(emu.CellAt(x, 0), original.CellAt(x, 0)) {
			t.Fatalf("cell %d lost content/style: %+v vs %+v", x, emu.CellAt(x, 0), original.CellAt(x, 0))
		}
	}
	if reflect.DeepEqual(emu.CellAt(2, 0).Style, original.CellAt(0, 0).Style) {
		t.Fatal("modal inherited underlying style")
	}
}

func popupFixture(t *testing.T, width, height int) *model {
	t.Helper()
	m := groupedRail()
	m.w, m.h = width, height
	m.attached, m.tab, m.focus = "workgroup-child", tabTerminal, focusAgent
	term := vterm.NewRemote(m.mainWidth(), m.paneRows(), nil, nil)
	t.Cleanup(func() { term.Close() })
	m.terms = map[paneKey]*vterm.Terminal{{m.attached, m.tab}: term}
	var pane strings.Builder
	for row := 0; row < m.paneRows(); row++ {
		fmt.Fprintf(&pane, "\x1b[%d;1H\x1b[32mwork %02d · 界 %s\x1b[0m", row+1, row, strings.Repeat("·", max(0, m.mainWidth()-14)))
	}
	term.Feed([]byte(pane.String()))
	deadline := time.Now().Add(time.Second)
	for !strings.Contains(ansi.Strip(term.Render()), fmt.Sprintf("work %02d", m.paneRows()-1)) {
		if time.Now().After(deadline) {
			t.Fatal("terminal fixture did not render")
		}
		time.Sleep(time.Millisecond)
	}
	snapshot := accountFixture()
	now := time.Now()
	for i := range snapshot.profiles {
		used := float64(23 + 44*i)
		snapshot.profiles[i].limits = []accountLimit{
			{name: "five_hour", state: "available", usedPercent: &used, observedAt: now, freshUntil: now.Add(time.Hour), resetsAt: time.Date(2030, 1, 2, 3, 4, 0, 0, time.UTC)},
			{name: "seven_day", state: "available", usedPercent: &used, observedAt: now, freshUntil: now.Add(time.Hour), resetsAt: time.Date(2030, 1, 5, 3, 4, 0, 0, time.UTC)},
		}
	}
	m.accountOverview = &snapshot
	m.accountCall = func(context.Context, accountRequest) (accountResponse, error) {
		return accountResponse{snapshot: &snapshot}, nil
	}
	return m
}

func TestAccountsPopupPreservesPaneAndCapturesInput(t *testing.T) {
	for _, focus := range []focus{focusAgent, focusSidebar} {
		m := popupFixture(t, 120, 32)
		m.focus = focus
		pane := m.renderMain()
		original := m.cur()
		cursor, section, tab, attached := m.cursor, m.sectionCursor, m.tab, m.attached
		// Click the actual Accounts bar while the terminal has focus.
		barY := 1 + m.paneRows() - len(m.accountOverviewLines()) - len(m.railHints())
		_, cmd := m.handleMouse(tea.MouseMsg{X: 2, Y: barY, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
		m.Update(cmd())
		if m.renderMain() != pane {
			t.Fatal("popup replaced underlying pane")
		}
		view := ansi.Strip(m.View())
		if !strings.Contains(view, "work 00") || !strings.Contains(view, "╭") || !strings.Contains(view, "Accounts · remaining budget") {
			t.Fatal("missing overlay or live pane")
		}
		// A reporting terminal makes leaked mouse and keyboard input observable.
		input := make(chan []byte, 20)
		spy := vterm.NewRemote(m.mainWidth(), m.paneRows(), func(b []byte) { input <- append([]byte(nil), b...) }, nil)
		spy.Feed([]byte("\x1b[?1002h\x1b[?1006hMOUSE READY"))
		deadline := time.Now().Add(time.Second)
		for !strings.Contains(ansi.Strip(spy.Render()), "MOUSE READY") {
			if time.Now().After(deadline) {
				t.Fatal("mouse fixture not ready")
			}
			time.Sleep(time.Millisecond)
		}
		defer spy.Close()
		m.terms[paneKey{attached, tab}] = spy
		for _, k := range []tea.KeyMsg{key("z"), key("D"), {Type: tea.KeyRunes, Runes: []rune("x"), Paste: true}, {Type: tea.KeyRunes, Runes: []rune("h"), Alt: true}, {Type: tea.KeyRunes, Runes: []rune("1"), Alt: true}} {
			m.handleKey(k)
		}
		for _, ev := range []tea.MouseMsg{{X: 50, Y: 10, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress}, {X: 1, Y: 4, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress}, {X: 50, Y: 10, Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress}} {
			m.handleMouse(ev)
		}
		if len(input) != 0 || m.focus != focus || m.tab != tab || m.attached != attached || m.cursor != cursor || m.sectionCursor != section || m.form != nil || m.confirm != nil {
			t.Fatal("modal leaked input/focus to underlying pane")
		}
		if m.accounts.scroll == 0 {
			t.Fatal("wheel did not scroll popup")
		}
		m.terms[paneKey{attached, tab}] = original
		m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
		if m.renderMain() != pane {
			t.Fatal("Esc lost preserved terminal content")
		}
		if m.accounts != nil || m.focus != focus {
			t.Fatal("Esc did not restore original focus")
		}
	}
}

func TestAccountsPopupRenderEvidence(t *testing.T) {
	// Capture styled output too, even when go test's stdout is not a terminal.
	previous := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	defer lipgloss.SetColorProfile(previous)
	for _, size := range [][2]int{{80, 24}, {120, 32}, {52, 12}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			m := popupFixture(t, size[0], size[1])
			save := func(name string) {
				out := m.View()
				if ansi.StringWidth(out) == 0 || lipgloss.Height(out) != m.h {
					t.Fatal("invalid frame height")
				}
				for _, line := range strings.Split(out, "\n") {
					if ansi.StringWidth(line) != m.w {
						t.Fatalf("invalid line width %d", ansi.StringWidth(line))
					}
				}
				if dir := os.Getenv("AMUX_ACCOUNTS_RENDER_DIR"); dir != "" {
					plainLines := strings.Split(ansi.Strip(out), "\n")
					for i := range plainLines {
						plainLines[i] = strings.TrimRight(plainLines[i], " ")
					}
					for ext, data := range map[string]string{"txt": strings.Join(plainLines, "\n"), "ansi": out} {
						if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("popup-%dx%d-%s.%s", m.w, m.h, name, ext)), []byte(data+"\n"), 0600); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			// Exact-width frames include a padded help row for the evidence comparison.
			save("rail")
			before := m.View()
			cmd := m.openAccounts()
			m.Update(cmd())
			save("open")
			for _, provider := range []string{"Claude · subscription", "Codex · subscription"} {
				if !strings.Contains(ansi.Strip(m.View()), provider) {
					t.Fatalf("missing provider %s", provider)
				}
			}
			m.handleKey(key("tab"))
			for i := 0; i < 100; i++ {
				before := m.accounts.scroll
				m.handleKey(key("pgdown"))
				m.View()
				if m.accounts.scroll == before {
					break
				}
			}
			save("details")
			m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
			m.Update(m.refreshAccountOverview()())
			if m.View() != before {
				t.Fatal("close did not restore frame and focus")
			}
			if m.w == 120 {
				m.accountOverview.legacyDefaults["claude"] = true
				m.accountOverview.profiles[0].limits[0].freshUntil = time.Now().Add(-time.Minute)
				m.accountOverview.profiles[1].authMode = "api"
				save("stale-api-rail")
				m.Update(m.openAccounts()())
				save("stale-api")
			}
		})
	}
}

func TestShortRailKeepsAccountAndNavigationHints(t *testing.T) {
	for _, height := range []int{12, 24} {
		m := groupedRail()
		m.w, m.h = 80, height
		snapshot := accountFixture()
		m.accountOverview = &snapshot
		out := ansi.Strip(m.renderSidebar())
		for _, want := range []string{"Accounts", "s", "Claude", "Codex", "↵ open", "Space fold/unfold", "/ search archived"} {
			if !strings.Contains(out, want) {
				t.Fatalf("height %d missing %q:\n%s", height, want, out)
			}
		}
		if lipgloss.Height(out) != m.paneRows() {
			t.Fatal("rail overflow")
		}
	}
}

func TestAccountsNarrowConfirmationAndExtraWindowsScroll(t *testing.T) {
	m := accountModel()
	m.w, m.h = 52, 12
	m.accounts.snapshot.profiles[0].limits = append(m.accounts.snapshot.profiles[0].limits, accountLimit{name: "EXTRA WINDOW", state: "unavailable", reason: "EXTRA DETAIL"})
	var pages strings.Builder
	for i := 0; i < 100; i++ {
		before := m.accounts.scroll
		m.handleKey(key("pgdown"))
		pages.WriteString(ansi.Strip(m.renderAccounts()))
		if m.accounts.scroll == before {
			break
		}
	}
	if !strings.Contains(pages.String(), "EXTRA WINDOW") || !strings.Contains(pages.String(), "EXTRA DETAIL") {
		t.Fatal("extra windows are unreachable")
	}
	m.handleKey(key("e"))
	if m.accounts.confirm == nil || m.accounts.scroll != 0 {
		t.Fatal("confirmation did not start at top")
	}
	for i := 0; i < 100; i++ {
		before := m.accounts.scroll
		_, cmd := m.handleKey(key("pgdown"))
		if cmd != nil {
			t.Fatal("scroll dispatched confirmation")
		}
		m.renderAccounts()
		if m.accounts.scroll == before {
			break
		}
	}
	if m.accounts.scroll == 0 || !strings.Contains(ansi.Strip(m.renderAccounts()), "Enter: confirm") {
		t.Fatal("narrow confirmation cannot be fully read")
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if m.accounts == nil || m.accounts.confirm != nil || m.accounts.scroll != 0 {
		t.Fatal("confirmation back did not restore summary")
	}
}
