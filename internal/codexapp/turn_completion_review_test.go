package codexapp

import (
	"context"
	"encoding/json"
	"testing"
)

// TestIndependentReviewCompletionCannotFinishAnotherTurn is ROOT's turn-completion
// audit reproducer (bin/root-coordination/turn-completion-review.go.txt): a
// turn/completed for a foreign thread, or for a peer turn on our own thread, must
// NOT satisfy the local Prompt, clear our control target, or remove the waiter.
func TestIndependentReviewCompletionCannotFinishAnotherTurn(t *testing.T) {
	for _, tc := range []struct{ name, thread, turn string }{
		{"foreign-thread", "other-thread", "our-turn"},
		{"different-turn", "our-thread", "peer-turn"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(Config{})
			s.threadID = "our-thread"
			s.curTurn = "our-turn"
			s.turnDone = make(chan *turnResult, 1)
			done := s.turnDone
			params, _ := json.Marshal(map[string]any{"threadId": tc.thread, "turn": map[string]any{"id": tc.turn, "status": "completed"}})
			s.onNotify("turn/completed", params)
			select {
			case r := <-done:
				t.Errorf("unrelated completion satisfied local Prompt: %#v", r)
			default:
			}
			if s.curTurn != "our-turn" {
				t.Errorf("unrelated completion cleared control target: %q", s.curTurn)
			}
			if s.turnDone != done {
				t.Error("unrelated completion removed local waiter")
			}
		})
	}
}

// A completion carrying NO correlating ids (empty {}) is not implicitly ours — a
// missing id must never silently satisfy the waiter (the old unconditional path did).
func TestCompletionWithoutIDsNotOurs(t *testing.T) {
	s := New(Config{})
	s.threadID = "our-thread"
	s.curTurn = "our-turn"
	s.turnDone = make(chan *turnResult, 1)
	done := s.turnDone
	s.onNotify("turn/completed", json.RawMessage(`{"turn":{"status":"completed"}}`))
	select {
	case r := <-done:
		t.Errorf("id-less completion satisfied local Prompt: %#v", r)
	default:
	}
	if s.curTurn != "our-turn" || s.turnDone != done {
		t.Errorf("id-less completion mutated turn state: curTurn=%q done==orig:%v", s.curTurn, s.turnDone == done)
	}
}

// The positive case: a completion that names our pinned thread AND the turn our
// Prompt bound as its own (ownTurn) wakes the Prompt exactly once and clears both the
// observed active turn and the local ownership.
func TestOwnTurnCompletionDeliversAndClears(t *testing.T) {
	s := New(Config{})
	s.threadID = "our-thread"
	s.curTurn = "our-turn"
	s.turnDone = make(chan *turnResult, 1)
	s.ownTurn = "our-turn" // our Prompt's turn/start response bound this turn
	done := s.turnDone
	s.onNotify("turn/completed", json.RawMessage(`{"threadId":"our-thread","turn":{"id":"our-turn","status":"completed"}}`))
	select {
	case r := <-done:
		if r == nil || r.TurnID != "our-turn" || r.StopReason != "completed" {
			t.Fatalf("delivered turn result = %#v", r)
		}
	default:
		t.Fatal("our own completion did not satisfy the local Prompt")
	}
	if s.curTurn != "" || s.turnDone != nil || s.ownTurn != "" {
		t.Errorf("own completion did not clear turn: curTurn=%q done=%v ownTurn=%q", s.curTurn, s.turnDone, s.ownTurn)
	}
}

// TestRootFollowupPeerTurnDoesNotOwnPendingPrompt is ROOT's follow-up reproducer
// (bin/root-coordination/pending-turn-ownership-review.go.txt): while our turn/start
// response is still in flight (ownership unbound), a peer's observed turn/started then
// turn/completed must NOT satisfy or remove our pending local waiter — observed
// active-turn tracking is separate from local-request ownership.
func TestRootFollowupPeerTurnDoesNotOwnPendingPrompt(t *testing.T) {
	s := New(Config{})
	s.threadID = "our-thread"
	s.resumable = true
	// Our turn/start request is in flight, but a peer's turn is observed first.
	done := make(chan *turnResult, 1)
	s.turnDone = done
	s.onNotify("turn/started", json.RawMessage(`{"threadId":"our-thread","turn":{"id":"peer-turn","items":[],"status":"inProgress"}}`))
	s.onNotify("turn/completed", json.RawMessage(`{"threadId":"our-thread","turn":{"id":"peer-turn","items":[],"status":"completed"}}`))
	select {
	case r := <-done:
		t.Errorf("peer lifecycle satisfied unbound local Prompt: %#v", r)
	default:
	}
	if s.turnDone != done {
		t.Error("peer completion removed unrelated local waiter")
	}
}

// A peer turn completing clears its OWN observed active-turn control state (curTurn)
// and its own approvals, but must not satisfy an unrelated pending local Prompt whose
// ownership is bound to a different turn.
func TestPeerCompletionClearsActiveButNotBoundWaiter(t *testing.T) {
	s := New(Config{})
	s.threadID = "our-thread"
	s.curTurn = "peer-turn" // a peer turn is the observed active turn
	s.ownTurn = "our-turn"  // but our Prompt owns a different (still-running) turn
	s.turnDone = make(chan *turnResult, 1)
	done := s.turnDone
	s.onNotify("turn/completed", json.RawMessage(`{"threadId":"our-thread","turn":{"id":"peer-turn","status":"completed"}}`))
	select {
	case r := <-done:
		t.Errorf("peer completion satisfied our bound waiter for a different turn: %#v", r)
	default:
	}
	if s.curTurn != "" {
		t.Errorf("peer completion did not clear the observed active turn: %q", s.curTurn)
	}
	if s.turnDone != done || s.ownTurn != "our-turn" {
		t.Errorf("peer completion disturbed our ownership: done==orig:%v ownTurn=%q", s.turnDone == done, s.ownTurn)
	}
}

// A foreign-thread turn/completed must not enter our event stream or mutate our state.
func TestForeignThreadCompletionDropped(t *testing.T) {
	s := New(Config{})
	s.threadID = "our-thread"
	s.curTurn = "our-turn"
	s.ownTurn = "our-turn"
	s.turnDone = make(chan *turnResult, 1)
	done := s.turnDone
	col := subscribeCollector(context.Background(), s)
	s.onNotify("turn/completed", json.RawMessage(`{"threadId":"other-thread","turn":{"id":"our-turn","status":"completed"}}`))
	select {
	case r := <-done:
		t.Errorf("foreign completion satisfied local Prompt: %#v", r)
	default:
	}
	if s.curTurn != "our-turn" || s.turnDone != done || s.ownTurn != "our-turn" {
		t.Errorf("foreign completion mutated our state: curTurn=%q done==orig:%v ownTurn=%q", s.curTurn, s.turnDone == done, s.ownTurn)
	}
	// Nothing from the foreign thread should reach our stream.
	if ev, ok := col.tryGet(); ok {
		t.Errorf("foreign completion contaminated the stream: %+v", ev)
	}
}
