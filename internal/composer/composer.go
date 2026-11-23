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
	switch runtime {
	case RuntimeClaude:
		return readClaude(rowsOf(snap))
	case RuntimeCodex:
		return readCodex(rowsOf(snap))
	}
	return Reading{}
}

// Same reports whether the composer text equals want, ignoring whitespace:
// the runtimes wrap long lines and indent continuation rows, and amux types
// tabs as spaces and may neutralize a leading command character with a space,
// so only the non-blank characters are comparable.
func Same(composerText, want string) bool {
	return squash(composerText) == squash(want)
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
	n := 0
	for _, c := range r {
		switch c.text {
		case " ":
		case "─", "━", "═", "╭", "╮", "╰", "╯", "┌", "┐", "└", "┘":
			n++
		default:
			return false
		}
	}
	return n >= 4
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
func readClaude(rows []row) Reading {
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
		return Reading{Found: true, Text: strings.TrimRight(strings.Join(lines, "\n"), "\n ")}
	}
	return Reading{}
}

// readCodex finds Codex's composer: the lowest row starting with its prompt
// glyph ("›", or "▌" in older releases), followed by continuation rows indented
// under it, up to the first blank (or all-placeholder) row — the footer below
// the composer is dim, so it reads as blank here.
func readCodex(rows []row) Reading {
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
		return Reading{Found: true, Text: strings.TrimRight(strings.Join(lines, "\n"), "\n ")}
	}
	return Reading{}
}
