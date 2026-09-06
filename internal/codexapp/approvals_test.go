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
	if _, err := a.answerLocally("r1", harnessproto.DecisionAllow); err != nil {
		t.Fatalf("answerLocally: %v", err)
	}
	if got := a.open(); len(got) != 0 {
		t.Fatalf("an answered request must not be re-answerable/open: %v", got)
	}
	// A second local answer is a duplicate.
	if _, err := a.answerLocally("r1", harnessproto.DecisionDeny); err != errDuplicateApproval {
		t.Fatalf("second answerLocally = %v, want duplicate", err)
	}
	// The server confirms — surfaces the decision we sent, exactly once.
	dec, ok := a.serverResolved("r1")
	if !ok || dec != harnessproto.DecisionAllow {
		t.Fatalf("serverResolved = (%q,%v), want (allow,true)", dec, ok)
	}
	// A duplicate server notification is ignored (no second emit).
	if _, ok := a.serverResolved("r1"); ok {
		t.Fatal("duplicate serverResolved should be ignored")
	}
	// A local answer after resolution is a duplicate (stale answer rejected).
	if _, err := a.answerLocally("r1", harnessproto.DecisionAllow); err != errDuplicateApproval {
		t.Fatalf("answerLocally after resolution = %v, want duplicate", err)
	}
}

func TestApprovalExternalResolutionClears(t *testing.T) {
	a := newApprovalTracker()
	a.register(&pendingApproval{key: "42", rawID: json.RawMessage(`42`), threadID: "thr_1"})

	// Another client answered — we never answered locally. The server notice clears
	// it with an unknown direction (cleared).
	dec, ok := a.serverResolved("42")
	if !ok || dec != harnessproto.DecisionCleared {
		t.Fatalf("external serverResolved = (%q,%v), want (cleared,true)", dec, ok)
	}
	// Now stale to a local answer.
	if _, err := a.answerLocally("42", harnessproto.DecisionAllow); err != errDuplicateApproval {
		t.Fatalf("answerLocally after external resolution = %v, want duplicate", err)
	}
}

func TestApprovalStaleAndUnknown(t *testing.T) {
	a := newApprovalTracker()
	if _, err := a.answerLocally("nope", harnessproto.DecisionAllow); err != errStaleApproval {
		t.Fatalf("answerLocally(unknown) = %v, want stale", err)
	}
	if _, ok := a.serverResolved("nope"); ok {
		t.Fatal("serverResolved(unknown) should be ignored")
	}
}

func TestApprovalThreadOf(t *testing.T) {
	a := newApprovalTracker()
	a.register(&pendingApproval{key: "r1", threadID: "thr_1"})
	if tid, ok := a.threadOf("r1"); !ok || tid != "thr_1" {
		t.Fatalf("threadOf = (%q,%v)", tid, ok)
	}
	if _, ok := a.threadOf("missing"); ok {
		t.Fatal("threadOf(unknown) should be false")
	}
}

func TestApprovalDrainOutstanding(t *testing.T) {
	a := newApprovalTracker()
	a.register(&pendingApproval{key: "pend", threadID: "t"})
	a.register(&pendingApproval{key: "ans", threadID: "t"})
	_, _ = a.answerLocally("ans", harnessproto.DecisionDeny)

	got := map[string]string{}
	for _, r := range a.drainOutstanding() {
		got[r.key] = r.decision
	}
	if got["pend"] != harnessproto.DecisionCleared {
		t.Fatalf("pending drained as %q, want cleared", got["pend"])
	}
	if got["ans"] != harnessproto.DecisionDeny {
		t.Fatalf("answered drained as %q, want the sent decision deny", got["ans"])
	}
	// After draining, nothing is live and a late server notice is ignored.
	if len(a.open()) != 0 {
		t.Fatal("open should be empty after drain")
	}
	if _, ok := a.serverResolved("pend"); ok {
		t.Fatal("late serverResolved after drain should be ignored")
	}
}

func TestApprovalRegisterIgnoresRepeatID(t *testing.T) {
	a := newApprovalTracker()
	a.register(&pendingApproval{key: "r1", method: "first"})
	a.register(&pendingApproval{key: "r1", method: "second"})
	p, err := a.answerLocally("r1", harnessproto.DecisionAllow)
	if err != nil {
		t.Fatalf("answerLocally: %v", err)
	}
	if p.method != "first" {
		t.Fatalf("repeat register should keep the first, got %q", p.method)
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
