package store

import "testing"

func TestSessionPRStateRoundTrip(t *testing.T) {
	db := openTemp(t)
	if err := db.PutSession(Session{ID: "m1", RootID: "wg", Agent: "claude"}); err != nil {
		t.Fatal(err)
	}
	if err := db.PutSessionPRState("m1", `[{"pr":{"number":1}}]`, 5); err != nil {
		t.Fatal(err)
	}
	if err := db.PutSessionPRState("m1", `[{"pr":{"number":2}}]`, 6); err != nil {
		t.Fatal(err)
	}
	got, err := db.SessionPRStates()
	if err != nil || got["m1"] != `[{"pr":{"number":2}}]` {
		t.Fatalf("SessionPRStates = %v, %v", got, err)
	}
	if err := db.DeleteSession("m1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.SessionPRStates(); len(got) != 0 {
		t.Fatalf("state survived session delete: %v", got)
	}
	if err := db.PutSessionPRState("m2", `x`, 1); err != nil {
		t.Fatal(err)
	}
	if err := db.PutSessionPRState("m2", "", 2); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.SessionPRStates(); len(got) != 0 {
		t.Fatalf("empty state did not delete: %v", got)
	}
}
