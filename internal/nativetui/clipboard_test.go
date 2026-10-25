package nativetui

import (
	"strings"
	"testing"
	"time"

	"amux/internal/core"
)

// An OSC 52 from the pane on screen reaches the clipboard; one from a pane
// that is open but hidden (another agent) is dropped, so a background agent
// cannot replace what the user copied.
func TestClipboardOnlyFromShownPane(t *testing.T) {
	got := make(chan string, 4)
	m := &model{client: &reconnectClient{}, w: 100, h: 30, dataCh: make(chan struct{}, 1),
		clip: func(b []byte) { got <- string(b) }}
	runReconnectCommands(m.launchPane("hidden", tabAgent))
	runReconnectCommands(m.launchPane("shown", tabAgent))
	defer func() {
		for _, term := range m.terms {
			term.Close()
		}
	}()
	m.View() // publishes the shown pane, as every frame does

	hidden := m.terms[paneKey{"hidden", tabAgent}]
	m.handlePaneFrame(&core.PaneFrame{Type: core.FramePaneOutput, PaneID: paneIDOf("hidden", tabAgent),
		Data: []byte("\x1b]52;c;aGlkZGVu\aparsed")}) // "hidden"
	m.handlePaneFrame(&core.PaneFrame{Type: core.FramePaneOutput, PaneID: paneIDOf("shown", tabAgent),
		Data: []byte("\x1b]52;c;c2hvd24=\a")}) // "shown"

	select {
	case text := <-got:
		if text != "shown" {
			t.Fatalf("clipboard got %q, want the shown pane's copy", text)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the shown pane's copy never reached the clipboard")
	}
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(hidden.Render(), "parsed") {
		if time.Now().After(deadline) {
			t.Fatal("hidden pane output never parsed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case text := <-got:
		t.Fatalf("hidden pane copied %q", text)
	default:
	}
}
