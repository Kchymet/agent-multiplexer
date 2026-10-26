// Package termselect computes text selection over a detached terminal viewport.
// It has no access to the child process, clipboard, or surrounding UI.
package termselect

import "strings"

// Cell is a displayed grapheme and its terminal width. Wide graphemes occupy a
// leading cell followed by empty continuation cells. Missing cells are blanks.
type Cell struct {
	Text  string
	Width int
}

// Point is a zero-based viewport cell coordinate.
type Point struct{ X, Y int }

// Span describes a highlighted row with an inclusive start and exclusive end.
type Span struct{ Y, Start, End int }

// Selection contains matching highlight geometry and plain copied text.
type Selection struct {
	Spans []Span
	Text  string
}

// Select selects both endpoint cells, clamped to the viewport, in reading
// order. Reversing the endpoints produces the same selection. Touching either
// half of a wide grapheme selects the whole glyph; continuation cells never
// contribute duplicate text. The caller decides whether a gesture is a drag:
// equal endpoints here deliberately select one whole cell.
//
// Each physical row is separated by a newline. Padding at the right viewport
// edge is trimmed; spaces at a partial-row endpoint are preserved. A glyph
// clipped by the viewport's right edge is treated as blank, never copied as
// text that was not fully visible. This function does not mutate the snapshot.
func Select(rows [][]Cell, cols int, anchor, head Point) Selection {
	if cols <= 0 || len(rows) == 0 {
		return Selection{}
	}
	clamp := func(p Point) Point {
		return Point{min(max(p.X, 0), cols-1), min(max(p.Y, 0), len(rows)-1)}
	}
	a, b := clamp(anchor), clamp(head)
	if a.Y > b.Y || a.Y == b.Y && a.X > b.X {
		a, b = b, a
	}
	selection := Selection{Spans: make([]Span, 0, b.Y-a.Y+1)}
	lines := make([]string, 0, b.Y-a.Y+1)
	for y := a.Y; y <= b.Y; y++ {
		start, end := 0, cols
		if y == a.Y {
			start = a.X
		}
		if y == b.Y {
			end = b.X + 1
		}
		var line strings.Builder
		for x := 0; x < cols; {
			text, width := " ", 1
			if x < len(rows[y]) {
				c := rows[y][x]
				if c.Width > 0 && c.Width <= cols-x && c.Text != "" {
					text, width = c.Text, c.Width
				}
			}
			if x < end && x+width > start {
				start = min(start, x)
				end = max(end, x+width)
				line.WriteString(text)
			}
			x += width
		}
		selection.Spans = append(selection.Spans, Span{y, start, end})
		text := line.String()
		if end == cols {
			text = strings.TrimRight(text, " ")
		}
		lines = append(lines, text)
	}
	selection.Text = strings.Join(lines, "\n")
	return selection
}
