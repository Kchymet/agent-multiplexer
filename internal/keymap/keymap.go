// Package keymap owns the native TUI's global hotkeys: the built-in defaults,
// the user's overrides in config.json ("keys"), and the read/validate/write
// plumbing behind `amux config`. Only the global chords — the ones that work
// even while an agent is focused — are configurable; sidebar-only keys (j/k,
// enter, a, w, …) never collide with agent input, so they stay fixed.
package keymap

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"amux/internal/amuxcfg"
	"amux/internal/core"
)

// Actions — the config key under "keys" is "keys.<action>" on the CLI.
const (
	FocusRail   = "focus-rail"
	FocusAgent  = "focus-agent"
	ToggleFocus = "toggle-focus"
	Quit        = "quit"
	TabAgent    = "tab-agent"
	TabEditor   = "tab-editor"
	TabTerm     = "tab-term"
)

// actions lists every configurable action in display order, with the help
// text `amux config` prints.
var actions = []struct{ name, desc string }{
	{FocusRail, "focus the sidebar rail"},
	{FocusAgent, "focus the agent pane"},
	{ToggleFocus, "toggle focus between rail and agent"},
	{Quit, "quit the dashboard"},
	{TabAgent, "switch to the agent tab"},
	{TabEditor, "switch to the editor tab"},
	{TabTerm, "switch to the terminal tab"},
}

var defaults = map[string]string{
	FocusRail:   "alt+h",
	FocusAgent:  "alt+l",
	ToggleFocus: "alt+a",
	Quit:        "alt+q",
	TabAgent:    "alt+1",
	TabEditor:   "alt+2",
	TabTerm:     "alt+3",
}

// Keymap is the effective action→chord map. The zero value behaves as the
// built-in defaults, so a model constructed without Load() still navigates.
type Keymap struct {
	chords map[string]string
}

func (k Keymap) resolved() map[string]string {
	if k.chords == nil {
		return defaults
	}
	return k.chords
}

// Chord returns the chord bound to an action ("" for an unknown action).
func (k Keymap) Chord(action string) string { return k.resolved()[action] }

// Action returns the action a chord (as Bubble Tea's KeyMsg.String() spells
// it) is bound to, or "" if the chord is unbound.
func (k Keymap) Action(chord string) string {
	for a, c := range k.resolved() {
		if c == chord {
			return a
		}
	}
	return ""
}

// Binding is one row of `amux config ls`.
type Binding struct {
	Action  string
	Chord   string
	Desc    string
	Default bool
}

// List returns every binding in display order, marking which still carry the
// built-in default.
func (k Keymap) List() []Binding {
	m := k.resolved()
	out := make([]Binding, 0, len(actions))
	for _, a := range actions {
		out = append(out, Binding{
			Action:  a.name,
			Chord:   m[a.name],
			Desc:    a.desc,
			Default: m[a.name] == defaults[a.name],
		})
	}
	return out
}

// Load reads config.json and overlays its "keys" section onto the defaults.
// It is deliberately forgiving: a missing file is the defaults, and invalid
// entries are skipped (reported in the returned error) rather than locking
// the user out of navigation.
func Load() (Keymap, error) {
	merged := make(map[string]string, len(defaults))
	for a, c := range defaults {
		merged[a] = c
	}
	raw, err := readConfig()
	if err != nil {
		return Keymap{merged}, err
	}
	var errs []string
	for action, chord := range raw {
		if !known(action) {
			errs = append(errs, fmt.Sprintf("unknown action %q", action))
			continue
		}
		n, err := Normalize(chord)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", action, err))
			continue
		}
		merged[action] = n
	}
	if dup := duplicate(merged); dup != "" {
		errs = append(errs, dup)
	}
	if len(errs) > 0 {
		return Keymap{merged}, fmt.Errorf("%s: %s", core.ConfigPath(), strings.Join(errs, "; "))
	}
	return Keymap{merged}, nil
}

// Set validates and persists one binding, returning the normalized chord. It
// refuses a chord already bound to a different action so two actions can never
// silently shadow each other.
func Set(action, chord string) (string, error) {
	if !known(action) {
		return "", fmt.Errorf("unknown action %q (see `amux config ls`)", action)
	}
	n, err := Normalize(chord)
	if err != nil {
		return "", err
	}
	overrides, err := readConfig()
	if err != nil {
		return "", err
	}
	if overrides == nil {
		overrides = map[string]string{}
	}
	overrides[action] = n
	// Check the collision against the *effective* map (defaults + overrides),
	// not just the file: rebinding onto a default chord is the common mistake.
	effective := make(map[string]string, len(defaults))
	for a, c := range defaults {
		effective[a] = c
	}
	for a, c := range overrides {
		if known(a) {
			if nc, err := Normalize(c); err == nil {
				effective[a] = nc
			}
		}
	}
	if dup := duplicate(effective); dup != "" {
		return "", errors.New(dup)
	}
	return n, writeConfig(overrides)
}

// Unset removes an override, restoring the action's built-in default.
func Unset(action string) error {
	if !known(action) {
		return fmt.Errorf("unknown action %q (see `amux config ls`)", action)
	}
	overrides, err := readConfig()
	if err != nil {
		return err
	}
	if _, ok := overrides[action]; !ok {
		return nil // already at the default
	}
	delete(overrides, action)
	return writeConfig(overrides)
}

// Normalize canonicalizes a user-typed chord into Bubble Tea's KeyMsg.String()
// spelling ("Option + L" → "alt+l") and validates it. Global chords must carry
// ctrl or alt (or be an f-key): an unmodified key would be stolen from the
// focused agent on every keystroke.
func Normalize(chord string) (string, error) {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(chord)), "+")
	var ctrl, alt, shift bool
	base := ""
	for _, p := range parts {
		switch p = strings.TrimSpace(p); p {
		case "ctrl", "control":
			ctrl = true
		case "alt", "opt", "option", "meta":
			alt = true
		case "shift":
			shift = true
		case "":
			return "", fmt.Errorf("invalid chord %q", chord)
		default:
			if base != "" {
				return "", fmt.Errorf("invalid chord %q: more than one base key (%q and %q)", chord, base, p)
			}
			base = p
		}
	}
	switch base {
	case "":
		return "", fmt.Errorf("invalid chord %q: no base key", chord)
	case "return":
		base = "enter"
	case "escape":
		base = "esc"
	case "pageup":
		base = "pgup"
	case "pagedown", "pgdn":
		base = "pgdown"
	}
	if !validBase(base) {
		return "", fmt.Errorf("invalid chord %q: unknown key %q", chord, base)
	}
	if !ctrl && !alt && !fkey(base) {
		return "", fmt.Errorf("chord %q needs ctrl or alt (or an f-key): an unmodified key would be swallowed before it reaches the agent", chord)
	}
	var b strings.Builder
	if ctrl {
		b.WriteString("ctrl+")
	}
	if alt {
		b.WriteString("alt+")
	}
	if shift {
		b.WriteString("shift+")
	}
	b.WriteString(base)
	return b.String(), nil
}

var namedKeys = map[string]bool{
	"enter": true, "tab": true, "esc": true, "space": true,
	"backspace": true, "delete": true, "insert": true,
	"up": true, "down": true, "left": true, "right": true,
	"home": true, "end": true, "pgup": true, "pgdown": true,
}

func validBase(base string) bool {
	if len([]rune(base)) == 1 {
		r := []rune(base)[0]
		return r > ' ' && r != '+'
	}
	return namedKeys[base] || fkey(base)
}

func fkey(base string) bool {
	if len(base) < 2 || base[0] != 'f' {
		return false
	}
	n := 0
	for _, r := range base[1:] {
		if r < '0' || r > '9' {
			return false
		}
		n = n*10 + int(r-'0')
	}
	return n >= 1 && n <= 20
}

func known(action string) bool {
	_, ok := defaults[action]
	return ok
}

// duplicate reports two actions sharing a chord, or "".
func duplicate(m map[string]string) string {
	seen := map[string]string{}
	// Iterate in display order so the error is deterministic.
	for _, a := range actions {
		c := m[a.name]
		if prev, ok := seen[c]; ok {
			return fmt.Sprintf("%q is bound to both %s and %s", c, prev, a.name)
		}
		seen[c] = a.name
	}
	return ""
}

// ---- config.json I/O -------------------------------------------------------
//
// The file is shared with future settings, so reads and writes go through a
// map[string]json.RawMessage that preserves any top-level fields this package
// doesn't own.

func readConfig() (map[string]string, error) {
	top, err := amuxcfg.Read()
	if err != nil {
		return nil, err
	}
	if top["keys"] == nil {
		return nil, nil
	}
	var keys map[string]string
	if err := json.Unmarshal(top["keys"], &keys); err != nil {
		return nil, fmt.Errorf("%s: \"keys\": %w", core.ConfigPath(), err)
	}
	return keys, nil
}

func writeConfig(keys map[string]string) error {
	top, err := amuxcfg.Read()
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		delete(top, "keys")
	} else {
		b, err := json.Marshal(keys) // map keys marshal sorted, so the file is stable
		if err != nil {
			return err
		}
		top["keys"] = b
	}
	return amuxcfg.Write(top)
}
