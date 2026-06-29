package nativetui

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/vt"
)

// keyToBytes translates a Bubble Tea key event into the bytes a terminal sends
// to a child process, so keystrokes can be forwarded to the embedded agent.
func keyToBytes(k tea.KeyMsg) []byte {
	// Meta/Alt combos that aren't bound to navigation are sent as ESC + the base
	// key, the way a terminal encodes them (e.g. Alt+b → ESC b).
	if k.Alt && k.Type == tea.KeyRunes {
		return append([]byte{0x1b}, []byte(string(k.Runes))...)
	}
	switch k.Type {
	case tea.KeyRunes:
		return []byte(string(k.Runes))
	case tea.KeySpace:
		return []byte(" ")
	case tea.KeyEnter:
		return []byte("\r")
	case tea.KeyTab:
		return []byte("\t")
	case tea.KeyShiftTab:
		return []byte("\x1b[Z")
	case tea.KeyEsc:
		return []byte{0x1b}
	case tea.KeyBackspace:
		return []byte{0x7f}
	case tea.KeyDelete:
		return []byte("\x1b[3~")
	case tea.KeyUp:
		return []byte("\x1b[A")
	case tea.KeyDown:
		return []byte("\x1b[B")
	case tea.KeyRight:
		return []byte("\x1b[C")
	case tea.KeyLeft:
		return []byte("\x1b[D")
	case tea.KeyHome:
		return []byte("\x1b[H")
	case tea.KeyEnd:
		return []byte("\x1b[F")
	case tea.KeyPgUp:
		return []byte("\x1b[5~")
	case tea.KeyPgDown:
		return []byte("\x1b[6~")
	}
	// Any remaining C0 control chord (ctrl+a … ctrl+z, ctrl+o, ctrl+t, ctrl+space,
	// …): Bubble Tea encodes these as a KeyType equal to the control byte itself
	// (0x00–0x1f), so forwarding that byte hands the agent the exact chord — this
	// is what makes Claude Code's ctrl+o and friends work while the pane is
	// focused. ctrl+alt+x is the same byte prefixed with ESC. (Runes and the named
	// keys above use negative KeyTypes, so they never reach here.)
	if k.Type >= 0 && k.Type <= 0x1f {
		if k.Alt {
			return []byte{0x1b, byte(k.Type)}
		}
		return []byte{byte(k.Type)}
	}
	return nil
}

// mouseToVT translates a Bubble Tea mouse event into the emulator's mouse event
// type at pane-local 0-based coordinates (x, y), preserving button and
// modifiers. The emulator only emits bytes to the child if it has enabled mouse
// reporting, so this is a no-op for agents that don't track the mouse.
func mouseToVT(ev tea.MouseMsg, x, y int) vt.Mouse {
	btn, mod := mouseButton(ev.Button), mouseMod(ev)
	switch {
	case tea.MouseEvent(ev).IsWheel():
		return vt.MouseWheel{X: x, Y: y, Button: btn, Mod: mod}
	case ev.Action == tea.MouseActionRelease:
		return vt.MouseRelease{X: x, Y: y, Button: btn, Mod: mod}
	case ev.Action == tea.MouseActionMotion:
		return vt.MouseMotion{X: x, Y: y, Button: btn, Mod: mod}
	default:
		return vt.MouseClick{X: x, Y: y, Button: btn, Mod: mod}
	}
}

func mouseButton(b tea.MouseButton) vt.MouseButton {
	switch b {
	case tea.MouseButtonLeft:
		return vt.MouseLeft
	case tea.MouseButtonMiddle:
		return vt.MouseMiddle
	case tea.MouseButtonRight:
		return vt.MouseRight
	case tea.MouseButtonWheelUp:
		return vt.MouseWheelUp
	case tea.MouseButtonWheelDown:
		return vt.MouseWheelDown
	case tea.MouseButtonWheelLeft:
		return vt.MouseWheelLeft
	case tea.MouseButtonWheelRight:
		return vt.MouseWheelRight
	case tea.MouseButtonBackward:
		return vt.MouseBackward
	case tea.MouseButtonForward:
		return vt.MouseForward
	default:
		return vt.MouseNone
	}
}

func mouseMod(ev tea.MouseMsg) vt.KeyMod {
	var mod vt.KeyMod
	if ev.Shift {
		mod |= vt.ModShift
	}
	if ev.Alt {
		mod |= vt.ModAlt
	}
	if ev.Ctrl {
		mod |= vt.ModCtrl
	}
	return mod
}
