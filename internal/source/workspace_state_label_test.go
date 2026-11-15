package source

import (
	"testing"

	"amux/internal/core"
)

// Each state is shown under its own name; unknown must not borrow "running".
func TestStateLabelNamesEachState(t *testing.T) {
	for _, st := range []string{core.StateIdle, core.StateReady, core.StateWaiting, core.StateRunning, core.StateUnknown} {
		if got := stateLabel(st); got != st {
			t.Errorf("stateLabel(%q) = %q", st, got)
		}
	}
}
