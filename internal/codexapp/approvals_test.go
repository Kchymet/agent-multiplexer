package codexapp

import (
	"encoding/json"
	"testing"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

func TestApprovalLocalAnswerAwaitsServerConfirmation(t *testing.T) {
	a := newApprovalTracker()
	a.register(&pendingApproval{key: "r1", rawID: json.RawMessage(`"r1"`), threadID: "t"})

	if got := a.open(); len(got) != 1 || got[0] != "r1" {
		t.Fatalf("open = %v", got)
	}
	// Local answer: recorded, but NOT yet resolved (awaiting the server notice).
	if _, err := a.answerLocally("r1"); err != nil {
		t.Fatalf("answerLocally: %v", err)
	}
	if got := a.open(); len(got) != 0 {
		t.Fatalf("an answered request must not be re-answerable/open: %v", got)
	}
	// A second local answer is a duplicate.
	if _, err := a.answerLocally("r1"); err != errDuplicateApproval {
		t.Fatalf("second answerLocally = %v, want duplicate", err)
	}
	// The server confirms — resolves exactly once (no decision returned; the notice
	// names no winner).
	if !a.serverResolved("r1") {
		t.Fatal("serverResolved should resolve a live answered request")
	}
	// A duplicate server notification is ignored (no second emit).
	if a.serverResolved("r1") {
		t.Fatal("duplicate serverResolved should be ignored")
	}
	// A local answer after resolution is a duplicate (stale answer rejected).
	if _, err := a.answerLocally("r1"); err != errDuplicateApproval {
		t.Fatalf("answerLocally after resolution = %v, want duplicate", err)
	}
}

func TestApprovalExternalResolutionClears(t *testing.T) {
	a := newApprovalTracker()
	a.register(&pendingApproval{key: "42", rawID: json.RawMessage(`42`), threadID: "thr_1"})

	// Another client answered — we never answered locally. The server notice clears
	// it (the tracker reports only that it was resolved; the winner is unknown).
	if !a.serverResolved("42") {
		t.Fatal("external serverResolved should clear a pending request")
	}
	// Now stale to a local answer.
	if _, err := a.answerLocally("42"); err != errDuplicateApproval {
		t.Fatalf("answerLocally after external resolution = %v, want duplicate", err)
	}
}

func TestApprovalStaleAndUnknown(t *testing.T) {
	a := newApprovalTracker()
	if _, err := a.answerLocally("nope"); err != errStaleApproval {
		t.Fatalf("answerLocally(unknown) = %v, want stale", err)
	}
	if a.serverResolved("nope") {
		t.Fatal("serverResolved(unknown) should be ignored")
	}
}

func TestApprovalDrainOutstandingIsNeutral(t *testing.T) {
	a := newApprovalTracker()
	a.register(&pendingApproval{key: "pend", threadID: "t"})
	a.register(&pendingApproval{key: "ans", threadID: "t"})
	_, _ = a.answerLocally("ans")

	got := map[string]bool{}
	for _, k := range a.drainOutstanding() {
		got[k] = true
	}
	if !got["pend"] || !got["ans"] {
		t.Fatalf("drainOutstanding did not return all live ids: %v", got)
	}
	// After draining, nothing is live and a late server notice is ignored.
	if len(a.open()) != 0 {
		t.Fatal("open should be empty after drain")
	}
	if a.serverResolved("pend") {
		t.Fatal("late serverResolved after drain should be ignored")
	}
}

func TestApprovalRegisterIgnoresRepeatID(t *testing.T) {
	a := newApprovalTracker()
	a.register(&pendingApproval{key: "r1", method: "first"})
	a.register(&pendingApproval{key: "r1", method: "second"})
	p, err := a.answerLocally("r1")
	if err != nil {
		t.Fatalf("answerLocally: %v", err)
	}
	if p.method != "first" {
		t.Fatalf("repeat register should keep the first, got %q", p.method)
	}
}

func TestWinningDecision(t *testing.T) {
	cases := map[string]string{
		"":        harnessproto.DecisionCleared,
		"accept":  harnessproto.DecisionAllow,
		"allow":   harnessproto.DecisionAllow,
		"decline": harnessproto.DecisionDeny,
		"reject":  harnessproto.DecisionDeny,
		"deny":    harnessproto.DecisionDeny,
		"weird":   harnessproto.DecisionCleared,
	}
	for in, want := range cases {
		if got := winningDecision(in); got != want {
			t.Errorf("winningDecision(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIDKey(t *testing.T) {
	if got := idKey(json.RawMessage(`"abc"`)); got != "abc" {
		t.Fatalf("string id key = %q", got)
	}
	if got := idKey(json.RawMessage(`42`)); got != "42" {
		t.Fatalf("numeric id key = %q", got)
	}
}
