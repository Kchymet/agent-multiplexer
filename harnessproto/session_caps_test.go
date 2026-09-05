package harnessproto

import (
	"encoding/json"
	"testing"
)

// TestSessionCapsAdditive locks the backward-compatibility contract for the
// AGE-178 per-session runtime/caps fields: they are additive, so a frame from a
// provider that predates them (no "runtime", no "caps") must still decode, with
// Runtime empty and Caps nil — the signal a consumer uses to pick its
// conservative fallback instead of trusting an absent capability.
func TestSessionCapsAdditive(t *testing.T) {
	// An old provider's session frame: none of the new keys present.
	const oldFrame = `{"id":"a1","title":"t","source":"workspace","kind":"claude",
		"status":"ready","cwd":"/x","startedAt":1,"canAttach":true,"canKill":true,"canResume":false}`
	var s Session
	if err := json.Unmarshal([]byte(oldFrame), &s); err != nil {
		t.Fatalf("decode old frame: %v", err)
	}
	if s.Runtime != "" {
		t.Errorf("old frame Runtime = %q, want empty (unknown ⇒ fall back to Kind)", s.Runtime)
	}
	if s.Caps != nil {
		t.Errorf("old frame Caps = %+v, want nil (unknown ⇒ conservative fallback)", s.Caps)
	}
	// The distinction the pointer buys us: "known, all off" is a non-nil block, not
	// nil, so a consumer can tell it apart from "old provider, unknown".
	off := Session{Caps: &SessionCaps{}}
	if off.Caps == nil {
		t.Fatal("a stated all-false caps block must be non-nil")
	}
}

// TestSessionCapsRoundTrip checks the new fields survive a marshal/unmarshal
// cycle unchanged — the property a consumer relies on across provider → broker.
func TestSessionCapsRoundTrip(t *testing.T) {
	in := Session{
		ID: "a2", Kind: RuntimeCodex, Runtime: RuntimeCodex,
		Caps: &SessionCaps{Prompt: true, Interject: false, Cancel: true, Permission: false},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Session
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Runtime != RuntimeCodex {
		t.Errorf("Runtime = %q, want %q", out.Runtime, RuntimeCodex)
	}
	if out.Caps == nil || *out.Caps != *in.Caps {
		t.Errorf("Caps = %+v, want %+v", out.Caps, in.Caps)
	}
}
