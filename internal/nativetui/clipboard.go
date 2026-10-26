package nativetui

import "amux/internal/vterm"

// bindClipboard keeps app-copy callbacks on the feed goroutine independent of
// model state: only the terminal published by the renderer may copy.
func (m *model) bindClipboard(t *vterm.Terminal) {
	if clip := m.clip; clip != nil {
		t.OnClipboard(func(text []byte) {
			if m.shown.Load() == t {
				clip(text)
			}
		})
	}
}
