package nativetui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// Soft wrapping for agent text that must be read in full: question text,
// headers, option labels, descriptions and previews. Everything here is
// computed from the width it is handed on every render; nothing caches a
// width, so a terminal resize (tea.WindowSizeMsg) re-wraps on the next frame.
//
// Widths are display cells measured with charmbracelet/x/ansi, so wide
// characters, emoji and ANSI styling count correctly. Text is wrapped plain
// and styled per line afterwards, so a style never straddles a line break.

// minHangCells is the narrowest text column worth hanging under a prefix.
// Below it the text drops to its own lines under a short indent instead.
const minHangCells = 12

// cleanText sanitizes untrusted multi-line agent text for display: line
// breaks are kept (as \n), tabs expand to spaces, and every other control
// character (ESC included, so no agent escape sequence survives) is dropped.
// Trailing blank lines go.
func cleanText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r == '\r':
			return '\n'
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
			return -1
		}
		return r
	}, s)
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(expandTabs([]rune(l)), " ")
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

// wrapText soft-wraps untrusted agent text to width cells: word wrap at
// spaces, with hard breaks inside tokens longer than the width (URLs, paths,
// unspaced CJK text). Line breaks in the text are kept, and so is a line's
// leading indentation. It always returns at least one line, and no line is
// wider than width.
func wrapText(s string, width int) []string {
	width = max(1, width)
	var out []string
	for _, para := range strings.Split(cleanText(s), "\n") {
		out = append(out, wrapLine(para, width)...)
	}
	if len(out) == 0 {
		out = []string{""}
	}
	return out
}

// wrapLine greedily wraps one line of plain text. Runs of spaces are kept
// inside a line (previews may be laid out with them) and dropped at a break.
// A token wider than the width starts on the current line when minHangCells
// are left
// and continues hard-broken; hyphens are not break points, so a URL or path
// is cut only where the line is full.
func wrapLine(s string, width int) []string {
	var out []string
	var cur strings.Builder
	curW := 0
	flush := func() {
		out = append(out, strings.TrimRight(cur.String(), " "))
		cur.Reset()
		curW = 0
	}
	add := func(tok string, w int) { cur.WriteString(tok); curW += w }
	for _, tok := range splitSpaceRuns(s) {
		tw := ansi.StringWidth(tok)
		switch {
		case tok[0] == ' ':
			if curW == 0 && len(out) > 0 {
				continue // no leading spaces on a wrapped line
			}
			if curW+tw >= width {
				flush()
				continue
			}
			add(tok, tw)
		case curW+tw <= width:
			add(tok, tw)
		case tw <= width:
			flush()
			add(tok, tw)
		default: // longer than a line: fill this one, then hard-break
			if curW > 0 && width-curW < minHangCells {
				flush() // too little room left to be worth starting here
			}
			for tw > 0 {
				head := ansi.Truncate(tok, width-curW, "")
				hw := ansi.StringWidth(head)
				if hw == 0 { // no room left (or a wide rune at the edge)
					if curW == 0 { // a rune wider than the whole line
						head, hw = ansi.Truncate(tok, 2, ""), 2
					} else {
						flush()
						continue
					}
				}
				add(head, hw)
				tok = ansi.TruncateLeft(tok, hw, "")
				tw = ansi.StringWidth(tok)
				if tw > 0 {
					flush()
				}
			}
		}
	}
	if curW > 0 || len(out) == 0 {
		flush()
	}
	return out
}

// splitSpaceRuns splits s into alternating runs of spaces and non-spaces.
func splitSpaceRuns(s string) []string {
	var toks []string
	start := 0
	for i := 1; i <= len(s); i++ {
		if i == len(s) || (s[i] == ' ') != (s[start] == ' ') {
			toks = append(toks, s[start:i])
			start = i
		}
	}
	return toks
}

// wrapFlat is wrapText for one-line fields (labels, headers, titles): any
// line breaks in the text become spaces first.
func wrapFlat(s string, width int) []string {
	return wrapText(flat(s), width)
}

// hang lays text out after prefix with a hanging indent: the first line
// starts with prefix and every continuation line is indented to where the
// text began, so wrapped text stays aligned under itself. style renders each
// text line (nil for none); the prefix keeps its own styling. In a column too
// narrow to hang under the prefix, the prefix takes a line of its own and the
// text wraps beneath it at the prefix's leading indent plus two cells.
func hang(prefix, text string, width int, style func(string) string, flatten bool) []string {
	pw := ansi.StringWidth(prefix)
	wrap := wrapText
	if flatten {
		wrap = wrapFlat
	}
	render := func(s string) string {
		if style == nil || s == "" {
			return s
		}
		return style(s)
	}
	if width-pw < minHangCells && pw > 0 {
		lead := len(ansi.Strip(prefix)) - len(strings.TrimLeft(ansi.Strip(prefix), " "))
		indent := strings.Repeat(" ", min(lead+2, max(0, width-1)))
		out := []string{ansi.Truncate(prefix, width, "")}
		for _, l := range wrap(text, width-len(indent)) {
			out = append(out, indent+render(l))
		}
		return out
	}
	pad := strings.Repeat(" ", pw)
	lines := wrap(text, width-pw)
	for i, l := range lines {
		if i == 0 {
			lines[i] = prefix + render(l)
		} else if l == "" {
			lines[i] = ""
		} else {
			lines[i] = pad + render(l)
		}
	}
	return lines
}

// questionOption is one option row as the question views draw it. Mark is an
// optional selection glyph drawn before the number ("[ ]"/"[✔]" for
// multi-select in the answer view, docs/question-prompts.md §4); Preview
// is plain text shown under the option (the TUI shows previews as text).
type questionOption struct {
	Number      int
	Mark        string
	Label       string
	Description string
	Preview     string
}

// optionLines renders one option at width with its number, label,
// description and preview. indent is the option's left margin. The label,
// its description and its preview all hang under the label's first cell, so
// "  10. a long label" wraps with every continuation aligned under "a".
// Descriptions and previews are dimmed; preview line breaks are kept.
func optionLines(o questionOption, indent, width int, selected bool) []string {
	num := fmt.Sprintf("%d. ", o.Number)
	if o.Mark != "" {
		num = o.Mark + " " + num
	}
	prefix := strings.Repeat(" ", indent) + num
	label := func(s string) string { return s }
	if selected {
		label = func(s string) string { return selStyle.Render(s) }
	}
	out := hang(prefix, o.Label, width, label, true)
	under := strings.Repeat(" ", min(ansi.StringWidth(prefix), max(0, width-minHangCells)))
	dim := dimFn
	if d := strings.TrimSpace(o.Description); d != "" {
		out = append(out, hang(under, d, width, dim, false)...)
	}
	if p := cleanText(o.Preview); strings.TrimSpace(p) != "" {
		bar := under + dimStyle.Render("│ ")
		for _, l := range wrapText(p, width-ansi.StringWidth(under)-2) {
			out = append(out, bar+dim(l))
		}
	}
	return out
}

// dimFn is dimStyle.Render as a one-line styler for hang.
func dimFn(s string) string { return dimStyle.Render(s) }

// headerChip renders a question's header chip ("[Layout] "), or "".
func headerChip(header string) string {
	h := flat(header)
	if h == "" {
		return ""
	}
	return "[" + h + "] "
}

// scrollWindow returns the first line of an avail-line window onto total
// lines, starting from the desired top and moved as little as possible so it
// stays in range and shows the selected span [selStart, selEnd): all of the
// span when it fits, otherwise a window that lies wholly inside it (the user
// pages through a tall selection; it never scrolls out of view).
func scrollWindow(top, total, avail, selStart, selEnd int) int {
	avail = max(1, avail)
	top = min(max(0, top), max(0, total-avail))
	if selEnd <= selStart {
		return top
	}
	if selEnd-selStart <= avail {
		if selStart < top {
			top = selStart
		}
		if selEnd > top+avail {
			top = selEnd - avail
		}
		return top
	}
	return min(max(top, selStart), selEnd-avail)
}

// scrollMark is the dim position indicator for a scrolled list ("" when
// everything fits): which lines show, of how many (PgUp/PgDn page; the
// hint bar names the keys).
func scrollMark(top, shown, total int) string {
	if total <= shown {
		return ""
	}
	return dimStyle.Render(fmt.Sprintf(" · ↕%d–%d/%d", top+1, top+shown, total))
}

// fitLine pads or cuts a rendered line to exactly width cells; wrapped lines
// already fit, so this only guards the frame.
func fitLine(s string, width int) string {
	if lipgloss.Width(s) > width {
		return ansi.Truncate(s, width, "")
	}
	return s
}
