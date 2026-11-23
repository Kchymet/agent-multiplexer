package nativetui

import (
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"amux/internal/core"
)

// The TUI's theme is the terminal adaptation of the Harness "Switchboard"
// identity: graphite surfaces and mineral neutrals, bright ink for titles and
// keys, blue for running and focus, green for ready, and yellow reserved for
// attention — a filled chip with dark text on what waits on the user. Shape
// and the state word always carry the meaning; color only supplements them, so
// every role survives 16 colors and no color at all.
//
// amux draws on the terminal's own (assumed dark) background and never paints
// a canvas, so the palette holds foregrounds plus two fills: the inset behind
// the selected row and section bars, and the attention chip.

// themeMode is the color depth the chrome is drawn in.
type themeMode int

const (
	themeTrueColor themeMode = iota
	theme256
	theme16
	themeNone // NO_COLOR, or a terminal without color: attributes only
)

func (t themeMode) String() string {
	return [...]string{"truecolor", "256", "16", "none"}[t]
}

// palette is one mode's resolved role colors.
type palette struct {
	ink, ink2, ink3  lipgloss.TerminalColor // titles and keys; body; metadata
	line             lipgloss.TerminalColor // unfocused borders and rules
	inset            lipgloss.TerminalColor // fill behind selection and section bars
	focus            lipgloss.TerminalColor // running state, focused border
	ready, danger    lipgloss.TerminalColor
	attn, attnInk    lipgloss.TerminalColor // attention chip fill and its text
	attnText         lipgloss.TerminalColor // attention as a foreground
	reverseSelection bool                   // no trustworthy fill: select by reverse video
}

func c(s string) lipgloss.TerminalColor { return lipgloss.Color(s) }

var palettes = map[themeMode]palette{
	// The dark column of the Switchboard token table.
	themeTrueColor: {
		ink: c("#E6ECEA"), ink2: c("#B4BEBB"), ink3: c("#93A09B"),
		line: c("#4A5852"), inset: c("#1E2725"),
		focus: c("#7DB0FF"), ready: c("#4CC38A"), danger: c("#FF7B6E"),
		attn: c("#FFC400"), attnInk: c("#1A1400"), attnText: c("#FFD75A"),
	},
	// Nearest xterm-256 cells to the same tokens.
	theme256: {
		ink: c("255"), ink2: c("250"), ink3: c("246"),
		line: c("240"), inset: c("236"),
		focus: c("111"), ready: c("78"), danger: c("209"),
		attn: c("220"), attnInk: c("16"), attnText: c("221"),
	},
	// The 16 ANSI colors are whatever the user's scheme says they are, so no
	// fill is trusted except the attention chip (bright yellow under black),
	// and bright black — invisible in some schemes — is used only for rules.
	theme16: {
		ink: c("15"), ink2: lipgloss.NoColor{}, ink3: c("7"),
		line: c("8"), inset: lipgloss.NoColor{},
		focus: c("12"), ready: c("10"), danger: c("9"),
		attn: c("11"), attnInk: c("0"), attnText: c("11"),
		reverseSelection: true,
	},
	themeNone: {
		ink: lipgloss.NoColor{}, ink2: lipgloss.NoColor{}, ink3: lipgloss.NoColor{},
		line: lipgloss.NoColor{}, inset: lipgloss.NoColor{},
		focus: lipgloss.NoColor{}, ready: lipgloss.NoColor{}, danger: lipgloss.NoColor{},
		attn: lipgloss.NoColor{}, attnInk: lipgloss.NoColor{}, attnText: lipgloss.NoColor{},
		reverseSelection: true,
	},
}

// The chrome's styles. They are package state, rebuilt by applyTheme before
// the program starts; rendering reads them and never constructs colors itself.
var (
	activeTheme themeMode

	dimStyle     lipgloss.Style // metadata, hint descriptions
	sepStyle     lipgloss.Style // unfocused borders and rules
	focusStyle   lipgloss.Style // the focused pane's border, the open-session mark
	titleStyle   lipgloss.Style // titles and workgroup names
	keyStyle     lipgloss.Style // hotkeys and emphasized values
	selStyle     lipgloss.Style // the selected row
	headerStyle  lipgloss.Style // table header bars
	sectionStyle lipgloss.Style // rail section bars
	// sectionKeyStyle accents a hotkey inside a section bar, keeping the bar's
	// fill so the header reads as one solid strip.
	sectionKeyStyle lipgloss.Style
	attnStyle       lipgloss.Style // the attention chip: yellow fill, dark text
	attnTextStyle   lipgloss.Style // attention without a fill (glyphs, low budgets)
	hotStyle        lipgloss.Style // a value worth noticing that needs no action
	errStyle        lipgloss.Style
	cursorStyle     lipgloss.Style // the form's text cursor
	stateStyles     map[string]lipgloss.Style
)

func init() { applyTheme(theme256) }

// applyTheme rebuilds every chrome style for mode. It does not touch
// lipgloss's color profile; see setTheme.
func applyTheme(mode themeMode) {
	p := palettes[mode]
	st := lipgloss.NewStyle
	activeTheme = mode

	dimStyle = st().Foreground(p.ink3)
	sepStyle = st().Foreground(p.line)
	focusStyle = st().Foreground(p.focus)
	titleStyle = st().Bold(true).Foreground(p.ink)
	keyStyle = st().Foreground(p.ink)
	errStyle = st().Foreground(p.danger)
	attnStyle = st().Bold(true).Foreground(p.attnInk).Background(p.attn)
	attnTextStyle = st().Foreground(p.attnText)
	hotStyle = st().Bold(true).Foreground(p.ink)
	cursorStyle = st().Reverse(true)
	if p.reverseSelection {
		selStyle = st().Bold(true).Reverse(true)
		headerStyle = st().Bold(true).Underline(true)
		sectionStyle = st().Bold(true).Foreground(p.ink3)
		sectionKeyStyle = st().Bold(true).Foreground(p.ink)
	} else {
		selStyle = st().Bold(true).Foreground(p.ink).Background(p.inset)
		headerStyle = st().Bold(true).Foreground(p.ink).Background(p.inset)
		sectionStyle = st().Bold(true).Foreground(p.ink2).Background(p.inset)
		sectionKeyStyle = st().Bold(true).Foreground(p.ink).Background(p.inset)
	}
	if mode == themeNone {
		// With no color to tell them apart, weight separates a key from its
		// description and reverse video is the only fill left for attention.
		keyStyle = st().Bold(true)
		attnStyle = st().Bold(true).Reverse(true)
		attnTextStyle = st().Bold(true)
		focusStyle = st().Bold(true)
		errStyle = st().Bold(true)
	}
	stateStyles = map[string]lipgloss.Style{
		core.StateWaiting: attnTextStyle,
		core.StateRunning: st().Foreground(p.focus),
		core.StateReady:   st().Foreground(p.ready),
		core.StateUnknown: st().Foreground(p.ink2),
		core.StateStopped: dimStyle,
		core.StateIdle:    dimStyle, // legacy spelling of stopped from an older provider
	}
}

// stateStyle is the foreground for a state's glyph and word. Waiting's word is
// drawn as the attention chip instead (see statusLine).
func stateStyle(state string) lipgloss.Style {
	if s, ok := stateStyles[state]; ok {
		return s
	}
	return dimStyle
}

// setTheme applies mode and pins lipgloss to the matching color profile, so a
// style renders exactly the roles chosen for that depth instead of a second,
// automatic degradation. themeNone keeps the ANSI profile because lipgloss's
// Ascii profile strips bold and reverse too — the attributes that carry
// selection, focus and attention once color is gone (NO_COLOR asks for no
// color, not no styling). plain leaves a terminal that cannot style at all
// (TERM=dumb, a pipe) on the Ascii profile.
func setTheme(mode themeMode, plain bool) {
	applyTheme(mode)
	switch {
	case plain:
		lipgloss.SetColorProfile(termenv.Ascii)
	case mode == themeTrueColor:
		lipgloss.SetColorProfile(termenv.TrueColor)
	case mode == theme256:
		lipgloss.SetColorProfile(termenv.ANSI256)
	default:
		lipgloss.SetColorProfile(termenv.ANSI)
	}
}

// detectTheme picks the mode from the environment and the terminal's detected
// capability. AMUX_COLOR (truecolor|256|16|none) overrides both; NO_COLOR, when
// set to anything non-empty, removes color. plain reports a terminal that
// should get no escape-styled text at all.
func detectTheme(getenv func(string) string, capability termenv.Profile) (mode themeMode, plain bool) {
	switch strings.ToLower(strings.TrimSpace(getenv("AMUX_COLOR"))) {
	case "truecolor", "24bit":
		return themeTrueColor, false
	case "256":
		return theme256, false
	case "16", "ansi":
		return theme16, false
	case "none", "never", "0":
		return themeNone, false
	}
	if getenv("NO_COLOR") != "" {
		return themeNone, capability == termenv.Ascii
	}
	switch capability {
	case termenv.TrueColor:
		return themeTrueColor, false
	case termenv.ANSI256:
		return theme256, false
	case termenv.ANSI:
		return theme16, false
	}
	return themeNone, true
}

// initAppearance settles colors and glyphs for an interactive run.
func initAppearance() {
	setTheme(detectTheme(os.Getenv, termenv.NewOutput(os.Stdout).ColorProfile()))
	asciiGlyphs = detectASCII(os.Getenv)
}

// ---- glyphs ----

// asciiGlyphs draws the chrome with ASCII only, for terminals and fonts that
// cannot show the box-drawing and geometric glyphs. It never alters an
// embedded pane: that is the agent's own output.
var asciiGlyphs bool

// detectASCII: AMUX_ASCII=1 forces the fallback and AMUX_ASCII=0 forbids it;
// otherwise it follows a locale that names a non-UTF-8 charset, and the Linux
// console and dumb terminals, whose fonts lack these glyphs. An unset locale
// is left alone — it is common where the terminal is nevertheless UTF-8.
func detectASCII(getenv func(string) string) bool {
	switch strings.ToLower(strings.TrimSpace(getenv("AMUX_ASCII"))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	if t := getenv("TERM"); t == "linux" || t == "dumb" {
		return true
	}
	for _, k := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if v := getenv(k); v != "" {
			v = strings.ToLower(v)
			return !strings.Contains(v, "utf-8") && !strings.Contains(v, "utf8")
		}
	}
	return false
}

// State glyphs: one shape per state, so a state never rests on color alone.
// Waiting is the solid diamond the other clients fill with yellow; running is
// the half-turned disc that stands in for their progress arc.
const (
	glyphWaiting  = "◆"
	glyphRunning  = "◐"
	glyphReady    = "●"
	glyphIdle     = "○"
	glyphUnknown  = "◌"
	glyphOpen     = "▌" // the session shown in the pane
	glyphCursor   = "▎" // leading edge of the selected row
	glyphExternal = "↗" // a session amux did not start
)

// stateGlyph is the shape for an activity state.
func stateGlyph(state string) string {
	switch state {
	case core.StateWaiting:
		return glyphWaiting
	case core.StateRunning:
		return glyphRunning
	case core.StateReady:
		return glyphReady
	case core.StateUnknown:
		return glyphUnknown
	default:
		return glyphIdle
	}
}

// Glyphs whose ASCII stand-in has a different width are chosen before layout.
func enterKey() string {
	if asciiGlyphs {
		return "Enter"
	}
	return "↵"
}

func upDownKeys() string {
	if asciiGlyphs {
		return "Up/Dn"
	}
	return "↑↓"
}

func leftRightKeys() string {
	if asciiGlyphs {
		return "Left/Right"
	}
	return "←/→"
}

// asciiFolds maps every other chrome glyph onto an ASCII character of the same
// cell width, so folding a rendered line never moves a column.
var asciiFolds = strings.NewReplacer(
	// states
	glyphWaiting, "!", glyphRunning, "~", glyphReady, "*", glyphIdle, "o", glyphUnknown, "?",
	// rail structure
	glyphOpen, "|", glyphCursor, ">", "▸", ">", "▾", "v", "◂", "<", "◦", ".",
	"⛁", "#", "⚙", "%", glyphExternal, "^", "◇", "^", "›", ">", "‹", "<",
	// rules and boxes
	"─", "-", "━", "=", "│", "|", "┬", "+", "┴", "+", "├", "|", "└", "`",
	"┌", "+", "┐", "+", "┘", "+", "╭", "+", "╮", "+", "╯", "+", "╰", "+", "▏", "|",
	// keys and punctuation
	"⌥ ", "M-", "⌥", "M", "↑", "^", "↓", "v", "←", "<", "→", ">", "↵", "E",
	"·", ".", "…", ".", "—", "-", "–", "-", "×", "x", "•", "*", "≥", ">", "＋", "+ ",
	// PR check markers the daemon writes into a session's status (prwatch):
	// CI✓ / CI✗ / CI… read CI+ / CIx / CI. in ASCII.
	"✓", "+", "✗", "x",
)

// foldASCII rewrites chrome for the ASCII fallback; a no-op otherwise.
func foldASCII(s string) string {
	if !asciiGlyphs {
		return s
	}
	return asciiFolds.Replace(s)
}

// boxStyle is the border for modal boxes: square corners, drawn in the focus
// color because a modal holds the keyboard.
func boxStyle() lipgloss.Style {
	return lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(palettes[activeTheme].focus)
}
