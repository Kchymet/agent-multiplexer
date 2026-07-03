package core

import "testing"

// TestBranchFor pins the single-source branch scheme: amux/<root>-<agent>, flat
// (a hyphen, not a slash after the root) so it can't collide with a legacy
// amux/<root> ref. Changing the scheme is a change to BranchFor alone.
func TestBranchFor(t *testing.T) {
	if got, want := BranchFor("r1", "a1"), "amux/r1-a1"; got != want {
		t.Fatalf("BranchFor(r1, a1) = %q, want %q", got, want)
	}
}
