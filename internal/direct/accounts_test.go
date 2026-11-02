package direct

import (
	"encoding/json"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"amux/internal/core"
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

func int64p(v int64) *int64 { return &v }

// A snapshot with every field the daemon can set, including the ones direct
// must never send.
func fullSnapshot() hp.AccountSnapshot {
	return hp.AccountSnapshot{
		DefaultRevisions: map[string]uint64{"claude": 7, "codex": 3},
		Version:          2,
		Defaults:         map[string]string{"claude": "claude:central", "codex": "codex:central"},
		MigrationPending: map[string]string{"claude": "host"},
		Profiles: []hp.AccountProfile{
			{ID: "claude:central", Harness: "claude", Source: "central", Revision: 9, DeliveryEpoch: 4, State: "ready",
				Mode: "subscription", SupportedModes: []string{"subscription", "api"}, LoginMethod: "browser",
				Identity: hp.AccountIdentity{State: "unknown", Evidence: "opaque-token"},
				Limits: hp.AccountLimits{State: "known", Source: "claude-statusline", ObservedAt: 1000, ProfileID: "claude:central",
					DeliveryEpoch: 4, SessionID: "a1", RuntimeGeneration: "gen-secret", ResetsAvailable: int64p(2),
					Windows: []hp.AccountLimitWindow{{Name: "5h", UsedPercent: 40, WindowMinutes: 300, ResetsAt: 2000}}}},
			{ID: "codex:central", Harness: "codex", Source: "central", Revision: 5, DeliveryEpoch: 1, State: "ready",
				Mode: "subscription", LoginMethod: "browser", Error: "dial unix /home/u/.codex/sock: secret path",
				Identity: hp.AccountIdentity{State: "known", AccountID: "acct-123", Email: "u@example.com", Plan: "pro", Evidence: "native"},
				Limits: hp.AccountLimits{State: "known", Source: "codex-native", ObservedAt: 1000, ProfileID: "codex:central",
					DeliveryEpoch: 1, RefreshError: "upstream said: token sk-live-xyz expired",
					Windows: []hp.AccountLimitWindow{{Name: "weekly", UsedPercent: 10}}}},
			{ID: "claude:host", Harness: "claude", Source: "host", State: "ready"}, // a legacy, non-managed profile
		},
	}
}

// boundRows is testRows with a central binding on a1 and a legacy one on wg1.
func boundRows() []core.Session {
	rows := testRows()
	rows[1].AccountBinding = &hp.AccountBinding{ProfileID: "claude:central", Source: "central"}
	rows[0].AccountBinding = &hp.AccountBinding{ProfileID: "claude:host-legacy-7", Source: "host"}
	return rows
}

func accountGrants(t *testing.T) (*Server, *fakeBackend, map[string]string) {
	t.Helper()
	f := newFake(boundRows()...)
	f.accounts = fullSnapshot()
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	s := newTestServer(t, a, f)
	toks := map[string]string{
		"steer":          enroll(t, a, []string{PermRead, PermSteer}, allSessions).AccessToken,
		"accounts-group": enroll(t, a, []string{PermRead, PermAccounts}, ResourceScope{Kind: ResourceWorkgroups, Workgroups: []string{"wg1"}}).AccessToken,
		"accounts-all":   enroll(t, a, []string{PermRead, PermAccounts}, allSessions).AccessToken,
		"every-other":    enroll(t, a, []string{PermRead, PermSteer, PermRespond, PermApprove, PermManage, PermCreate}, allSessions).AccessToken,
	}
	return s, f, toks
}

// One rule gates the route, the capability flag, and the list and watch rows.
func TestAccountsGateIsIdenticalEverywhere(t *testing.T) {
	s, f, toks := accountGrants(t)
	allowed := map[string]bool{"accounts-all": true}
	for name, tok := range toks {
		f.setRows(boundRows()...) // every grant starts from the same inventory
		before := f.accountCalls.Load()
		w := do(s, "GET", PathAccounts, tok, "")
		if allowed[name] {
			if w.Code != 200 {
				t.Fatalf("%s: accounts %d %s", name, w.Code, w.Body)
			}
		} else if w.Code != 403 || apiCode(t, w) != CodeDenied || !strings.Contains(w.Header().Get("WWW-Authenticate"), `scope="accounts"`) {
			t.Errorf("%s: accounts %d %s %q", name, w.Code, w.Body, w.Header().Get("WWW-Authenticate"))
		} else if f.accountCalls.Load() != before {
			t.Errorf("%s: a denied accounts read reached the backend", name)
		}

		// account_status is always serialized.
		w = do(s, "GET", PathCapabilities, tok, "")
		want := `"account_status":` + map[bool]string{true: "true", false: "false"}[allowed[name]]
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("%s: capabilities %s, want %s", name, w.Body, want)
		}

		// List and watch rows.
		w = do(s, "GET", PathSession, tok, "")
		var list SessionsResponse
		_ = json.Unmarshal(w.Body.Bytes(), &list)
		ts := httptest.NewServer(s)
		conn := dialWatch(t, ts, testOrigin)
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_ = conn.WriteJSON(WatchAuth{Type: "auth", AccessToken: tok})
		var welcome WatchWelcome
		var snap WatchSnapshot
		if conn.ReadJSON(&welcome) != nil || conn.ReadJSON(&snap) != nil {
			t.Fatalf("%s: watch", name)
		}
		// A later update carries the same rule.
		rows := boundRows()
		rows[1].Title = "renamed for " + name // unique per grant: f is shared across iterations
		f.setRows(rows...)
		var update WatchSnapshot
		if conn.ReadJSON(&update) != nil {
			t.Fatalf("%s: watch update", name)
		}
		conn.Close()
		ts.Close()
		for label, sessions := range map[string][]Session{"list": list.Sessions, "watch": snap.Sessions, "update": update.Sessions} {
			for _, row := range sessions {
				if allowed[name] != (row.AccountBinding != nil) && (row.ID == "a1" || row.ID == "wg1") {
					t.Errorf("%s %s row %s binding=%+v", name, label, row.ID, row.AccountBinding)
				}
				if row.ID == "wg1" && row.AccountBinding != nil && row.AccountBinding.ProfileID != "" {
					t.Errorf("%s %s: a non-central profile id was sent: %+v", name, label, row.AccountBinding)
				}
			}
		}
	}
}

func TestAccountsProjectionIsAllowlisted(t *testing.T) {
	s, _, toks := accountGrants(t)
	w := do(s, "GET", PathAccounts, toks["accounts-all"], "")
	raw := w.Body.String()
	for _, leak := range []string{"revision", "Revision", "delivery", "Epoch", "ceremony", "migration", "gen-secret",
		"session_id", "sessionId", "runtime", "claude:host", "/home/", "sk-live", "upstream said", "secret path", "attempt_id", "url"} {
		if strings.Contains(raw, leak) {
			t.Errorf("accounts response leaks %q: %s", leak, raw)
		}
	}
	var resp AccountsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Accounts) != 2 || resp.Defaults["claude"] != "claude:central" || resp.Defaults["codex"] != "codex:central" {
		t.Fatalf("accounts %+v", resp)
	}
	byID := map[string]Account{}
	for _, a := range resp.Accounts {
		byID[a.ProfileID] = a
	}
	codex := byID["codex:central"]
	if codex.Error != CodeAccountUnrecognized || codex.Limits.RefreshError != CodeAccountUnrecognized {
		t.Fatalf("raw errors were not replaced: %+v", codex)
	}
	if codex.Identity.AccountID != "acct-123" || codex.Identity.Email != "u@example.com" {
		t.Fatalf("identity %+v", codex.Identity)
	}
	if byID["claude:central"].Limits.ResetsAvailable == nil || *byID["claude:central"].Limits.ResetsAvailable != 2 {
		t.Fatalf("attributable claude resets dropped: %+v", byID["claude:central"].Limits)
	}
}

func TestAccountsDefaultsAreNotFabricated(t *testing.T) {
	snap := fullSnapshot()
	snap.Defaults = map[string]string{"claude": "claude:host"}
	got := projectAccounts("sid", snap)
	if len(got.Defaults) != 0 {
		t.Fatalf("defaults %+v, want none (the only default names a non-managed profile)", got.Defaults)
	}
	snap.Defaults = nil
	if got := projectAccounts("sid", snap); got.Defaults == nil || len(got.Defaults) != 0 {
		t.Fatalf("absent defaults %+v", got.Defaults)
	}
}

func TestAccountsKnownErrorCodesPassThrough(t *testing.T) {
	snap := fullSnapshot()
	snap.Profiles[1].Error = "credential_mode_mismatch"
	snap.Profiles[1].Limits.RefreshError = "native_limits_unavailable"
	got := projectAccounts("sid", snap)
	for _, a := range got.Accounts {
		if a.ProfileID == "codex:central" && (a.Error != "credential_mode_mismatch" || a.Limits.RefreshError != "native_limits_unavailable") {
			t.Fatalf("fixed codes altered: %+v", a)
		}
	}
}

// The attribution check the DTO cannot carry is applied on the server.
func TestAccountsLimitsAttribution(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*hp.AccountProfile)
		ok     bool
	}{
		{"claude matching", func(*hp.AccountProfile) {}, true},
		{"claude missing session", func(p *hp.AccountProfile) { p.Limits.SessionID = "" }, false},
		{"claude missing generation", func(p *hp.AccountProfile) { p.Limits.RuntimeGeneration = "" }, false},
		{"claude other epoch", func(p *hp.AccountProfile) { p.Limits.DeliveryEpoch = 3 }, false},
		{"claude other profile", func(p *hp.AccountProfile) { p.Limits.ProfileID = "claude:host" }, false},
		{"claude wrong source", func(p *hp.AccountProfile) { p.Limits.Source = "claude-usage" }, false},
	}
	codexCases := []struct {
		name   string
		mutate func(*hp.AccountProfile)
		ok     bool
	}{
		{"codex matching", func(*hp.AccountProfile) {}, true},
		{"codex unknown identity", func(p *hp.AccountProfile) { p.Identity.State = "unknown" }, false},
		{"codex missing account id", func(p *hp.AccountProfile) { p.Identity.AccountID = "" }, false},
		{"codex other epoch", func(p *hp.AccountProfile) { p.Limits.DeliveryEpoch = 9 }, false},
		{"codex other profile", func(p *hp.AccountProfile) { p.Limits.ProfileID = "claude:central" }, false},
		{"codex wrong source", func(p *hp.AccountProfile) { p.Limits.Source = "claude-statusline" }, false},
	}
	run := func(idx int, id, name string, mutate func(*hp.AccountProfile), ok bool) {
		snap := fullSnapshot()
		mutate(&snap.Profiles[idx])
		source := snap.Profiles[idx].Limits.Source
		for _, a := range projectAccounts("sid", snap).Accounts {
			if a.ProfileID != id {
				continue
			}
			if ok && (a.Limits.State != "known" || len(a.Limits.Windows) == 0) {
				t.Errorf("%s: attributable limits hidden: %+v", name, a.Limits)
			}
			if !ok && (a.Limits.State != "unknown" || len(a.Limits.Windows) != 0 || a.Limits.ResetsAvailable != nil || a.Limits.Source != safeWord(source)) {
				t.Errorf("%s: unattributable limits shown: %+v", name, a.Limits)
			}
		}
	}
	for _, c := range cases {
		run(0, "claude:central", c.name, c.mutate, c.ok)
	}
	for _, c := range codexCases {
		run(1, "codex:central", c.name, c.mutate, c.ok)
	}
}

// accounts is the seventh permission: explicit, implied by nothing, refused
// by older ceilings.
func TestAccountsPermissionVocabulary(t *testing.T) {
	if p, err := ValidatePerms([]string{PermAccounts, PermRead}); err != nil || !slices.Equal(p, []string{PermRead, PermAccounts}) {
		t.Fatalf("ValidatePerms: %v %v", p, err)
	}
	a, _ := enabledAuthority(t, nil) // cloud ceiling read,steer
	r := issueReq("acct")
	r.Permissions = []string{PermRead, PermAccounts}
	if _, _, err := a.IssueCloudCode(r); cloudCode(t, err) != CloudErrScope {
		t.Fatalf("read,steer ceiling issued accounts: %v", err)
	}
	f := newFake(testRows()...)
	f.accounts = fullSnapshot()
	s := newTestServer(t, a, f)
	for _, perms := range [][]string{{PermRead}, {PermRead, PermSteer}} {
		tok := enroll(t, a, perms, allSessions)
		if w := do(s, "GET", PathAccounts, tok.AccessToken, ""); w.Code != 403 {
			t.Fatalf("%v read accounts: %d", perms, w.Code)
		}
	}
	if f.accountCalls.Load() != 0 {
		t.Fatal("denied reads reached the backend")
	}
}
