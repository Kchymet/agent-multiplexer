package agent

import (
	"testing"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// TestCapsFor pins the per-kind control surface amux advertises. The caps must
// track the steering Keys: a runtime amux can drive by keystroke reports the
// verbs those keys serve; one it cannot drive reports all-false so a consumer
// disables the controls instead of failing on them.
func TestCapsFor(t *testing.T) {
	steerable := harnessproto.SessionCaps{Prompt: true, Interject: true, Cancel: true, Permission: true}
	claude := harnessproto.SessionCaps{Prompt: true, Interject: true, Cancel: true, Permission: false}
	for _, tc := range []struct {
		kind string
		want harnessproto.SessionCaps
	}{
		// Claude's session-controlled hooks are observations, not an authoritative
		// permission source. Codex has structured supervisor-owned approvals.
		{"claude", claude},
		{"codex", steerable},
		// An empty kind resolves to the default (claude).
		{"", claude},
		// Hermes has no steering keys (noopHarness), so nothing is advertised.
		{"hermes", harnessproto.SessionCaps{}},
		// An unrecognized kind is a no-op harness: honestly all-false.
		{"totally-unknown", harnessproto.SessionCaps{}},
	} {
		if got := CapsFor(tc.kind); got != tc.want {
			t.Errorf("CapsFor(%q) = %+v, want %+v", tc.kind, got, tc.want)
		}
	}
}

// TestPermissionRequiresAnswerKeys guards the rule that Permission is never
// advertised without both an allow and a deny key: the daemon must never be told
// it can answer a prompt it could only guess at. It is a white-box check on the
// derivation, so a future runtime that gains a transcript but lacks a deny key
// still reports Permission=false.
func TestPermissionRequiresAnswerKeys(t *testing.T) {
	if correlatesPermissions("claude") {
		t.Fatal("session-controlled Claude observations must not correlate permissions")
	}
	// Claude has both keys but no authoritative prompt source, so permission is off.
	if CapsFor("claude").Permission {
		t.Error("claude must not advertise Permission")
	}
	// A runtime that correlates but is missing an answer key would be caught by the
	// len(Allow)/len(Deny) guard in CapsFor; assert the guard is actually consulted
	// by checking a no-key harness with a correlating name never slips through.
	if CapsFor("hermes").Permission {
		t.Error("a harness without answer keys must not advertise Permission")
	}
}
