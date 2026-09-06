package nativetui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// Text-field layout: a field's value may span several lines (a pasted prompt
// usually does), but the form modal has to stay inside the main pane. These
// helpers lay a value out in rows of a fixed width, find the row the cursor is
// on, and pick the window of rows to show.

// tabCells is how many cells a literal tab occupies when a field is drawn. The
// value keeps the tab; only the picture expands it (lipgloss would otherwise
// count it as zero-width and let the row overrun the box).
const tabCells = 4

// maxFieldRows caps how tall the active text field grows, even in a tall pane,
// so the modal stays a form rather than a full-screen editor.
const maxFieldRows = 10

func cellWidth(r rune) int {
	if r == '\t' {
		return tabCells
	}
	return lipgloss.Width(string(r))
}

// span is one visual row of a text field: the rune range [start, end) of the
// value it shows. A row ends at a newline (excluded from the range) or where the
// text wrapped to fit the width.
type span struct{ start, end int }

// wrapSpans lays r out in rows at most width cells wide, breaking at newlines
// and, for longer lines, after the last blank that fits (hard-breaking a word
// wider than a row). Every rune lands on exactly one row and an empty value or a
// trailing newline still yields an (empty) row, so cursor positions map to rows
// by range lookup.
func wrapSpans(r []rune, width int) []span {
	if width < 1 {
		width = 1
	}
	var out []span
	start := 0
	for {
		nl := start // end of the logical line
		for nl < len(r) && r[nl] != '\n' {
			nl++
		}
		for s := start; ; {
			e, cells, blank := s, 0, -1
			for e < nl {
				cw := cellWidth(r[e])
				if cells+cw > width && e > s {
					break
				}
				if isWordSpace(r[e]) {
					blank = e
				}
				cells += cw
				e++
			}
			if e < nl && blank >= s {
				e = blank + 1 // keep the blank on this row; the next word starts the next
			}
			out = append(out, span{s, e})
			if e >= nl {
				break
			}
			s = e
		}
		if nl >= len(r) {
			return out
		}
		start = nl + 1
	}
}

// cursorRow returns the row holding rune index c. A cursor sitting just past a
// row's last rune belongs to that row only when the row ends the logical line
// (end of value or a newline); after a wrap break it is the start of the next.
func cursorRow(spans []span, r []rune, c int) int {
	for i, sp := range spans {
		if c >= sp.start && c < sp.end {
			return i
		}
		if c == sp.end && (sp.end == len(r) || r[sp.end] == '\n') {
			return i
		}
	}
	return len(spans) - 1
}

// expandTabs renders a rune range as text, widening tabs to tabCells so the
// picture is as wide as wrapSpans measured it.
func expandTabs(r []rune) string {
	return strings.ReplaceAll(string(r), "\t", strings.Repeat(" ", tabCells))
}

// truncateCells cuts s to at most width cells, marking a cut with an ellipsis.
func truncateCells(s string, width int) string {
	if width < 1 {
		return ""
	}
	s = expandTabs([]rune(s))
	if lipgloss.Width(s) <= width {
		return s
	}
	return ansi.Truncate(s, width, "…")
}

// clampBlock hard-limits a rendered block to h rows of w cells. The form lays
// itself out to fit, so this only bites in degenerate panes — but a modal must
// never push the surrounding chrome around.
func clampBlock(s string, w, h int) string {
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	lines := strings.Split(s, "\n")
	if len(lines) > h {
		lines = lines[:h]
	}
	for i, l := range lines {
		lines[i] = ansi.Truncate(l, w, "")
	}
	return strings.Join(lines, "\n")
}
