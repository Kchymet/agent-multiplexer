package harnessproto

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestAccountsV1Fixture(t *testing.T) {
	b, e := os.ReadFile("testdata/accounts-v1.json")
	if e != nil {
		t.Fatal(e)
	}
	var frames map[string]json.RawMessage
	if e = json.Unmarshal(b, &frames); e != nil {
		t.Fatal(e)
	}
	for _, key := range []string{"login", "poll", "complete", "disconnect", "status"} {
		var m MuxMsg
		if e = json.Unmarshal(frames[key], &m); e != nil || m.Account == nil || !ValidateAccountRequest(*m.Account, m.Type == MAccountCeremony) {
			t.Fatalf("%s: %s %v", key, frames[key], e)
		}
		round, _ := json.Marshal(m)
		var a, b any
		json.Unmarshal(frames[key], &a)
		json.Unmarshal(round, &b)
		aa, _ := json.Marshal(a)
		bb, _ := json.Marshal(b)
		if string(aa) != string(bb) {
			t.Fatalf("wire drift %s: %s", key, round)
		}
	}
	var m HarnessMsg
	if e = json.Unmarshal(frames["snapshot"], &m); e != nil {
		t.Fatal(e)
	}
	out, _ := json.Marshal(m)
	for _, secret := range []string{"authUrl", "secret", "attemptId", "synthetic"} {
		if strings.Contains(string(out), secret) {
			t.Fatalf("safe snapshot contains %s", secret)
		}
	}
}
func TestAccountSafeRequestsRejectCeremonyMaterial(t *testing.T) {
	for _, op := range []string{AccountStatus, AccountDefault, AccountLogout, AccountRebind} {
		r := AccountRequest{Operation: op}
		if op != AccountStatus {
			r.ProfileID = "codex:central"
			r.Revision = 1
		}
		if op == AccountDefault {
			r.DefaultRevision = 1
		}
		if op == AccountRebind {
			r.WorkloadID = "w"
		}
		if !ValidateAccountRequest(r, false) {
			t.Fatal(op)
		}
		for _, mutate := range []func(*AccountRequest){func(r *AccountRequest) { r.Secret = "secret" }, func(r *AccountRequest) { r.AttemptID = "attempt" }, func(r *AccountRequest) { r.Initiator = "browser" }} {
			bad := r
			mutate(&bad)
			if ValidateAccountRequest(bad, false) {
				t.Fatalf("accepted ceremony field on %s", op)
			}
		}
	}
}

func TestManagedOnlyPreservesRoutingWithoutExposingSourceChoices(t *testing.T) {
	s := AccountSnapshot{Version: 1, Defaults: map[string]string{"claude": "claude:host", "codex": "codex:central"}, DefaultRevisions: map[string]uint64{"claude": 9}, Profiles: []AccountProfile{
		{ID: "claude:host", Harness: "claude", Source: "host"},
		{ID: "codex:central", Harness: "codex", Source: "central"},
		{ID: "claude:central", Harness: "claude", Source: "central"},
		{ID: "claude:central", Harness: "claude", Source: "central"},
		{ID: "codex:environment", Harness: "codex", Source: "environment"},
	}}
	got := s.ManagedOnly()
	if len(got.Profiles) != 2 || got.Profiles[0].ID != "claude:central" || got.Profiles[1].ID != "codex:central" {
		t.Fatalf("projection: %+v", got)
	}
	if got.Defaults["claude"] != "claude:host" || got.DefaultRevisions["claude"] != 9 || len(s.Profiles) != 5 {
		t.Fatal("projection changed routing or source state")
	}
}
