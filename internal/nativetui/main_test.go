package nativetui

import (
	"os"
	"testing"
)

// The suite predates the compact rail and drives pane mechanics (selection,
// scrollback, the hardware cursor) in deliberately tiny terminals with the
// full rail's coordinates. Pin the full rail for it; the compact layout has
// its own tests, which opt back in with compactRailForTest.
func TestMain(m *testing.M) {
	compactBelow = 0
	os.Exit(m.Run())
}

func compactRailForTest(t *testing.T) {
	t.Helper()
	compactBelow = 72
	t.Cleanup(func() { compactBelow = 0 })
}
