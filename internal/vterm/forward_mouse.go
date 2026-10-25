package vterm

import "github.com/charmbracelet/x/vt"

// ForwardMouse sends an event belonging to a gesture the embedder has already
// assigned to the child. Unlike MouseEvent it does not reconsider Shift on each
// event: a Shift-modified release must still end an app-owned press. Encoding
// continues to honor the child's current reporting modes and event modifiers.
func (t *Terminal) ForwardMouse(ev vt.Mouse) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || !t.mouseTracked() {
		return
	}
	t.scrollTo(0)
	t.emu.SendMouse(ev)
}
