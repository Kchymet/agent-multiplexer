package harnessproto

import (
	"encoding/json"
	"net"
	"reflect"
	"testing"
)

// The steering verbs (prompt, interject, stop, permission) are protocol, not
// convention: the orchestrator encodes them and a daemon in another repo decodes
// them. These tests pin the wire strings, the field keys, and a full round trip
// of each verb, so renaming any of them fails here rather than in a silent
// no-op at the far end of a TLS connection.

// TestSteeringVerbStrings pins the exact wire spelling of every steering verb,
// its field keys, and the permission decisions. A Go-side rename is free; a
// change to these string literals is a wire break.
func TestSteeringVerbStrings(t *testing.T) {
	cases := []struct{ got, want string }{
		{VerbPrompt, "prompt"},
		{VerbInterject, "interject"},
		{VerbStop, "stop"},
		{VerbPermission, "permission"},
		{FieldText, "text"},
		{FieldRequestID, "request_id"},
		{FieldDecision, "decision"},
		{FieldReason, "reason"},
		{DecisionAllow, "allow"},
		{DecisionDeny, "deny"},
		{ResultApplied, "applied"},
		{ResultAccepted, "accepted"},
		{ErrUnsupportedVerb, "unsupported verb"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("wire string = %q, want %q", c.got, c.want)
		}
	}
}

// TestSteeringVerbsAccepted asserts the steering verbs joined the closed set —
// an orchestrator screens against SessionVerbs before sending, so a verb missing
// here is unsendable — and that SteeringVerbs is exactly the new four.
func TestSteeringVerbsAccepted(t *testing.T) {
	want := map[string]bool{VerbPrompt: true, VerbInterject: true, VerbStop: true, VerbPermission: true}
	if !reflect.DeepEqual(SteeringVerbs, want) {
		t.Fatalf("SteeringVerbs = %v, want %v", SteeringVerbs, want)
	}
	for v := range SteeringVerbs {
		if !SessionVerbs[v] {
			t.Errorf("steering verb %q missing from SessionVerbs", v)
		}
	}
	// The lifecycle verbs stay accepted and stay non-steering.
	for _, v := range []string{VerbNewWorkgroup, VerbAddAgent, VerbRename, VerbArchive, VerbUnarchive, VerbStart} {
		if !SessionVerbs[v] {
			t.Errorf("lifecycle verb %q dropped from SessionVerbs", v)
		}
		if SteeringVerbs[v] {
			t.Errorf("lifecycle verb %q wrongly listed as steering", v)
		}
	}
	// A pane verb is still outside the set (the extension carries no pane access).
	for _, v := range []string{MSpawn, MInput, MResize, MKill, "pane.write"} {
		if SessionVerbs[v] {
			t.Errorf("pane verb %q must not be an accepted session verb", v)
		}
	}
}

// TestSteeringActionRoundTrip sends each steering verb over a real connection and
// asserts it decodes byte-for-byte on the far side, fields included.
func TestSteeringActionRoundTrip(t *testing.T) {
	a, b := net.Pipe()
	prov, orch := NewConn(a), NewConn(b)
	defer prov.Close()
	defer orch.Close()

	actions := []MuxMsg{
		{Type: MSessionAction, ReqID: "r1", Action: VerbPrompt, ID: "a2",
			Fields: map[string]string{FieldText: "run the tests"}},
		{Type: MSessionAction, ReqID: "r2", Action: VerbInterject, ID: "a2",
			Fields: map[string]string{FieldText: "skip the flaky one"}},
		{Type: MSessionAction, ReqID: "r3", Action: VerbStop, ID: "a2"},
		{Type: MSessionAction, ReqID: "r4", Action: VerbPermission, ID: "a2",
			Fields: map[string]string{FieldRequestID: "perm-9", FieldDecision: DecisionDeny, FieldReason: "writes outside the worktree"}},
	}
	for _, want := range actions {
		go func() { _ = orch.WriteMux(want) }() // net.Pipe is unbuffered
		got, err := prov.ReadMux()
		if err != nil {
			t.Fatalf("read %s: %v", want.Action, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s round-trip mismatch:\n got  %+v\n want %+v", want.Action, got, want)
		}
	}

	// And the results that answer them, including the disposition field.
	results := []HarnessMsg{
		{Type: HSessionResult, ReqID: "r1", OK: true, Result: ResultAccepted},
		{Type: HSessionResult, ReqID: "r3", Error: ErrUnsupportedVerb},
		{Type: HSessionResult, ReqID: "r4", Error: "no pending permission request perm-9"},
	}
	for _, want := range results {
		go func() { _ = prov.WriteHarness(want) }()
		got, err := orch.ReadHarness()
		if err != nil {
			t.Fatalf("read result %s: %v", want.ReqID, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("session-result round-trip mismatch:\n got  %+v\n want %+v", got, want)
		}
	}
}

// TestSessionResultDispositionCompat guards the additive promise: a result with
// no disposition marshals exactly as it did before the field existed, and an
// old daemon's bare {ok:true} still decodes (Result empty, meaning applied).
func TestSessionResultDispositionCompat(t *testing.T) {
	b, err := json.Marshal(HarnessMsg{Type: HSessionResult, ReqID: "r7", OK: true})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"type":"session-result","reqId":"r7","ok":true}`; string(b) != want {
		t.Fatalf("bare success = %s, want %s (result must be omitempty)", b, want)
	}

	var old HarnessMsg
	if err := json.Unmarshal([]byte(`{"type":"session-result","reqId":"r7","ok":true}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.Result != "" {
		t.Fatalf("pre-field result = %q, want empty (read as %q)", old.Result, ResultApplied)
	}
}
