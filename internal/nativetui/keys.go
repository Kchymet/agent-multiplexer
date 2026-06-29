package nativetui

import tea "github.com/charmbracelet/bubbletea"

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
	case tea.KeyCtrlA:
		return []byte{0x01}
	case tea.KeyCtrlC:
		return []byte{0x03}
	case tea.KeyCtrlD:
		return []byte{0x04}
	case tea.KeyCtrlU:
		return []byte{0x15}
	case tea.KeyCtrlK:
		return []byte{0x0b}
	case tea.KeyCtrlW:
		return []byte{0x17}
	case tea.KeyCtrlE:
		return []byte{0x05}
	case tea.KeyCtrlL:
		return []byte{0x0c}
	case tea.KeyCtrlR:
		return []byte{0x12}
	case tea.KeyCtrlZ:
		return []byte{0x1a}
	}
	return nil
}
