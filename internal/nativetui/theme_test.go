package nativetui

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"amux/internal/core"
	"amux/internal/store"
)

// themeForTest renders with mode on its real color profile, then restores the
// suite's default (styles built, profile Ascii as under `go test`).
func themeForTest(t *testing.T, mode themeMode) {
	t.Helper()
	prev := lipgloss.ColorProfile()
	setTheme(mode, false)
	t.Cleanup(func() {
		applyTheme(theme256)
		lipgloss.SetColorProfile(prev)
	})
}

func asciiForTest(t *testing.T) {
	t.Helper()
	asciiGlyphs = true
	t.Cleanup(func() { asciiGlyphs = false })
}

// switchboardRail is a rail with every state, every row kind, and a folded
// workgroup hiding a waiting member.
func switchboardRail(w, h int) *model {
	wg := core.SectionWorkgroups
	m := &model{w: w, h: h, cursor: 2, sessions: []core.Session{
		{ID: "console", Title: "amux console", Mode: store.ModeConsole, Role: store.RoleConsole, State: core.StateIdle, Status: "idle · amux-wide"},
		{ID: "wg-pay", Title: "payments", IsRoot: true, Role: store.RoleCoordinator, Section: wg, State: core.StateWaiting, Status: "running · 4 agents"},
		{ID: "a1f3", Title: "idempotency keys", RootID: "wg-pay", Section: wg, State: core.StateWaiting, Status: "waiting · api"},
		{ID: "b2c4", Title: "ledger migration", RootID: "wg-pay", Section: wg, State: core.StateRunning, Status: "running · api · PR #212 CI✓"},
		{ID: "c9d0", Title: "refund webhooks", RootID: "wg-pay", Section: wg, State: core.StateReady, Status: "ready · api"},
		{ID: "d7e1", Title: "決済フローの国際化対応", RootID: "wg-pay", Section: wg, State: core.StateUnknown, Status: "unknown · web"},
		{ID: "wg-docs", Title: "docs refresh", IsRoot: true, Role: store.RoleCoordinator, Section: wg, State: core.StateWaiting, Status: "ready · 2 agents"},
		{ID: "e5f6", Title: "api reference", RootID: "wg-docs", Section: wg, State: core.StateWaiting, Status: "waiting · docs"},
		{ID: "f0a1", Title: "tutorials", RootID: "wg-docs", Section: wg, State: core.StateIdle, Status: "idle · docs"},
		{ID: "wg-infra", Title: "infra", IsRoot: true, Role: store.RoleCoordinator, Section: wg, State: core.StateRunning, Status: "running · 0 agents"},
		{ID: "acme/api", Title: "acme/api", Kind: "repo", Role: store.RoleRepo, Section: core.SectionRepos, State: core.StateReady, Status: "ready · repo home"},
		{ID: "ext1", Title: "external-session", Mode: "external", Section: core.SectionDetached, State: core.StateRunning, Status: "running · untracked"},
	}}
	m.collapsed = map[railGroup]bool{{section: wg, id: "wg-docs"}: true}
	m.attached = "b2c4"
	return m
}

func TestStateGlyphsAreDistinctWithAndWithoutUnicode(t *testing.T) {
	states := []string{core.StateWaiting, core.StateRunning, core.StateReady, core.StateIdle, core.StateUnknown}
	seen, folded := map[string]string{}, map[string]string{}
	for _, st := range states {
		g := stateGlyph(st)
		if prev, dup := seen[g]; dup {
			t.Errorf("%s and %s share the glyph %q", prev, st, g)
		}
		seen[g] = st
		a := asciiFolds.Replace(g)
		if prev, dup := folded[a]; dup || len(a) != 1 || a[0] >= 0x80 {
			t.Errorf("%s folds to %q (shared with %q)", st, a, prev)
		}
		folded[a] = st
	}
	// A session amux did not start must not read as one waiting on the user.
	if glyph(core.Session{Mode: "external", State: core.StateWaiting}) == glyphWaiting {
		t.Error("external rows collide with the waiting glyph")
	}
}

func TestStatusWordIsItsOwnState(t *testing.T) {
	m := switchboardRail(120, 40)
	out := ansi.Strip(m.renderSidebar())
	for _, want := range []string{
		glyphWaiting + " idempotency keys", "a1f3 ·  waiting  · api",
		glyphRunning + " ledger migration", "b2c4 · running",
		glyphReady + " refund webhooks", "c9d0 · ready",
		glyphUnknown, "d7e1 · unknown · web",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rail missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "d7e1 · running") {
		t.Errorf("an unknown session is labelled running:\n%s", out)
	}
}

// A folded workgroup must still say that members wait on the user.
func TestFoldedWorkgroupSurfacesWaitingMembers(t *testing.T) {
	m := switchboardRail(120, 40)
	out := ansi.Strip(m.renderSidebar())
	if strings.Contains(out, "api reference") {
		t.Fatalf("fixture workgroup should be folded:\n%s", out)
	}
	if !strings.Contains(out, glyphWaiting+" 1 waiting  ready") {
		t.Errorf("folded workgroup hides its waiting member:\n%s", out)
	}
}

func TestAttentionSummary(t *testing.T) {
	m := switchboardRail(120, 40)
	sum := m.attentionSummary()
	if len(sum) != 1 {
		t.Fatalf("summary = %q, want one line", sum)
	}
	got := ansi.Strip(sum[0])
	want := fmt.Sprintf(" %s 2 waiting  %s 1  %s 0", glyphWaiting, glyphRunning, glyphReady)
	if strings.TrimRight(got, " ") != want || lipgloss.Width(sum[0]) != sidebarWidth {
		t.Errorf("summary = %q (width %d), want %q", got, lipgloss.Width(sum[0]), want)
	}
	if !strings.Contains(ansi.Strip(m.renderSidebar()), want) {
		t.Error("rail does not show the summary")
	}

	// The accounts bar is still found by counting up from the bottom.
	snapshot := accountFixture()
	m.accountOverview = &snapshot
	lines := strings.Split(ansi.Strip(m.renderSidebar()), "\n")
	barY := 1 + m.paneRows() - len(m.accountOverviewLines()) - len(m.railHints())
	if !strings.Contains(lines[barY-1], "Accounts") || !strings.Contains(lines[barY-2], "waiting") {
		t.Errorf("summary moved the accounts bar: row %d = %q, above = %q", barY, lines[barY-1], lines[barY-2])
	}
	_, cmd := m.handleMouse(tea.MouseMsg{X: 2, Y: barY, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if cmd == nil {
		t.Error("click on the accounts bar no longer opens Accounts")
	}

	// A short rail keeps its list and hints rather than the summary.
	m.h = 14
	if out := ansi.Strip(m.renderSidebar()); strings.Contains(out, "2 waiting") || !strings.Contains(out, "/ search archived") {
		t.Errorf("short rail should drop the summary, not the hints:\n%s", out)
	}
	// No workgroups, nothing to summarize.
	if got := (&model{w: 120, h: 40}).attentionSummary(); got != nil {
		t.Errorf("empty rail summary = %q", got)
	}
}

// An unknown workgroup is reported as unknown, never folded into running.
func TestAttentionSummaryKeepsUnknownOutOfRunning(t *testing.T) {
	m := switchboardRail(120, 40)
	turned := false
	for i := range m.sessions {
		s := &m.sessions[i]
		if s.IsRoot && s.Section == core.SectionWorkgroups && s.State == core.StateRunning {
			s.State = core.StateUnknown
			turned = true
		}
	}
	if !turned {
		t.Fatal("fixture has no running workgroup to turn unknown")
	}
	got := strings.TrimRight(ansi.Strip(m.attentionSummary()[0]), " ")
	// The fourth figure does not fit 26 columns beside the word, so the line sheds "waiting"
	// (the chip and its glyph still mark attention) rather than a number.
	want := fmt.Sprintf(" %s 2  %s 0  %s 0  %s 1", glyphWaiting, glyphRunning, glyphReady, glyphUnknown)
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	if w := lipgloss.Width(m.attentionSummary()[0]); w != sidebarWidth {
		t.Errorf("summary width = %d, want %d", w, sidebarWidth)
	}
}

// The summary gives up words, zero categories and spacing before it gives up a
// number: every non-zero count survives whole at the full and the compact rail,
// including multi-digit ones.
func TestSummaryLineNeverClipsACount(t *testing.T) {
	for _, tc := range []struct {
		waiting, running, ready, unknown, width int
		want                                    string
	}{
		{2, 1, 0, 0, sidebarWidth, " ◆ 2 waiting  ◐ 1  ● 0"},
		{2, 0, 0, 1, sidebarWidth, " ◆ 2  ◐ 0  ● 0  ◌ 1"},
		{12, 10, 11, 13, sidebarWidth, " ◆ 12  ◐ 10  ● 11  ◌ 13"},
		{2, 1, 0, 0, compactRailWidth, " ◆ 2  ◐ 1  ● 0"},
		{2, 0, 0, 1, compactRailWidth, " ◆ 2  ◌ 1"},
		{2, 3, 4, 1, compactRailWidth, " ◆ 2 ◐ 3 ● 4 ◌ 1"},
		{12, 10, 0, 13, compactRailWidth, " ◆ 12  ◐ 10  ◌ 13"},
		{12, 10, 11, 13, compactRailWidth, " ◆12 ◐10 ●11 ◌13"},
		{120, 100, 110, 130, compactRailWidth, " ◆120 ◐100●110◌130"},
	} {
		line := summaryLine(tc.waiting, tc.running, tc.ready, tc.unknown, tc.width)
		got := strings.TrimRight(ansi.Strip(line), " ")
		if got != tc.want {
			t.Errorf("summaryLine(%d,%d,%d,%d,%d) = %q, want %q", tc.waiting, tc.running, tc.ready, tc.unknown, tc.width, got, tc.want)
		}
		if w := lipgloss.Width(line); w != tc.width {
			t.Errorf("summaryLine(...,%d) is %d cells: %q", tc.width, w, got)
		}
		for _, n := range []int{tc.waiting, tc.running, tc.ready, tc.unknown} {
			if n > 0 && !strings.Contains(got, strconv.Itoa(n)) {
				t.Errorf("non-zero count %d missing from %q", n, got)
			}
		}
	}
}

func TestTruncateMeasuresCells(t *testing.T) {
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{
		{"abcdef", 6, "abcdef"},
		{"abcdef", 4, "abc…"},
		{"abcdef", 1, "…"},
		{"abcdef", 0, ""},
		{"決済フロー", 10, "決済フロー"},
		{"決済フロー", 7, "決済フ…"},
		{"決済フロー", 6, "決済…"}, // a wide glyph never straddles the edge
	} {
		got := truncate(tc.in, tc.n)
		if got != tc.want || ansi.StringWidth(got) > tc.n {
			t.Errorf("truncate(%q, %d) = %q (%d cells), want %q", tc.in, tc.n, got, ansi.StringWidth(got), tc.want)
		}
	}
	// Every rail line fits the rail, wide titles included.
	for _, w := range []int{120, 60} {
		compactRailForTest(t)
		m := switchboardRail(w, 30)
		for i, line := range strings.Split(m.renderSidebar(), "\n") {
			if got := ansi.StringWidth(line); got != m.railWidth() {
				t.Errorf("width %d: rail line %d is %d cells, want %d: %q", w, i, got, m.railWidth(), ansi.Strip(line))
			}
		}
	}
}

func TestDetectTheme(t *testing.T) {
	for _, tc := range []struct {
		name      string
		env       map[string]string
		cap       termenv.Profile
		want      themeMode
		wantPlain bool
	}{
		{"truecolor terminal", nil, termenv.TrueColor, themeTrueColor, false},
		{"256 terminal", nil, termenv.ANSI256, theme256, false},
		{"16 color terminal", nil, termenv.ANSI, theme16, false},
		{"dumb terminal", nil, termenv.Ascii, themeNone, true},
		{"NO_COLOR keeps attributes", map[string]string{"NO_COLOR": "1"}, termenv.TrueColor, themeNone, false},
		{"empty NO_COLOR is unset", map[string]string{"NO_COLOR": ""}, termenv.ANSI256, theme256, false},
		{"NO_COLOR on a dumb terminal", map[string]string{"NO_COLOR": "1"}, termenv.Ascii, themeNone, true},
		{"override beats detection", map[string]string{"AMUX_COLOR": "16"}, termenv.TrueColor, theme16, false},
		{"override beats NO_COLOR", map[string]string{"AMUX_COLOR": "256", "NO_COLOR": "1"}, termenv.Ascii, theme256, false},
		{"override none", map[string]string{"AMUX_COLOR": "none"}, termenv.TrueColor, themeNone, false},
		{"unknown override is ignored", map[string]string{"AMUX_COLOR": "mauve"}, termenv.ANSI, theme16, false},
	} {
		mode, plain := detectTheme(func(k string) string { return tc.env[k] }, tc.cap)
		if mode != tc.want || plain != tc.wantPlain {
			t.Errorf("%s: got %v plain=%v, want %v plain=%v", tc.name, mode, plain, tc.want, tc.wantPlain)
		}
	}
}

func TestDetectASCII(t *testing.T) {
	for _, tc := range []struct {
		env  map[string]string
		want bool
	}{
		{nil, false},
		{map[string]string{"LANG": "en_US.UTF-8"}, false},
		{map[string]string{"LANG": "C.utf8"}, false},
		{map[string]string{"LANG": "en_US.ISO-8859-1"}, true},
		{map[string]string{"LC_ALL": "C", "LANG": "en_US.UTF-8"}, true},
		{map[string]string{"TERM": "linux", "LANG": "en_US.UTF-8"}, true},
		{map[string]string{"AMUX_ASCII": "1", "LANG": "en_US.UTF-8"}, true},
		{map[string]string{"AMUX_ASCII": "0", "LANG": "C", "TERM": "linux"}, false},
	} {
		if got := detectASCII(func(k string) string { return tc.env[k] }); got != tc.want {
			t.Errorf("detectASCII(%v) = %v, want %v", tc.env, got, tc.want)
		}
	}
}

var (
	sgrColor = regexp.MustCompile(`\x1b\[[0-9;]*m`)
	// A color parameter: 30–37/90–97 fg, 40–47/100–107 bg, or 38/48 extended.
	colorParam = regexp.MustCompile(`^(3[0-8]|4[0-8]|9[0-7]|10[0-7])$`)
)

// colorParams returns the color-setting SGR parameters used in s, with the
// arguments of 38/48 folded into one entry ("48;5;220").
func colorParams(s string) map[string]bool {
	out := map[string]bool{}
	for _, seq := range sgrColor.FindAllString(s, -1) {
		ps := strings.Split(strings.TrimSuffix(strings.TrimPrefix(seq, "\x1b["), "m"), ";")
		for i := 0; i < len(ps); i++ {
			if !colorParam.MatchString(ps[i]) {
				continue
			}
			if (ps[i] == "38" || ps[i] == "48") && i+1 < len(ps) {
				n := 2 // ;5;N
				if ps[i+1] == "2" {
					n = 4 // ;2;R;G;B
				}
				end := min(len(ps), i+1+n)
				out[strings.Join(ps[i:end], ";")] = true
				i = end - 1
				continue
			}
			out[ps[i]] = true
		}
	}
	return out
}

func TestThemeColorProfiles(t *testing.T) {
	for _, tc := range []struct {
		mode             themeMode
		attnFill, focus  string // the yellow chip's background; the running/focus foreground
		allow            func(p string) bool
		wantAnyColor     bool
		selectionReverse bool
	}{
		{themeTrueColor, "48;2;255;195;0", "38;2;125;176;255", func(p string) bool { return strings.HasPrefix(p, "38;2;") || strings.HasPrefix(p, "48;2;") }, true, false},
		{theme256, "48;5;220", "38;5;111", func(p string) bool { return strings.HasPrefix(p, "38;5;") || strings.HasPrefix(p, "48;5;") }, true, false},
		{theme16, "103", "94", func(p string) bool { return !strings.Contains(p, ";") }, true, true},
		{themeNone, "", "", func(string) bool { return false }, false, true},
	} {
		t.Run(tc.mode.String(), func(t *testing.T) {
			themeForTest(t, tc.mode)
			m := switchboardRail(120, 40)
			m.focus = focusSidebar
			view := m.view()
			params := colorParams(view)
			if (len(params) > 0) != tc.wantAnyColor {
				t.Fatalf("color parameters = %v, want any=%v", params, tc.wantAnyColor)
			}
			for p := range params {
				if !tc.allow(p) {
					t.Errorf("%s mode emitted a color outside its depth: %q", tc.mode, p)
				}
			}
			for _, want := range []string{tc.attnFill, tc.focus} {
				if want != "" && !params[want] {
					t.Errorf("%s mode missing %q; have %v", tc.mode, want, params)
				}
			}
			// Yellow is a fill for attention only: it backs the waiting chips
			// and nothing else.
			if tc.attnFill != "" {
				for _, line := range strings.Split(view, "\n") {
					if strings.Contains(line, tc.attnFill) && !strings.Contains(ansi.Strip(line), "waiting") {
						t.Errorf("attention fill on a line with nothing waiting: %q", ansi.Strip(line))
					}
				}
			}
			// Selection and attention never depend on color: bold everywhere,
			// reverse video where no fill can be trusted.
			row := m.renderRow(m.cursor, m.sessions[m.cursor])
			first := strings.SplitN(row, "\n", 2)[0]
			if !strings.Contains(first, "\x1b[1") {
				t.Errorf("selected row is not bold: %q", first)
			}
			if hasReverse(first) != tc.selectionReverse {
				t.Errorf("selected row reverse = %v, want %v: %q", hasReverse(first), tc.selectionReverse, first)
			}
			if tc.mode == themeNone && !hasReverse(m.attentionSummary()[0]) {
				t.Errorf("no-color attention chip has no reverse video: %q", m.attentionSummary()[0])
			}
			// The words and shapes are the same at every depth.
			plain := ansi.Strip(view)
			for _, want := range []string{glyphWaiting, "waiting", glyphRunning, "running", glyphUnknown, "unknown", glyphOpen, glyphCursor} {
				if !strings.Contains(plain, want) {
					t.Errorf("%s mode lost %q", tc.mode, want)
				}
			}
		})
	}
}

func hasReverse(s string) bool {
	for _, seq := range sgrColor.FindAllString(s, -1) {
		for _, p := range strings.Split(strings.TrimSuffix(strings.TrimPrefix(seq, "\x1b["), "m"), ";") {
			if p == "7" {
				return true
			}
		}
	}
	return false
}

func TestASCIIFallback(t *testing.T) {
	unicode := switchboardRail(120, 30)
	uni := ansi.Strip(unicode.view())
	asciiForTest(t)
	m := switchboardRail(120, 30)
	snapshot := accountFixture()
	m.accountOverview = &snapshot
	// Chrome that shares a frame with the rail: the form and the confirm box.
	for name, open := range map[string]func(){
		"rail":    func() {},
		"confirm": func() { m.confirm = &confirmState{message: "Permanently delete agent x?"} },
		"picker":  func() { m.confirm, m.picker = nil, newRepoPicker("Repos · x", m.sessions, nil) },
	} {
		open()
		out := ansi.Strip(m.view())
		for i, line := range strings.Split(out, "\n") {
			for _, r := range line {
				// The fixture's own CJK title is content, not chrome.
				if r >= 0x80 && !strings.ContainsRune("決済フローの国際化対応", r) {
					t.Fatalf("%s: non-ASCII %q in ASCII mode, line %d: %q", name, r, i, line)
				}
			}
			if w := ansi.StringWidth(line); w != m.w {
				t.Errorf("%s: line %d is %d cells, want %d: %q", name, i, w, m.w, line)
			}
		}
	}
	m.picker = nil
	out := ansi.Strip(m.view())
	for _, want := range []string{"! idempotency keys", "~ ledger migration", "* refund webhooks", "? ", "v payments", "> docs refresh", "Enter open", "a1f3 .  waiting  . api"} {
		if !strings.Contains(out, want) {
			t.Errorf("ASCII rail missing %q:\n%s", want, out)
		}
	}
	// Single-cell folds leave the rail's columns where they were.
	uniLines, asciiLines := strings.Split(uni, "\n"), strings.Split(ansi.Strip(switchboardRail(120, 30).view()), "\n")
	for _, i := range []int{3, 4, 5} { // payments' first member rows
		if a, b := ansi.StringWidth(uniLines[i]), ansi.StringWidth(asciiLines[i]); a != b {
			t.Errorf("line %d: unicode %d cells, ASCII %d", i, a, b)
		}
	}
}

// The agent's own output is never rewritten, whatever amux does to its chrome.
// The daemon's PR check markers (prwatch: CI✓ / CI✗ / CI…) arrive inside a
// session's status text; ASCII mode must fold them too. The statuses are short
// so the markers are not truncated away before the assertion can see them.
func TestASCIIFallbackFoldsPRCheckMarkers(t *testing.T) {
	asciiForTest(t)
	m := switchboardRail(120, 30)
	marks := map[string]string{"idempotency keys": "CI✓", "ledger migration": "CI✗", "refund webhooks": "CI…"}
	for i := range m.sessions {
		if mark, ok := marks[m.sessions[i].Title]; ok {
			m.sessions[i].Status = mark
			delete(marks, m.sessions[i].Title)
		}
	}
	if len(marks) != 0 {
		t.Fatalf("fixture rows missing: %v", marks)
	}
	out := ansi.Strip(m.view())
	for _, want := range []string{"CI+", "CIx", "CI."} {
		if !strings.Contains(out, want) {
			t.Errorf("ASCII rail missing %q:\n%s", want, out)
		}
	}
	for i, line := range strings.Split(out, "\n") {
		for _, r := range line {
			if r >= 0x80 && !strings.ContainsRune("決済フローの国際化対応", r) {
				t.Fatalf("non-ASCII %q in ASCII mode, line %d: %q", r, i, line)
			}
		}
		if w := ansi.StringWidth(line); w != m.w {
			t.Errorf("line %d is %d cells, want %d: %q", i, w, m.w, line)
		}
	}
}

func TestASCIIFallbackLeavesPaneAlone(t *testing.T) {
	asciiForTest(t)
	m, term, _ := scrollModel(t, 30, 6)
	feedPane(t, term, "● agent ─ output … 界\r\n")
	out := ansi.Strip(m.view())
	if !strings.Contains(out, "● agent ─ output … 界") {
		t.Errorf("pane content was folded:\n%s", out)
	}
	if strings.Contains(strings.SplitN(out, "\n", 2)[0], "─") {
		t.Errorf("border was not folded:\n%s", out)
	}
}

func TestCompactRail(t *testing.T) {
	compactRailForTest(t)
	for _, tc := range []struct{ w, rail int }{{120, sidebarWidth}, {80, sidebarWidth}, {72, sidebarWidth}, {71, compactRailWidth}, {40, compactRailWidth}} {
		m := switchboardRail(tc.w, 20)
		if m.railWidth() != tc.rail || m.mainWidth() != tc.w-tc.rail-1 {
			t.Errorf("width %d: rail %d main %d, want rail %d main %d", tc.w, m.railWidth(), m.mainWidth(), tc.rail, tc.w-tc.rail-1)
		}
		for i, line := range strings.Split(m.view(), "\n") {
			if got := ansi.StringWidth(line); got != tc.w {
				t.Errorf("width %d: view line %d is %d cells", tc.w, i, got)
			}
		}
	}

	// The compact rail still names each state and keeps its hints.
	m := switchboardRail(60, 30)
	out := ansi.Strip(m.renderSidebar())
	for _, want := range []string{glyphWaiting + " idempotency", "waiting", glyphWaiting + " 2 ", "Space fold/unfold"} {
		if !strings.Contains(out, want) {
			t.Errorf("compact rail missing %q:\n%s", want, out)
		}
	}

	// Everything that crosses the divider follows the rail's width: the pane is
	// sized to the wider main area, its cursor lands in it, and a click on its
	// first column reaches the app as column 1.
	pane, term, input := scrollModel(t, 60-compactRailWidth-1, 6)
	pane.w = 60
	if pane.mainWidth() != 41 {
		t.Fatalf("main width = %d, want 41", pane.mainWidth())
	}
	feedPane(t, term, "\x1b[?1000h\x1b[?1006hab")
	if cur := pane.hardwareCursor(); !cur.visible || cur.x != compactRailWidth+1+2 {
		t.Errorf("hardware cursor = %+v, want x=%d", cur, compactRailWidth+1+2)
	}
	// The divider is not the pane; one column right is the app's column 1.
	pane.handleMouse(tea.MouseMsg{X: compactRailWidth, Y: 1, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if got := input(); got != "" {
		t.Errorf("click on the divider reached the app: %q", got)
	}
	pane.handleMouse(tea.MouseMsg{X: compactRailWidth + 1, Y: 1, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	waitPane(t, "press at the pane's first column", func() bool { return strings.Contains(input(), "\x1b[<0;1;1M") })
}

// TestSwitchboardEvidence writes the captures under docs/evidence/switchboard-tui
// when AMUX_SWITCHBOARD_RENDER_DIR is set (see that directory's README).
func TestSwitchboardEvidence(t *testing.T) {
	dir := os.Getenv("AMUX_SWITCHBOARD_RENDER_DIR")
	if dir == "" {
		t.Skip("set AMUX_SWITCHBOARD_RENDER_DIR to write captures")
	}
	compactRailForTest(t)
	save := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fixture := func(w, h int) *model {
		m := switchboardRail(w, h)
		snapshot := accountFixture()
		m.accountOverview = &snapshot
		return m
	}
	for _, sz := range [][2]int{{80, 24}, {120, 32}} {
		for _, mode := range []themeMode{themeTrueColor, theme256, theme16, themeNone} {
			themeForTest(t, mode)
			name := fmt.Sprintf("rail-%dx%d-%s", sz[0], sz[1], mode)
			view := fixture(sz[0], sz[1]).view()
			save(name+".ansi", view)
			if mode == themeNone {
				save(fmt.Sprintf("rail-%dx%d.txt", sz[0], sz[1]), ansi.Strip(view))
			}
		}
		func() {
			asciiGlyphs = true
			defer func() { asciiGlyphs = false }()
			save(fmt.Sprintf("rail-%dx%d-ascii.txt", sz[0], sz[1]), ansi.Strip(fixture(sz[0], sz[1]).view()))
		}()
	}
	themeForTest(t, theme256)
	compact := fixture(60, 24)
	save("compact-60x24-256.ansi", compact.view())
	save("compact-60x24.txt", ansi.Strip(compact.view()))
	form := fixture(100, 28)
	form.openNewWorkgroupForm()
	save("form-100x28-256.ansi", form.view())
	save("form-100x28.txt", ansi.Strip(form.view()))
}
