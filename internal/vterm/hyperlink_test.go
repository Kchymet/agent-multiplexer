package vterm

import (
	"strings"
	"testing"
)

// OSC 8 is "ESC ] 8 ; params ; URI ST". The mirror does not pass the child's
// bytes through: it parses them into emulator cells and re-encodes every link
// from those cells (Render), so what the outer terminal sees is only as good as
// that round trip. The previously pinned x/vt stored the two fields swapped and
// re-emitted "ESC ] 8 ; <uri> ; <params> BEL": a link with params became one
// whose URI is "id=…" (iTerm2 hands that to LaunchServices on Cmd-click: "The
// application can't be opened. -50"), and one without params got an empty URI,
// which ends the link, so it wasn't clickable at all.
//
// Each case feeds the child's bytes and requires every rendered row to carry
// exactly the expected links, params first and URI last, and nothing else.
// Re-encoding always terminates with BEL, whichever terminator the child used.
func TestHyperlinksRoundTrip(t *testing.T) {
	const (
		web  = "https://example.com/a/b?x=1&y=two#frag"
		file = "file:///Users/me/src/amux/main.go"
		host = "file://kmbp.local/Users/me/src/amux/main.go"
		line = "file:///Users/me/src/amux/main.go:42"
	)
	open := func(params, uri string) string { return "\x1b]8;" + params + ";" + uri + "\x07" }
	const end = "\x1b]8;;\x07"
	for _, tc := range []struct {
		name string
		cols int
		in   string
		// rows[i] lists the exact link spans row i must render, in order;
		// a row not listed must carry no link.
		rows [][]string
	}{
		{
			name: "https ST",
			cols: 60,
			in:   "see \x1b]8;;" + web + "\x1b\\docs\x1b]8;;\x1b\\ here",
			rows: [][]string{{open("", web) + "docs" + end}},
		},
		{
			name: "file BEL",
			cols: 60,
			in:   "\x1b]8;;" + file + "\x07main.go\x1b]8;;\x07",
			rows: [][]string{{open("", file) + "main.go" + end}},
		},
		{
			name: "file with hostname",
			cols: 70,
			in:   "\x1b]8;;" + host + "\x1b\\main.go\x1b]8;;\x1b\\",
			rows: [][]string{{open("", host) + "main.go" + end}},
		},
		{
			name: "line suffix in URI and text",
			cols: 60,
			in:   "\x1b]8;;" + line + "\x07main.go:42\x1b]8;;\x07",
			rows: [][]string{{open("", line) + "main.go:42" + end}},
		},
		{
			name: "id param",
			cols: 60,
			in:   "\x1b]8;id=k1;" + file + "\x07main.go:12\x1b]8;;\x07",
			rows: [][]string{{open("id=k1", file) + "main.go:12" + end}},
		},
		{
			name: "multiple params",
			cols: 60,
			in:   "\x1b]8;id=k2:foo=bar;" + web + "\x1b\\x\x1b]8;;\x1b\\",
			rows: [][]string{{open("id=k2:foo=bar", web) + "x" + end}},
		},
		{
			name: "two links on one row",
			cols: 60,
			in:   "\x1b]8;;" + web + "\x07a\x1b]8;;\x07 \x1b]8;id=f;" + file + "\x07b\x1b]8;;\x07",
			rows: [][]string{{open("", web) + "a" + end, open("id=f", file) + "b" + end}},
		},
		{
			// The link text autowraps at the right margin: both rows must
			// carry the whole link, each closed at its row's end so nothing
			// past the pane (padding, the next composited block) is linked.
			name: "wraps across rows",
			cols: 20,
			in:   "0123456789ab\x1b]8;id=w;" + file + "\x07src/amux/main.go\x1b]8;;\x07 z",
			rows: [][]string{
				{open("id=w", file) + "src/amux" + end},
				{open("id=w", file) + "/main.go" + end},
			},
		},
		{
			// A repaint over part of the link (cursor back, plain text) must
			// unlink exactly the overwritten cells and keep the rest.
			name: "partial repaint over link",
			cols: 40,
			in:   "\x1b]8;;" + file + "\x07main.go:12\x1b]8;;\x07\x1b[1;5HXX",
			rows: [][]string{{open("", file) + "main" + end, open("", file) + "o:12" + end}},
		},
		{
			// A full-screen agent redraws its frame with the same link: the
			// rows it rewrites must still carry it, once, on the same cells.
			name: "link repainted by a redraw",
			cols: 40,
			in: "\x1b]8;id=r;" + file + "\x07main.go\x1b]8;;\x07\r\nstatus 1" +
				"\x1b[H\x1b[2J\x1b]8;id=r;" + file + "\x07main.go\x1b]8;;\x07\r\nstatus 2",
			rows: [][]string{{open("id=r", file) + "main.go" + end}, nil},
		},
		{
			// A link left open across a repaint of another row keeps applying
			// to new text until the child closes it, as on a real terminal.
			name: "open link spans a repaint elsewhere",
			cols: 40,
			in:   "\x1b]8;;" + web + "\x07ab\x1b[3;1Hcd\x1b]8;;\x07ef",
			rows: [][]string{{open("", web) + "ab" + end}, nil, {open("", web) + "cd" + end}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			term := New(tc.cols, 4)
			term.ingest([]byte(tc.in))
			got := strings.Split(term.Render(), "\n")
			for y, row := range got {
				var want []string
				if y < len(tc.rows) {
					want = tc.rows[y]
				}
				if links := osc8Spans(row); strings.Join(links, "|") != strings.Join(want, "|") {
					t.Errorf("row %d links\n got %q\nwant %q\n row %q", y, links, want, row)
				}
			}
		})
	}
}

// osc8Spans returns each OSC 8 link in a rendered row as one string: the
// opening sequence, the linked text with SGR sequences removed, and the
// closing sequence.
func osc8Spans(row string) []string {
	var spans []string
	for {
		i := strings.Index(row, "\x1b]8;")
		if i < 0 {
			return spans
		}
		row = row[i:]
		j := strings.IndexByte(row, '\x07')
		if j < 0 {
			return append(spans, row)
		}
		openSeq := row[:j+1]
		row = row[j+1:]
		if openSeq == "\x1b]8;;\x07" {
			spans = append(spans, openSeq) // a close with no open: wrong
			continue
		}
		k := strings.Index(row, "\x1b]8;")
		if k < 0 {
			return append(spans, openSeq+stripSGR(row))
		}
		text := stripSGR(row[:k])
		row = row[k:]
		closeSeq := row
		if c := strings.IndexByte(row, '\x07'); c >= 0 {
			closeSeq = row[:c+1]
		}
		if closeSeq != "\x1b]8;;\x07" {
			// Opened again without closing: report it as-is.
			spans = append(spans, openSeq+text)
			continue
		}
		spans = append(spans, openSeq+text+closeSeq)
		row = row[len(closeSeq):]
	}
}

// stripSGR removes CSI … m sequences so link text compares as plain text.
func stripSGR(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
				j++
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
