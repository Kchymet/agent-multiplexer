// Package composer reads an interactive agent TUI's input box (its
// "composer") off a rendered terminal screen, so the daemon can tell whether a
// human has an unsent draft there before it types anything of its own.
//
// amux delivers wake-ups, goal nudges and remote steering by typing into the
// runtime's composer (see daemon/typed_delivery.go). Typing into a composer
// that already holds the user's half-written message joins the two, and the
// trailing Enter submits both as one prompt. The runtimes expose no API that
// reports the composer's content, so the only reliable signal amux has is the
// screen it already mirrors: this package finds the composer on that screen and
// returns the text in it.
//
// The parsers are deliberately conservative. A screen on which no composer is
// recognized reads as Found=false ("unknown"), never as empty, so a caller that
// must not mix with a draft can fail closed. Placeholder text — the dim/gray
// hint both runtimes draw in an empty composer — is not content.
package composer

import (
	"image/color"
	"strings"
	"unicode"

	uv "github.com/charmbracelet/ultraviolet"

	"amux/internal/vterm"
)

// Runtime names, matching agent.Canonical (kept as literals so this package
// does not import the agent registry).
const (
	RuntimeClaude = "claude"
	RuntimeCodex  = "codex"
)

// Reading is what a screen says about the composer.
type Reading struct {
	// Found reports that a composer was recognized on the screen. When false the
	// composer's content is unknown — the runtime may be showing a dialog, still
	// booting, or have changed its layout — and Text is empty.
	Found bool
	// Text is the composer's content with placeholder text and the runtime's own
	// chrome (prompt glyph, borders) removed. Rows are joined with "\n"; wrapped
	// rows are not re-joined, so compare with Same rather than ==.
	Text string
}

// Empty reports a recognized composer holding no text.
func (r Reading) Empty() bool { return r.Found && strings.TrimSpace(r.Text) == "" }

// Read parses the composer of the given runtime off a screen snapshot. An
// unsupported runtime reads as not found.
func Read(runtime string, snap vterm.Snapshot) Reading {
	r, _ := readAt(runtime, rowsOf(snap))
	return r
}

// readAt parses the composer and also returns its prompt row (-1 when none).
func readAt(runtime string, rows []row) (Reading, int) {
	switch runtime {
	case RuntimeClaude:
		return readClaude(rows)
	case RuntimeCodex:
		return readCodex(rows)
	}
	return Reading{}, -1
}

// State is what the input region of a pane says about whether amux may type.
type State int

const (
	// Unknown: no composer is recognized (booting, a repaint, a layout amux
	// does not know). Nothing may be typed: amux cannot tell what holds focus.
	Unknown State = iota
	// Free: the composer is recognized and holds no text.
	Free
	// Draft: the composer holds text (someone's unsent draft — or amux's own
	// earlier text, which only the caller can recognize).
	Draft
	// Dialog: a numbered selector (a permission prompt, Claude Code's auto-mode
	// opt-in, an AskUserQuestion, a Codex approval) owns the keyboard in place of
	// the composer. Typed text would answer it.
	Dialog
)

func (s State) String() string {
	switch s {
	case Free:
		return "free"
	case Draft:
		return "draft"
	case Dialog:
		return "dialog"
	}
	return "unknown"
}

// Pane is a pane's classification for typed delivery.
type Pane struct {
	State State
	// Text is the composer's content (Draft only).
	Text string
	// Busy reports a turn in progress ("esc to interrupt" on screen). It is
	// informational: a busy runtime takes typed text (Claude Code queues it,
	// Codex steers with it), so busy alone never refuses delivery.
	Busy bool
}

// Classify reads only the pane's input region: the composer block the reader
// recognizes (Claude Code's rule-framed prompt, Codex's "›" block), plus a
// selector drawn below or in place of it. Everything else on screen —
// transcript, tool output, spinner and status lines, queued-message previews,
// footers — is never read as composer content, so a busy pane classifies the
// same as an idle one.
func Classify(runtime string, snap vterm.Snapshot) Pane {
	rows := rowsOf(snap)
	r, y := readAt(runtime, rows)
	p := Pane{Busy: busy(rows), Text: r.Text}
	sel := selectorRow(rows)
	switch {
	case sel >= 0 && (y < 0 || sel > y):
		// A selector below the lowest composer, or with no composer at all:
		// the dialog has replaced (or covers) the input box.
		return Pane{State: Dialog, Busy: p.Busy}
	case sel >= 0 && sel == y && runtime == RuntimeCodex:
		// Codex draws its approval choices with the composer's own "›" glyph.
		return Pane{State: Dialog, Busy: p.Busy}
	case !r.Found:
		return Pane{State: Unknown, Busy: p.Busy}
	case r.Empty():
		p.State, p.Text = Free, ""
		return p
	}
	p.State = Draft
	return p
}

// busy reports a running-turn status line: both runtimes print "esc to
// interrupt" in their spinner/status row while a turn runs.
func busy(rows []row) bool {
	for _, r := range rows {
		if strings.Contains(strings.ToLower(r.plain()), "esc to interrupt") {
			return true
		}
	}
	return false
}

// selectorRow finds the lowest numbered selector — a focus glyph on an option
// "1." … "9." with another numbered option within the next rows — and returns
// its focused row, or -1. Both runtimes draw permission/approval prompts,
// Claude Code's auto-mode opt-in and AskUserQuestion this way.
func selectorRow(rows []row) int {
	for y := len(rows) - 1; y >= 0; y-- {
		x, ok := promptAt(rows[y], ">❯›")
		if !ok || !numbered(content(rows[y], x)) {
			continue
		}
		for k := y - 3; k <= y+3; k++ {
			if k == y || k < 0 || k >= len(rows) {
				continue
			}
			if numbered(strings.TrimLeft(rows[k].plain(), " │")) {
				return y
			}
		}
	}
	return -1
}

// numbered reports option text such as "2. No, and tell Claude what to do".
func numbered(s string) bool {
	s = strings.TrimLeft(s, " ")
	return len(s) >= 3 && s[0] >= '1' && s[0] <= '9' && s[1] == '.' && s[2] == ' '
}

// Echo is how a composer read after typing compares with what amux typed.
type Echo int

const (
	// EchoNone: the composer shows nothing (yet).
	EchoNone Echo = iota
	// EchoPartial: the composer shows a beginning of amux's text — the runtime
	// is still painting it.
	EchoPartial
	// EchoExact: the composer shows amux's text and nothing else but rendering
	// noise.
	EchoExact
	// EchoForeign: the composer shows something amux did not type.
	EchoForeign
)

// MatchTyped compares a composer's text, read after amux typed, with the text
// amux typed. Whitespace is ignored (wrapping, continuation indents), and so
// are glyphs a busy runtime repaints in and around its input box — spinner
// frames, ellipses, return-key hints — which are rendering noise, never
// someone's words. The caller must establish separately that nobody else
// typed; this only says what the screen shows.
func MatchTyped(composerText, typed string) Echo {
	a, t := denoise(squash(unescape(composerText))), denoise(squash(unescape(typed)))
	switch {
	case a == "":
		return EchoNone
	case a == t:
		return EchoExact
	case strings.HasPrefix(t, a):
		return EchoPartial
	}
	return EchoForeign
}

// noiseGlyphs are status/spinner glyphs a runtime may paint into the input
// region while a turn runs.
const noiseGlyphs = "✻✶✳✢✽✺·•…↵⏎⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏"

func denoise(s string) string {
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune(noiseGlyphs, r) {
			return -1
		}
		return r
	}, s)
}

// Same reports whether the composer text equals want, ignoring whitespace:
// the runtimes wrap long lines and indent continuation rows, and amux types
// tabs as spaces and may neutralize a leading command character with a space,
// so only the non-blank characters are comparable.
func Same(composerText, want string) bool {
	return squash(unescape(composerText)) == squash(unescape(want))
}

// unescape removes the backslash Claude Code renders at the end of a line
// typed as backslash+Enter (amux's TypedNewline; docs/steering.md rule 9), on
// either side of a comparison, so a multi-line typed steer compares equal to
// the text amux sent.
func unescape(s string) string {
	s = strings.ReplaceAll(s, "\\\n", "\n")
	return strings.TrimSuffix(s, "\\")
}

func squash(s string) string {
	var b strings.Builder
	for _, r := range s {
		if !unicode.IsSpace(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// cell is one screen cell reduced to what the parsers need.
type cell struct {
	text string
	// hint marks placeholder-styled text: faint (SGR 2), or a neutral gray
	// foreground. Both runtimes draw their empty-composer hint this way and
	// draw typed text in the default color.
	hint bool
	// cursor marks a reverse-video cell: Claude Code draws its own cursor that
	// way (over the first placeholder character when the composer is empty).
	cursor bool
}

type row []cell

// plain is the row's text with every cell kept.
func (r row) plain() string {
	var b strings.Builder
	for _, c := range r {
		b.WriteString(c.text)
	}
	return strings.TrimRight(b.String(), " ")
}

func rowsOf(snap vterm.Snapshot) []row {
	out := make([]row, len(snap.Cells))
	for y, line := range snap.Cells {
		r := make(row, 0, len(line))
		for _, c := range line {
			if c.Width == 0 && c.Content == "" {
				continue // the trailing half of a wide character
			}
			text := c.Content
			if text == "" || text == "\u00a0" {
				// Claude Code 2.1 draws its prompt as "❯" + NO-BREAK SPACE.
				text = " "
			}
			r = append(r, cell{
				text:   text,
				hint:   c.Style.Attrs&uv.AttrFaint != 0 || grayish(c.Style.Fg),
				cursor: c.Style.Attrs&uv.AttrReverse != 0,
			})
		}
		out[y] = r
	}
	return out
}

// grayish reports a neutral mid-gray foreground (ANSI bright black, a gray
// 256-color or truecolor value) — the color placeholder text is drawn in. The
// default foreground (nil), white and black are not gray.
func grayish(c color.Color) bool {
	if c == nil {
		return false
	}
	r, g, b, _ := c.RGBA()
	r, g, b = r>>8, g>>8, b>>8
	spread := max(r, g, b) - min(r, g, b)
	return spread <= 24 && r >= 0x40 && r <= 0xb8
}

// content extracts a composer row's text from x on: placeholder-styled cells
// are dropped, and so is a cursor cell that sits on placeholder text (Claude
// Code's empty composer renders `> ▮ry "…"`: the first hint character in
// reverse video, the rest gray). A cursor cell over real text is kept.
func content(r row, x int) string {
	var b strings.Builder
	for i := x; i < len(r); i++ {
		c := r[i]
		if c.hint {
			continue
		}
		if c.cursor && i+1 < len(r) && r[i+1].hint {
			continue
		}
		b.WriteString(c.text)
	}
	return strings.TrimRight(b.String(), " ")
}

// promptAt finds a prompt glyph at the start of a row (after leading blanks
// and an optional box border), returning the index of the first text cell
// after it.
func promptAt(r row, glyphs string) (int, bool) {
	i := 0
	for i < len(r) && r[i].text == " " {
		i++
	}
	if i < len(r) && r[i].text == "│" {
		i++
		for i < len(r) && r[i].text == " " {
			i++
		}
	}
	// The glyph itself may be drawn gray; only its shape matters.
	if i >= len(r) || !strings.Contains(glyphs, r[i].text) {
		return 0, false
	}
	i++
	if i < len(r) && r[i].text != " " && !r[i].cursor {
		return 0, false // ">foo" is text, not a prompt
	}
	if i < len(r) && r[i].text == " " {
		i++
	}
	return i, true
}

// isRule reports a horizontal rule or box edge: a row made only of box-drawing
// characters (at least a few of them), as Claude Code draws above and below its
// composer.
func isRule(r row) bool {
	n, other := 0, 0
	for _, c := range r {
		switch c.text {
		case " ":
		case "─", "━", "═", "╭", "╮", "╰", "╯", "┌", "┐", "└", "┘":
			n++
		default:
			other++
		}
	}
	// A long rule may carry a short label (a mode or session name drawn into
	// the frame); text with a few dashes in it is not a rule.
	return (other == 0 && n >= 4) || (n >= 20 && other*4 <= n)
}

// trimBorder drops a box's right border from row text.
func trimBorder(s string) string {
	s = strings.TrimRight(s, " ")
	s = strings.TrimSuffix(s, "│")
	return strings.TrimRight(s, " ")
}

// readClaude finds Claude Code's composer: a prompt row (">" or "❯") directly
// below a rule or box top, followed by continuation rows up to the next rule
// or box bottom. The lowest such block on the screen is the composer — earlier
// user messages in the transcript also start with ">" but are not framed.
func readClaude(rows []row) (Reading, int) {
	for y := len(rows) - 1; y >= 1; y-- {
		x, ok := promptAt(rows[y], ">❯")
		if !ok || !isRule(rows[y-1]) {
			continue
		}
		end := -1
		for k := y + 1; k < len(rows); k++ {
			if isRule(rows[k]) {
				end = k
				break
			}
		}
		if end < 0 {
			continue // no bottom edge: not a complete composer
		}
		lines := []string{trimBorder(content(rows[y], x))}
		for k := y + 1; k < end; k++ {
			line := content(rows[k], 0)
			line = strings.TrimLeft(line, " ")
			line = strings.TrimPrefix(line, "│")
			lines = append(lines, trimBorder(line))
		}
		return Reading{Found: true, Text: strings.TrimRight(strings.Join(lines, "\n"), "\n ")}, y
	}
	return Reading{}, -1
}

// readCodex finds Codex's composer: the lowest row starting with its prompt
// glyph ("›", or "▌" in older releases), followed by continuation rows indented
// under it, up to the first blank (or all-placeholder) row — the footer below
// the composer is dim, so it reads as blank here.
func readCodex(rows []row) (Reading, int) {
	for y := len(rows) - 1; y >= 0; y-- {
		x, ok := promptAt(rows[y], "›▌")
		if !ok {
			continue
		}
		lines := []string{content(rows[y], x)}
		for k := y + 1; k < len(rows); k++ {
			line := content(rows[k], 0)
			if strings.TrimSpace(line) == "" || !strings.HasPrefix(rows[k].plain(), "  ") && !strings.HasPrefix(rows[k].plain(), "▌") {
				break
			}
			lines = append(lines, strings.TrimLeft(strings.TrimPrefix(strings.TrimLeft(line, " "), "▌"), " "))
		}
		return Reading{Found: true, Text: strings.TrimRight(strings.Join(lines, "\n"), "\n ")}, y
	}
	return Reading{}, -1
}
