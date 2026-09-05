package codexapp

import (
	"encoding/json"
	"testing"
)

func TestApprovalTakeStaleAndDuplicate(t *testing.T) {
	a := newApprovalTracker()
	a.register(&pendingApproval{key: "r1", rawID: json.RawMessage(`"r1"`)})

	if got := a.open(); len(got) != 1 || got[0] != "r1" {
		t.Fatalf("open = %v", got)
	}
	if _, err := a.take("r1"); err != nil {
		t.Fatalf("first take: %v", err)
	}
	if _, err := a.take("r1"); err != errDuplicateApproval {
		t.Fatalf("second take err = %v, want duplicate", err)
	}
	if _, err := a.take("never"); err != errStaleApproval {
		t.Fatalf("unknown take err = %v, want stale", err)
	}
	if len(a.open()) != 0 {
		t.Fatal("open should be empty after take")
	}
}

func TestApprovalRegisterIgnoresRepeatID(t *testing.T) {
	a := newApprovalTracker()
	a.register(&pendingApproval{key: "r1", method: "first"})
	a.register(&pendingApproval{key: "r1", method: "second"})
	p, err := a.take("r1")
	if err != nil {
		t.Fatalf("take: %v", err)
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
