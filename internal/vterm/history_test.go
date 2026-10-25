package vterm

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// write feeds s synchronously through the same path pump and feedLoop use.
func (t *Terminal) write(s string) { t.ingest([]byte(s)) }

// historyText returns every history row as trimmed plain text, oldest first.
func (t *Terminal) historyText() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	sb := t.emu.Scrollback()
	out := make([]string, 0, sb.Len())
	for i := 0; i < sb.Len(); i++ {
		out = append(out, sb.Line(i).String())
	}
	return out
}

// renderRows returns Render split into rows of plain text, right-trimmed.
func renderRows(term *Terminal) []string {
	rows := strings.Split(term.Render(), "\n")
	for i, r := range rows {
		rows[i] = strings.TrimRight(ansi.Strip(r), " ")
	}
	return rows
}

// numbered returns "line 1\r\n" … "line n\r\n".
func numbered(from, to int) string {
	var b strings.Builder
	for i := from; i <= to; i++ {
		fmt.Fprintf(&b, "line %d\r\n", i)
	}
	return b.String()
}

// codexInsert is the byte shape Codex's inline transcript mode emits to commit
// rows above its viewport (codex-rs insert_history.rs, InsertHistoryMode
// Standard): a scrolling region from row 1 to the viewport's top, the cursor on
// the region's last row, "\r\n" plus the row for each committed line, then the
// region reset and the cursor restored. Each "\r\n" scrolls one row off row 0.
func codexInsert(viewportTop int, lines ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\x1b[1;%dr", viewportTop)
	fmt.Fprintf(&b, "\x1b[%d;1H", viewportTop) // MoveTo(0, top-1), 1-based
	for _, l := range lines {
		b.WriteString("\r\n")
		b.WriteString(l)
	}
	b.WriteString("\x1b[r")
	fmt.Fprintf(&b, "\x1b[%d;1H", viewportTop+1)
	return b.String()
}

// ratatuiFrame is a cursor-positioned full repaint inside a synchronized
// update, the shape every ratatui/Ink frame (and real Codex) has: it touches
// cells, never the scroll position.
func ratatuiFrame(rows int, tag string) string {
	var b strings.Builder
	b.WriteString("\x1b[?2026h")
	for y := 1; y <= rows; y++ {
		fmt.Fprintf(&b, "\x1b[%d;1H\x1b[2Krow %d %s", y, y, tag)
	}
	b.WriteString("\x1b[?2026l")
	return b.String()
}

// viewportFrame repaints only rows top..bottom (1-based) with cursor
// positioning, the way Codex redraws its inline viewport under the transcript.
func viewportFrame(top, bottom int, tag string) string {
	var b strings.Builder
	b.WriteString("\x1b[?2026h")
	for y := top; y <= bottom; y++ {
		fmt.Fprintf(&b, "\x1b[%d;1H\x1b[2Krow %d %s", y, y, tag)
	}
	b.WriteString("\x1b[?2026l")
	return b.String()
}

func TestHistoryCapturesScrolledOffRows(t *testing.T) {
	term := New(20, 4)
	term.write(numbered(1, 10)) // 4-row screen: lines 1–6 scroll off (10 lines + trailing blank row)
	got := term.historyText()
	want := []string{"line 1", "line 2", "line 3", "line 4", "line 5", "line 6", "line 7"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("history = %q, want %q", got, want)
	}
	v := term.Viewport()
	if v.Offset != 0 || v.HistoryLen != 7 || v.Cols != 20 || v.Rows != 4 || v.AltScreen {
		t.Fatalf("viewport = %+v", v)
	}
	if term.Indicator() != "" {
		t.Fatalf("indicator while live = %q", term.Indicator())
	}

	if !term.ScrollLines(2) {
		t.Fatal("ScrollLines(2) reported no movement")
	}
	rows := renderRows(term)
	if len(rows) != 4 {
		t.Fatalf("rendered %d rows, want 4", len(rows))
	}
	// Two newest history rows above the live screen's top two rows; the
	// indicator is text for the embedder, never drawn into the pane.
	if rows[0] != "line 6" || rows[1] != "line 7" || rows[2] != "line 8" || rows[3] != "line 9" {
		t.Fatalf("rows = %q", rows)
	}
	if term.Indicator() != " ↑ 2/7 " {
		t.Fatalf("indicator = %q", term.Indicator())
	}
	for _, line := range strings.Split(term.Render(), "\n") {
		if w := ansi.StringWidth(line); w != 20 {
			t.Fatalf("scrolled frame row width %d, want 20: %q", w, line)
		}
	}

	// Bounds: clamp at the oldest row, then at live; no movement reports false.
	if !term.ScrollLines(100) || term.Viewport().Offset != 7 {
		t.Fatalf("clamp to top: %+v", term.Viewport())
	}
	if term.ScrollLines(1) {
		t.Fatal("ScrollLines past the top moved")
	}
	if rows := renderRows(term); !strings.HasPrefix(rows[0], "line 1") {
		t.Fatalf("top row = %q", rows[0])
	}
	if !term.ReturnLive() || term.Viewport().Offset != 0 || term.ReturnLive() {
		t.Fatal("ReturnLive")
	}
	if term.ScrollLines(-1) {
		t.Fatal("ScrollLines below live moved")
	}
	// Pages are rows-1 so a row of context carries over.
	if !term.ScrollPages(1) || term.Viewport().Offset != 3 {
		t.Fatalf("ScrollPages(1): %+v", term.Viewport())
	}
	if !term.ScrollToTop() || term.Viewport().Offset != 7 {
		t.Fatalf("ScrollToTop: %+v", term.Viewport())
	}
	if !term.ScrollPages(-1) || term.Viewport().Offset != 4 {
		t.Fatalf("ScrollPages(-1): %+v", term.Viewport())
	}
}

// TestHistoryChunkBoundaries: the same output must yield the same history,
// frame and viewport however the stream is cut into Feed chunks — escape
// sequences, UTF-8 and the scrub state all span boundaries.
func TestHistoryChunkBoundaries(t *testing.T) {
	// Committed rows sit on screen above the viewport until later inserts
	// scroll them off, so the second insert is what lands the first in history.
	stream := numbered(1, 12) +
		codexInsert(3, "\x1b[1;32m• committed one\x1b[0m", "日本語 wide 列", "\x1b]0;✳ title\x07plain") +
		codexInsert(3, "second", "batch", "here") +
		"\x1b[2J\x1b[H" + ratatuiFrame(3, "a") + ratatuiFrame(3, "b") +
		codexInsert(4, "after repaint") + "tail\r\n"
	feed := func(chunks [][]byte) *Terminal {
		term := New(16, 5)
		for _, c := range chunks {
			term.write(string(c))
		}
		return term
	}
	whole := feed([][]byte{[]byte(stream)})
	wantHist := whole.historyText()
	if len(wantHist) < 14 || !contains(wantHist, "• committed one") || !contains(wantHist, "日本語 wide 列") || !contains(wantHist, "plain") {
		t.Fatalf("baseline history looks wrong: %q", wantHist)
	}
	whole.ScrollLines(3)
	wantFrame, wantView := whole.Render(), whole.Viewport()

	split := func(name string, sizes func() int) {
		var chunks [][]byte
		for rest := []byte(stream); len(rest) > 0; {
			n := min(max(1, sizes()), len(rest))
			chunks = append(chunks, rest[:n])
			rest = rest[n:]
		}
		term := feed(chunks)
		if got := term.historyText(); strings.Join(got, "\n") != strings.Join(wantHist, "\n") {
			t.Fatalf("%s: history differs\n got %q\nwant %q", name, got, wantHist)
		}
		term.ScrollLines(3)
		if got := term.Render(); got != wantFrame {
			t.Fatalf("%s: frame differs\n got %q\nwant %q", name, got, wantFrame)
		}
		gotView := term.Viewport()
		gotView.Revision, wantView.Revision = 0, 0
		if gotView != wantView {
			t.Fatalf("%s: viewport %+v, want %+v", name, gotView, wantView)
		}
	}
	split("bytes", func() int { return 1 })
	split("sevens", func() int { return 7 })
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 20; i++ {
		split(fmt.Sprintf("random-%d", i), func() int { return rng.Intn(40) })
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func TestHistoryBounded(t *testing.T) {
	term := New(20, 3)
	term.SetHistoryLimit(50)
	term.write(numbered(1, 500))
	got := term.historyText()
	if len(got) != 50 || got[0] != "line 449" || got[49] != "line 498" {
		t.Fatalf("history len %d, first %q, last %q", len(got), got[0], got[len(got)-1])
	}
	if v := term.Viewport(); v.HistoryLen != 50 {
		t.Fatalf("HistoryLen = %d", v.HistoryLen)
	}
	// Shrinking drops the oldest rows at once and keeps the offset in range.
	term.ScrollToTop()
	term.SetHistoryLimit(10)
	if v := term.Viewport(); v.HistoryLen != 10 || v.Offset != 10 {
		t.Fatalf("after shrink: %+v", v)
	}
	if got := term.historyText(); got[0] != "line 489" {
		t.Fatalf("oldest after shrink = %q", got[0])
	}
	// The default bound applies without a call.
	def := New(20, 3)
	def.write(numbered(1, DefaultHistoryLines+300))
	if n := def.Viewport().HistoryLen; n != DefaultHistoryLines {
		t.Fatalf("default history len %d, want %d", n, DefaultHistoryLines)
	}
	// A full ring still returns the viewport to live on growth (length alone no
	// longer changes).
	def.ScrollLines(5)
	def.write("more\r\n")
	if off := def.Viewport().Offset; off != 0 {
		t.Fatalf("offset after growth on full ring = %d", off)
	}
}

// TestHistoryIgnoresRepaints: cursor-positioned redraws and erase-display
// clears — the way every TUI frame and clear-and-repaint cycle is drawn — must
// not grow history, while Codex-style inserts between them are captured once.
func TestHistoryIgnoresRepaints(t *testing.T) {
	term := New(30, 6)
	for i := 0; i < 50; i++ {
		term.write(ratatuiFrame(6, fmt.Sprint(i)))
	}
	if got := term.historyText(); len(got) != 0 {
		t.Fatalf("cursor-only repaints grew history: %q", got)
	}
	for i := 0; i < 20; i++ {
		term.write("\x1b[2J\x1b[H" + ratatuiFrame(6, "clear"))
	}
	if got := term.historyText(); len(got) != 0 {
		t.Fatalf("clear-and-repaint grew history: %q", got)
	}
	// Codex inline from a cleared screen, viewport on rows 3-6 (two rows of
	// transcript above it): each insert scrolls the rows above the viewport
	// off row 0 — blank ones first, then the committed rows, each exactly
	// once — while viewport frames and the clear on a viewport move add
	// nothing.
	term.write("\x1b[2J\x1b[H")
	term.write(codexInsert(2, "user: hello", "codex: hi"))
	for i := 0; i < 5; i++ {
		term.write(viewportFrame(3, 6, "spinner"))
	}
	term.write(codexInsert(2, "codex: more", "codex: done"))
	term.write("\x1b[2J" + viewportFrame(3, 6, "viewport moved"))
	term.write(codexInsert(2, "codex: final"))
	want := []string{"", "", "user: hello", "codex: hi", ""}
	if got := term.historyText(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("history = %q, want %q", got, want)
	}
	if rows := renderRows(term); rows[0] != "" || rows[1] != "codex: final" || rows[2] != "row 3 viewport moved" {
		t.Fatalf("screen = %q", rows)
	}
	// ESC[3J is "clear scrollback" and still does.
	term.write("\x1b[3J")
	if n := term.Viewport().HistoryLen; n != 0 {
		t.Fatalf("ESC[3J left %d rows", n)
	}
}

// TestEraseDisplayKeepsPenBackground: the ED 2 override must still erase like
// ED 2 — every cell, with the pen's background (bce), cursor untouched — and
// output in the same chunk after it must still apply.
func TestEraseDisplayKeepsPenBackground(t *testing.T) {
	term := New(10, 3)
	term.write("\x1b[44mX\x1b[2;3H\x1b[2JY")
	term.mu.Lock()
	defer term.mu.Unlock()
	x := term.emu.CellAt(0, 0)
	if x.Content != " " {
		t.Fatalf("cell (0,0) not erased: %q", x.Content)
	}
	if x.Style.Bg == nil {
		t.Fatal("erased cell lost the pen background")
	}
	if y := term.emu.CellAt(2, 1); y.Content != "Y" {
		t.Fatalf("output after ED 2 in the same chunk: (2,1) = %q", y.Content)
	}
	if pos := term.emu.CursorPosition(); pos.X != 3 || pos.Y != 1 {
		t.Fatalf("cursor after ED 2 = %+v, want (3,1)", pos)
	}
	for yy := 0; yy < 3; yy++ {
		for xx := 0; xx < 10; xx++ {
			if xx == 2 && yy == 1 {
				continue
			}
			if c := term.emu.CellAt(xx, yy); c.Style.Bg == nil || c.Content != " " {
				t.Fatalf("cell (%d,%d) = %+v, want blank with bg", xx, yy, c)
			}
		}
	}
}

// altFrames returns the alternate-screen frame history as text rows per
// frame, oldest first.
func altFrames(term *Terminal) [][]string {
	term.mu.Lock()
	defer term.mu.Unlock()
	out := make([][]string, 0, len(term.alt.frames))
	for _, fr := range term.alt.frames {
		rows := make([]string, len(fr))
		for y, row := range fr {
			rows[y] = row.String()
		}
		out = append(out, rows)
	}
	return out
}

// syncFrame paints lines on rows 1..n inside a synchronized update, the way
// owned-mode Codex redraws its transcript: cursor positioning and erase-line
// only, never a scroll operation.
func syncFrame(lines ...string) string {
	var b strings.Builder
	b.WriteString("\x1b[?2026h")
	for y, l := range lines {
		fmt.Fprintf(&b, "\x1b[%d;1H\x1b[2K%s", y+1, l)
	}
	b.WriteString("\x1b[?2026l")
	return b.String()
}

// TestHistoryPrimaryVersusAlternate: the two screens have separate histories.
// Entering the alternate screen returns the viewport to live and starts an
// empty frame history; nothing the program does there touches primary history;
// leaving hands the untouched primary history back and drops the frames.
func TestHistoryPrimaryVersusAlternate(t *testing.T) {
	term := New(20, 4)
	term.write(numbered(1, 8)) // 5 rows into history
	term.ScrollLines(3)
	term.write("\x1b[?1049h")
	v := term.Viewport()
	if !v.AltScreen || v.Offset != 0 || v.HistoryLen != 0 {
		t.Fatalf("after entering alt: %+v", v)
	}
	if term.ScrollLines(2) || term.ScrollPages(1) || term.ScrollToTop() {
		t.Fatal("scrolling moved with no completed frame")
	}
	term.write(strings.Repeat("alt row\r\n", 30)) // scrolls inside the alt screen
	term.write("\x1b[2J" + syncFrame("one", "two", "three", "four"))
	term.write(syncFrame("five", "six", "seven", "eight"))
	v = term.Viewport()
	if !v.AltScreen || v.HistoryLen != 4 {
		t.Fatalf("alt frame history: %+v (frames %q)", v, altFrames(term))
	}
	if !term.ScrollLines(2) || term.Indicator() != " ↑ 2/4 " {
		t.Fatalf("alt scroll: %+v %q", term.Viewport(), term.Indicator())
	}
	if rows := renderRows(term); rows[0] != "three" || rows[1] != "four" || rows[2] != "five" || rows[3] != "six" {
		t.Fatalf("alt scrolled rows = %q", rows)
	}
	term.write("\x1b[?1049l")
	v = term.Viewport()
	if v.AltScreen || v.Offset != 0 || v.HistoryLen != 5 {
		t.Fatalf("after leaving alt: %+v", v)
	}
	if n := len(altFrames(term)); n != 0 {
		t.Fatalf("%d alt frames kept after leaving the alternate screen", n)
	}
	if !term.ScrollLines(2) {
		t.Fatal("primary history not scrollable after alt")
	}
	rows := renderRows(term)
	if !strings.HasPrefix(rows[0], "line 4") || rows[1] != "line 5" || rows[2] != "line 6" {
		t.Fatalf("rows after alt = %q", rows)
	}
}

// TestAltFramesCommitPreviouslyPainted: DECSET 2026 commits the frame on
// display, so the live frame is never in history; identical and blank frames
// are dropped; a nested begin does not commit a half-painted screen.
func TestAltFramesCommitPreviouslyPainted(t *testing.T) {
	term := New(12, 2)
	term.write("\x1b[?1049h")
	term.write(syncFrame("a1", "a2")) // the blank entry screen is not kept
	if got := altFrames(term); len(got) != 0 {
		t.Fatalf("blank frame kept: %q", got)
	}
	term.write(syncFrame("a1", "a2"))      // identical repaint: commits a, once
	term.write(syncFrame("a1", "a2"))      // again: the committed a is unchanged
	term.write("\x1b[2;3H")                // cursor-only movement between frames
	term.write(syncFrame("b1", "b2"))      // commits nothing new (still a)
	term.write("\x1b[?2026h\x1b[1;1Hhalf") // begins c, commits b
	term.write("\x1b[?2026h")              // nested begin: no commit
	term.write("\x1b[?2026l")
	term.write(syncFrame("d1", "d2")) // commits the finished c
	want := [][]string{{"a1", "a2"}, {"b1", "b2"}, {"half", "b2"}}
	if got := altFrames(term); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("frames = %q, want %q", got, want)
	}
	v := term.Viewport()
	if v.HistoryLen != 6 || v.Offset != 0 {
		t.Fatalf("viewport %+v", v)
	}
	// The live frame d is on screen and not in history; scrolling shows the
	// stream of committed rows above it.
	term.ScrollLines(1)
	if rows := renderRows(term); rows[0] != "b2" || rows[1] != "d1" {
		t.Fatalf("rows at offset 1 = %q", rows)
	}
	term.ScrollToTop()
	if rows := renderRows(term); rows[0] != "a1" || rows[1] != "a2" {
		t.Fatalf("rows at top = %q", rows)
	}
	snap := term.Snapshot()
	if !snap.AltScreen || snap.Offset != 6 || snap.Text(0) != "a1" || snap.Text(1) != "a2" {
		t.Fatalf("snapshot %+v %q %q", snap, snap.Text(0), snap.Text(1))
	}
	// New output returns to live, and the frame it begins is committed.
	term.write(syncFrame("e1", "e2"))
	if v := term.Viewport(); v.Offset != 0 || v.HistoryLen != 8 {
		t.Fatalf("after output %+v", v)
	}
}

// TestAltFramesChunkBoundaries: frame capture happens at the sequence, so the
// same bytes chunked differently yield the same frame history.
func TestAltFramesChunkBoundaries(t *testing.T) {
	var b strings.Builder
	b.WriteString("\x1b[?1049h\x1b[?1000h\x1b[?1006h")
	for i := 0; i < 12; i++ {
		b.WriteString(syncFrame(fmt.Sprintf("turn %d ⠋ 日本", i), "composer", fmt.Sprintf("status %d", i)))
	}
	all := b.String()
	whole := New(16, 3)
	whole.write(all)
	wantFrames := altFrames(whole)
	if len(wantFrames) != 11 {
		t.Fatalf("whole: %d frames", len(wantFrames))
	}
	whole.ScrollLines(7)
	wantRows := renderRows(whole)
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	for trial := 0; trial < 20; trial++ {
		term := New(16, 3)
		rest := all
		for len(rest) > 0 {
			n := 1 + rng.Intn(min(len(rest), 11))
			term.write(rest[:n])
			rest = rest[n:]
		}
		if got := altFrames(term); fmt.Sprint(got) != fmt.Sprint(wantFrames) {
			t.Fatalf("trial %d frames differ:\n%q\n%q", trial, got, wantFrames)
		}
		term.ScrollLines(7)
		if got := renderRows(term); fmt.Sprint(got) != fmt.Sprint(wantRows) {
			t.Fatalf("trial %d rows differ:\n%q\n%q", trial, got, wantRows)
		}
	}
}

// TestAltFramesBounded: the frame history is bounded by rows, dropping whole
// frames oldest first, with the newest frame always kept; it never mixes with
// primary history and RIS clears it.
func TestAltFramesBounded(t *testing.T) {
	term := New(10, 3)
	term.write(numbered(1, 5)) // 2 primary rows
	term.SetHistoryLimit(7)    // two 3-row frames fit, three do not
	term.write("\x1b[?1049h")
	for i := 0; i < 6; i++ {
		term.write(syncFrame(fmt.Sprintf("f%d", i), "x", "y"))
	}
	frames := altFrames(term)
	if len(frames) != 2 || frames[0][0] != "f3" || frames[1][0] != "f4" {
		t.Fatalf("frames = %q", frames)
	}
	if v := term.Viewport(); v.HistoryLen != 6 {
		t.Fatalf("viewport %+v", v)
	}
	term.SetHistoryLimit(1) // smaller than a frame: the newest frame survives
	if frames := altFrames(term); len(frames) != 1 || frames[0][0] != "f4" {
		t.Fatalf("frames after shrink = %q", frames)
	}
	term.ScrollToTop()
	if v := term.Viewport(); v.Offset != 3 {
		t.Fatalf("offset after shrink %+v", v)
	}
	term.write("\x1bc") // RIS: both histories go
	if v := term.Viewport(); v.AltScreen || v.HistoryLen != 0 || len(altFrames(term)) != 0 {
		t.Fatalf("after RIS %+v", v)
	}
	// Primary history never saw the frames.
	term.SetHistoryLimit(0)
	term.write(numbered(1, 4))
	if got := term.historyText(); len(got) != 2 || got[0] != "line 1" || got[1] != "line 2" {
		t.Fatalf("primary history = %q", got)
	}
}

// TestAltWithoutSyncNoHistory: an alternate-screen program that never uses
// synchronized output completes no frame, so it keeps its own scrolling.
func TestAltWithoutSyncNoHistory(t *testing.T) {
	term := New(10, 3)
	term.write("\x1b[?1049h")
	for i := 0; i < 5; i++ {
		term.write(fmt.Sprintf("\x1b[1;1H\x1b[2Kframe %d\x1b[2;1Hbody", i))
	}
	if v := term.Viewport(); !v.AltScreen || v.HistoryLen != 0 || term.ScrollLines(1) {
		t.Fatalf("history without frame boundaries: %+v", v)
	}
}

func TestHistoryResize(t *testing.T) {
	term := New(30, 4)
	term.write(numbered(1, 8))
	term.write("a much longer history row that overflows\r\n") // scrolls off later
	term.write(numbered(9, 12))
	term.ScrollLines(3)
	rev := term.Viewport().Revision
	term.Resize(12, 3)
	v := term.Viewport()
	if v.Offset != 0 || v.Cols != 12 || v.Rows != 3 || v.Revision == rev {
		t.Fatalf("after resize: %+v", v)
	}
	if v.HistoryLen == 0 {
		t.Fatal("resize dropped history")
	}
	term.ScrollToTop()
	frame := strings.Split(term.Render(), "\n")
	if len(frame) != 3 {
		t.Fatalf("frame has %d rows, want 3", len(frame))
	}
	for _, line := range frame {
		if w := ansi.StringWidth(line); w != 12 {
			t.Fatalf("row width %d, want 12: %q", w, line)
		}
	}
	snap := term.Snapshot()
	if snap.Cols != 12 || snap.Rows != 3 || len(snap.Cells) != 3 || len(snap.Cells[0]) != 12 {
		t.Fatalf("snapshot shape %dx%d / %d", snap.Cols, snap.Rows, len(snap.Cells))
	}
	// Wider again: narrow history rows are padded, the long one still cut
	// to the pane, every row exactly the pane width.
	term.Resize(60, 5)
	term.ScrollLines(4)
	for _, line := range strings.Split(term.Render(), "\n") {
		if w := ansi.StringWidth(line); w != 60 {
			t.Fatalf("row width %d, want 60: %q", w, line)
		}
	}
	found := false
	for _, r := range renderRows(term) {
		if strings.HasPrefix(r, "a much longer history row") {
			found = true
		}
	}
	if !found {
		t.Fatalf("long row missing from %q", renderRows(term))
	}
}

func TestHistoryResetClears(t *testing.T) {
	term := New(20, 3)
	term.write(numbered(1, 10))
	term.ScrollLines(2)
	term.write("\x1bc")
	if v := term.Viewport(); v.HistoryLen != 0 || v.Offset != 0 {
		t.Fatalf("after RIS: %+v", v)
	}
	// The replay the daemon sends after a reset re-scrolls output the history
	// already held; with the clear it is captured exactly once.
	term.write(numbered(5, 10))
	if got := term.historyText(); len(got) != 4 || got[0] != "line 5" {
		t.Fatalf("history after replay = %q", got)
	}

	// Remote mode: Reset rides the feed queue, ordered with output.
	remote := NewRemote(20, 3, func([]byte) {}, func(int, int) {})
	defer remote.Close()
	remote.Feed([]byte(numbered(1, 10)))
	if !waitFor(2*time.Second, func() bool { return remote.Viewport().HistoryLen == 8 }) {
		t.Fatalf("remote history = %+v", remote.Viewport())
	}
	remote.ScrollLines(3)
	remote.Reset()
	remote.Feed([]byte(numbered(1, 10)))
	if !waitFor(2*time.Second, func() bool {
		v := remote.Viewport()
		return v.HistoryLen == 8 && v.Offset == 0
	}) {
		t.Fatalf("remote after reset+replay = %+v", remote.Viewport())
	}
}

// TestViewportReturnsLive covers the auto-live rules: any output returns to
// live — rows scrolling into history or a repaint that changes text alike — and
// so does input.
func TestViewportReturnsLive(t *testing.T) {
	var sent []byte
	term := NewRemote(20, 4, func(b []byte) { sent = append(sent, b...) }, nil)
	defer term.Close()
	term.write(numbered(1, 10))
	term.ScrollLines(3)
	// While scrolled, the lower rows show the live screen.
	if rows := renderRows(term); rows[3] != "line 8" {
		t.Fatalf("live rows under scrolled viewport: %q", rows)
	}
	term.write(ratatuiFrame(4, "spin")) // repaint without scrolling
	if off := term.Viewport().Offset; off != 0 {
		t.Fatalf("repaint left the viewport at %d", off)
	}
	if rows := renderRows(term); rows[0] != "row 1 spin" {
		t.Fatalf("live frame after return: %q", rows)
	}
	term.ScrollLines(3)
	term.write("\x1b[4;1Hnew output\r\n") // scrolls one row off
	if off := term.Viewport().Offset; off != 0 {
		t.Fatalf("growth left the viewport at %d", off)
	}
	term.ScrollLines(2)
	if _, err := term.Write([]byte("k")); err != nil {
		t.Fatal(err)
	}
	if off := term.Viewport().Offset; off != 0 || string(sent) != "k" {
		t.Fatalf("input: offset %d, sent %q", off, sent)
	}
}

// TestSnapshotAlignedWithRender: the snapshot is the frame's cells — same
// shape, same text per physical row, no indicator in either — detached from
// later output, with a revision that moves on every change.
func TestSnapshotAlignedWithRender(t *testing.T) {
	term := New(24, 4)
	term.write(numbered(1, 10))
	term.write("\x1b[1;31m日本語\x1b[0m end")
	term.ScrollLines(2)
	snap := term.Snapshot()
	rows := renderRows(term)
	if snap.Offset != 2 || snap.HistoryLen != 7 || snap.Cols != 24 || snap.Rows != 4 || snap.AltScreen {
		t.Fatalf("snapshot header %+v", snap)
	}
	for y := 0; y < 4; y++ {
		if got, want := snap.Text(y), rows[y]; got != want || strings.Contains(want, "↑") {
			t.Fatalf("row %d: snapshot %q, render %q", y, got, want)
		}
	}
	if snap.Text(99) != "" {
		t.Fatal("Text out of range")
	}
	// Live snapshot equals the live frame too; its bottom row holds the wide
	// text: first cell Width 2, placeholder zero cell, style carried.
	term.ReturnLive()
	live, liveRows := term.Snapshot(), renderRows(term)
	for y := range liveRows {
		if live.Text(y) != liveRows[y] {
			t.Fatalf("live row %d: %q vs %q", y, live.Text(y), liveRows[y])
		}
	}
	wide := live.Cells[3]
	if wide[0].Content != "日" || wide[0].Width != 2 || !wide[1].IsZero() || wide[0].Style.Fg == nil || wide[0].Style.Attrs&uv.AttrBold == 0 {
		t.Fatalf("wide cells = %+v %+v", wide[0], wide[1])
	}
	// Detached: later output doesn't alter the copy; revision moves.
	term.write("\x1b[4;1Hchanged")
	if live.Text(3) != "日本語 end" {
		t.Fatalf("snapshot mutated: %q", live.Text(3))
	}
	if again := term.Snapshot(); again.Revision == live.Revision {
		t.Fatal("revision did not advance on output")
	}
	r1 := term.Viewport().Revision
	term.ScrollLines(1)
	if r2 := term.Viewport().Revision; r2 == r1 {
		t.Fatal("revision did not advance on scroll")
	}
}

// TestIndicatorStaysOutOfPane: the badge is text for the embedder's border;
// Render never overlays it, so a selection over row 0 copies what is there,
// in narrow and wide panes alike.
func TestIndicatorStaysOutOfPane(t *testing.T) {
	for _, cols := range []int{8, 40} {
		term := New(cols, 3)
		term.write(numbered(1, 6))
		term.ScrollLines(1)
		if strings.Contains(term.Render(), "↑") {
			t.Fatalf("cols %d: indicator drawn into the pane", cols)
		}
		if term.Indicator() != " ↑ 1/4 " {
			t.Fatalf("cols %d: indicator %q", cols, term.Indicator())
		}
		snap, rows := term.Snapshot(), renderRows(term)
		for y := range rows {
			if snap.Text(y) != rows[y] {
				t.Fatalf("cols %d row %d: snapshot %q render %q", cols, y, snap.Text(y), rows[y])
			}
		}
		if rows[0] != "line 4" {
			t.Fatalf("cols %d row 0 = %q", cols, rows[0])
		}
	}
}

func TestMouseTrackedExported(t *testing.T) {
	term := New(10, 3)
	if term.MouseTracked() {
		t.Fatal("tracked before the child asked")
	}
	term.write("\x1b[?1000h\x1b[?1006h")
	if !term.MouseTracked() {
		t.Fatal("not tracked after DECSET 1000")
	}
}
