package codexapp

import (
	"strings"
	"testing"
)

// AtSafePoint is the App Server half of "idle between turns" for harness
// restarts and daemon self-update: no active turn (ours or a peer's), no
// prompt of ours between turn/start and completion, no open approval.
func TestSupervisorAtSafePoint(t *testing.T) {
	s := New(Config{SessionID: "a1"})
	if ok, why := s.AtSafePoint(); ok || !strings.Contains(why, "no thread") {
		t.Fatalf("threadless supervisor: %v %q", ok, why)
	}
	s.mu.Lock()
	s.threadID = "thread-1"
	s.mu.Unlock()
	if ok, why := s.AtSafePoint(); !ok {
		t.Fatalf("idle supervisor not at a safe point: %q", why)
	}

	steps := []struct {
		name  string
		set   func()
		clear func()
		why   string
	}{
		{"active turn", func() { s.curTurn = "turn-1" }, func() { s.curTurn = "" }, "mid-turn"},
		{"prompt starting", func() { s.turnDone = make(chan *turnResult, 1) }, func() { s.turnDone = nil }, "prompt is starting"},
	}
	for _, step := range steps {
		s.mu.Lock()
		step.set()
		s.mu.Unlock()
		if ok, why := s.AtSafePoint(); ok || !strings.Contains(why, step.why) {
			t.Fatalf("%s: %v %q", step.name, ok, why)
		}
		s.mu.Lock()
		step.clear()
		s.mu.Unlock()
	}

	if !s.approvals.register(&pendingApproval{key: "req-1", threadID: "thread-1", turnID: "turn-1"}) {
		t.Fatal("register approval")
	}
	if ok, why := s.AtSafePoint(); ok || !strings.Contains(why, "approval") {
		t.Fatalf("open approval: %v %q", ok, why)
	}
	s.approvals.drainForTurn("turn-1")
	if ok, why := s.AtSafePoint(); !ok {
		t.Fatalf("after approval cleared: %q", why)
	}
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	if ok, _ := s.AtSafePoint(); ok {
		t.Fatal("closed supervisor at a safe point")
	}
}

// CanResume mirrors the launch's resume decision: a structured identity that
// ran a turn (or a legacy one) resumes; a known-unrun thread starts fresh.
func TestCanResumeFollowsIdentity(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	if CanResume("a1", "") {
		t.Fatal("no identity, no legacy thread: resumable")
	}
	if !CanResume("a1", "legacy-thread") {
		t.Fatal("legacy PTY conversation not adopted for resume")
	}
}
