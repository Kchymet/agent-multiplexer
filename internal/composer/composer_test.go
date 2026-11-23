package composer

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"amux/internal/vterm"
)

// Pane fixtures: the byte streams the runtimes paint, replayed through the
// same emulator the daemon mirrors panes with. They reproduce each TUI's
// composer layout and styling (glyphs, frames, placeholder attributes, footer)
// at the cells the parser reads.
const (
	esc     = "\x1b"
	dim     = esc + "[2m"
	gray    = esc + "[90m"
	grayRGB = esc + "[38;2;153;153;153m"
	inverse = esc + "[7m"
	reset   = esc + "[0m"
	rule    = "────────────────────────────────────────────────────────────"
)

// claudeScreen paints a Claude Code (2.x) screen: transcript, then the
// composer between two rules, then its footer. composer is the raw (styled)
// text after the "> " prompt; extra composer rows are given pre-indented.
func claudeScreen(composer ...string) string {
	var b strings.Builder
	b.WriteString(esc + "[2J" + esc + "[H")
	b.WriteString("> " + dim + "earlier prompt the user sent" + reset + "\r\n")
	b.WriteString("⏺ Done. The tests pass.\r\n")
	b.WriteString("\r\n")
	b.WriteString(gray + rule + reset + "\r\n")
	b.WriteString("> " + composer[0] + "\r\n")
	for _, row := range composer[1:] {
		b.WriteString(row + "\r\n")
	}
	b.WriteString(gray + rule + reset + "\r\n")
	b.WriteString(dim + "  ? for shortcuts" + reset)
	return b.String()
}

// claudeBoxScreen paints the boxed composer of Claude Code 1.x.
func claudeBoxScreen(composer string) string {
	return esc + "[2J" + esc + "[H" +
		"⏺ Earlier answer.\r\n\r\n" +
		gray + "╭" + strings.Repeat("─", 58) + "╮" + reset + "\r\n" +
		gray + "│" + reset + " > " + composer + esc + "[60G" + gray + "│" + reset + "\r\n" +
		gray + "╰" + strings.Repeat("─", 58) + "╯" + reset + "\r\n" +
		dim + "  ? for shortcuts" + reset
}

// codexScreen paints a Codex screen: transcript, the "›" composer, a blank
// row and the dim footer.
func codexScreen(composer ...string) string {
	var b strings.Builder
	b.WriteString(esc + "[2J" + esc + "[H")
	b.WriteString("› " + "earlier prompt" + "\r\n\r\n")
	b.WriteString("• Updated the parser.\r\n\r\n")
	b.WriteString("› " + composer[0] + "\r\n")
	for _, row := range composer[1:] {
		b.WriteString(row + "\r\n")
	}
	b.WriteString("\r\n")
	b.WriteString(dim + "  ⏎ send   ⇧⏎ newline   ⌃T transcript   ⌃C quit" + reset + "\r\n")
	b.WriteString(dim + "  100% context left" + reset)
	return b.String()
}

func read(t *testing.T, runtime, stream string) Reading {
	t.Helper()
	return Read(runtime, vterm.Capture([]byte(stream), 80, 24))
}

func TestReadClaudeComposer(t *testing.T) {
	draft := "...Ideally we can support status and polling on those"
	for _, tc := range []struct {
		name   string
		stream string
		found  bool
		text   string
	}{
		{"empty with placeholder (dim)", claudeScreen(inverse + "T" + reset + dim + `ry "fix the flaky test"` + reset), true, ""},
		{"empty with placeholder (gray)", claudeScreen(inverse + "T" + reset + grayRGB + `ry "how does prwatch work?"` + reset), true, ""},
		{"empty with only the cursor", claudeScreen(inverse + " " + reset), true, ""},
		{"empty, bare", claudeScreen(""), true, ""},
		{"draft", claudeScreen(draft + inverse + " " + reset), true, draft},
		{"draft with the cursor inside it", claudeScreen("fix th" + inverse + "e" + reset + " bug"), true, "fix the bug"},
		{"multi-line draft", claudeScreen("first line", "  second line", "  third"), true, "first line\nsecond line\nthird"},
		{"boxed, empty", claudeBoxScreen(inverse + "T" + reset + dim + `ry "refactor"` + reset), true, ""},
		{"boxed, draft", claudeBoxScreen("half a thought"), true, "half a thought"},
		{"slash command draft in color", claudeScreen(esc + "[38;5;75m/compact" + reset), true, "/compact"},
		{"permission dialog, no composer", esc + "[2J" + esc + "[H" + "> earlier prompt\r\n\r\nBash command\r\n  rm -rf build\r\n❯ 1. Yes\r\n  2. No, and tell Claude what to do differently (esc)\r\n", false, ""},
		{"booting", esc + "[2J" + esc + "[H" + "✻ Welcome to Claude Code!", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := read(t, RuntimeClaude, tc.stream)
			if got.Found != tc.found || got.Text != tc.text {
				t.Fatalf("Read = %+v, want found=%v text=%q", got, tc.found, tc.text)
			}
			if tc.found && (got.Empty() != (tc.text == "")) {
				t.Fatalf("Empty() = %v for %q", got.Empty(), got.Text)
			}
		})
	}
}

func TestReadCodexComposer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stream string
		found  bool
		text   string
	}{
		{"empty with placeholder", codexScreen(dim + "Ask Codex to do anything" + reset), true, ""},
		{"empty, bare", codexScreen(""), true, ""},
		{"draft", codexScreen("please also cover the polling case"), true, "please also cover the polling case"},
		{"multi-line draft", codexScreen("line one", "  line two"), true, "line one\nline two"},
		{"older bar glyph", esc + "[2J" + esc + "[H" + "▌ half typed\r\n\r\n" + dim + "  ⏎ send" + reset, true, "half typed"},
		{"approval modal, no composer", esc + "[2J" + esc + "[H" + "Allow command?\r\n  git push\r\n  y yes   n no", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := read(t, RuntimeCodex, tc.stream)
			if got.Found != tc.found || got.Text != tc.text {
				t.Fatalf("Read = %+v, want found=%v text=%q", got, tc.found, tc.text)
			}
		})
	}
}

// A composer wider than the pane wraps; Same compares across the wrap.
func TestSameAcrossWrapping(t *testing.T) {
	text := "[amux goal]\nPR update (amux PR watcher wake-up, not a new task):\n- member m1 PR o/r#7: CI failed (check build) — https://github.com/o/r/pull/7\nCheck these PRs and act on them as your task requires."
	rows := []string{"[amux goal]"}
	for _, line := range strings.Split(text, "\n")[1:] {
		for len(line) > 70 {
			rows = append(rows, "  "+line[:70])
			line = line[70:]
		}
		rows = append(rows, "  "+line)
	}
	got := read(t, RuntimeClaude, claudeScreen(rows...))
	if !got.Found || !Same(got.Text, text) {
		t.Fatalf("wrapped composer %q does not match the typed text", got.Text)
	}
	if Same(got.Text, "user draft "+text) {
		t.Fatal("Same ignored a leading draft")
	}
}

// The genuine Codex 0.157.1 terminal stream captured for the embedded-scroll
// evidence (docs/evidence/embedded-scroll, 100x28): a person typed a draft into
// the composer, submitted it, and the composer went back to its dim
// placeholder with the submitted text now in the transcript above it.
func TestReadCodexRealCapture(t *testing.T) {
	raw, err := os.ReadFile("../../docs/evidence/embedded-scroll/always-pty.raw")
	if err != nil {
		t.Fatal(err)
	}
	const draft = "DRAFT-WHEEL-MUST-STAY"
	at := bytes.Index(raw, []byte(draft))
	if at < 0 {
		t.Fatal("capture holds no draft")
	}
	// Up to just past the frame that first paints the draft.
	got := Read(RuntimeCodex, vterm.Capture(raw[:at+200], 100, 28))
	if !got.Found || got.Empty() || !strings.HasPrefix(got.Text, draft) {
		t.Fatalf("composer with a draft read as %+v", got)
	}
	// The whole stream ends after the draft was submitted: the composer shows
	// "Ask Codex to do anything" (dim) and the draft sits in the transcript.
	if got := Read(RuntimeCodex, vterm.Capture(raw, 100, 28)); !got.Empty() {
		t.Fatalf("composer after submit read as %+v, want empty", got)
	}
}

func TestUnknownRuntime(t *testing.T) {
	if got := read(t, "hermes", claudeScreen("x")); got.Found {
		t.Fatalf("unsupported runtime read as %+v", got)
	}
}
