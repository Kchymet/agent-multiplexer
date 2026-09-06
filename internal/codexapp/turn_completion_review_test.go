package codexapp

import (
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

// The positive case: a completion that names our pinned thread AND our tracked turn
// wakes the Prompt exactly once and clears the tracked turn.
func TestOwnTurnCompletionDeliversAndClears(t *testing.T) {
	s := New(Config{})
	s.threadID = "our-thread"
	s.curTurn = "our-turn"
	s.turnDone = make(chan *turnResult, 1)
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
	if s.curTurn != "" || s.turnDone != nil {
		t.Errorf("own completion did not clear turn: curTurn=%q done=%v", s.curTurn, s.turnDone)
	}
}
