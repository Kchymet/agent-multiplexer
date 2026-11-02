package direct

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"amux/internal/core"
	"amux/internal/daemon"
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// workflowRows is a realistic workspace inventory: two live workgroups (wg1
// with member a1, wg3), an archived workgroup wg2 with member b1 (archived
// through its root only), the console, a repo home and a detached session.
func workflowRows() []core.Session {
	root := &core.SessionCaps{Prompt: true, Interject: true, Cancel: true, Goal: true}
	member := &core.SessionCaps{Prompt: true, Interject: true, Cancel: true, Permission: true}
	return []core.Session{
		{ID: "wg1", Title: "one", Source: "workspace", IsRoot: true, Role: hp.RoleCoordinator, Section: hp.SectionWorkgroups, Caps: root},
		{ID: "a1", Title: "member", Source: "workspace", RootID: "wg1", Section: hp.SectionWorkgroups, Caps: member, Cwd: "/home/secret/wt"},
		{ID: "wg3", Title: "three", Source: "workspace", IsRoot: true, Role: hp.RoleCoordinator, Section: hp.SectionWorkgroups, Caps: root},
		{ID: "wg2", Title: "two", Source: "workspace", IsRoot: true, Role: hp.RoleCoordinator, Archived: true, Section: hp.SectionArchived, Caps: root},
		{ID: "b1", Title: "archived member", Source: "workspace", RootID: "wg2", Section: hp.SectionArchived, Caps: member},
		{ID: "console", Title: "console", Source: "workspace", Role: hp.RoleConsole, Section: hp.SectionWorkgroups, Caps: root},
		{ID: "api", Title: "api", Source: "workspace", Role: hp.RoleRepo, Section: hp.SectionRepos, Caps: root},
		{ID: "d1", Title: "detached", Source: "claude", Section: hp.SectionDetached, Caps: member},
	}
}

func workflowFake() *fakeBackend {
	f := newFake(workflowRows()...)
	f.catalog = MachineCatalog{
		Harnesses: []CatalogHarness{{Name: "claude", Version: "2.1.0"}, {Name: "codex", Version: "0.9.0"}},
		Repos:     []string{"api", "web", "secret-repo"},
		Ceilings:  map[string][]string{"wg1": {"api"}, "wg2": {"api"}, "wg3": {"web"}},
	}
	return f
}

var allPerms = []string{PermRead, PermSteer, PermRespond, PermApprove, PermManage, PermCreate}

func body(verb string, fields map[string]string, key string) string {
	if fields == nil {
		fields = map[string]string{}
	}
	b, _ := json.Marshal(ActionRequest{Verb: verb, Fields: fields, IdemKey: key})
	return string(b)
}

func act(s *Server, tok, sid, verb string, fields map[string]string, key string) (int, string, ActionResponse) {
	path := PathWorkgroups
	if sid != "" {
		path = PathSession + "/" + sid + "/actions"
	}
	w := do(s, "POST", path, tok, body(verb, fields, key))
	var e APIError
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	var r ActionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &r)
	return w.Code, e.Code, r
}

func drainApplied(f *fakeBackend) []core.Action {
	var out []core.Action
	for {
		select {
		case a := <-f.applied:
			out = append(out, a)
		default:
			return out
		}
	}
}

// Every newly admitted verb needs its own permission. An S1 grant (read,steer)
// is refused all of them with 403 denied naming the missing permission, before
// any backend call, whether it was enrolled manually, re-opened from disk,
// refreshed, or minted through a cloud code or device ticket under a read,steer
// ceiling.
func TestS1GrantsNeverGainWorkflowVerbs(t *testing.T) {
	root := t.TempDir()
	a, err := OpenAuthority(root, Lifetimes{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	manual := enroll(t, a, []string{PermRead, PermSteer}, allSessions)
	pending, _, err := a.NewCode("later", []string{PermRead, PermSteer}, allSessions, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.EnableCloud(cloudPolicyReq([]string{PermRead, PermSteer}, allSessions)); err != nil {
		t.Fatal(err)
	}
	a.Close()

	// Re-open: what an upgraded binary sees on disk.
	a = openTestAuthority(t, root, Lifetimes{}, nil)
	refreshed, err := a.Refresh(manual.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	later, err := a.RedeemCode(pending, "phone", ClientWeb)
	if err != nil {
		t.Fatal(err)
	}
	// A saved read,steer cloud ceiling refuses the new permissions.
	r := issueReq("wide")
	r.Permissions, r.Resource = []string{PermRead, PermSteer, PermCreate}, allSessions
	if _, _, err := a.IssueCloudCode(r); cloudCode(t, err) != CloudErrScope {
		t.Fatalf("cloud ceiling widened: %v", err)
	}
	tr := ticketReq("ticket")
	tr.Permissions, tr.Resource = []string{PermRead, PermSteer}, allSessions
	tcode, _, err := a.IssueCloudCode(tr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.ClaimTicket(tcode, testChallenge, CodeChallengeMethodS256); err != nil {
		t.Fatal(err)
	}
	ticket, err := a.Redeem(tcode, "Pixel", ClientAndroid, testVerifier)
	if err != nil {
		t.Fatal(err)
	}

	f := workflowFake()
	s := newTestServer(t, a, f)
	for name, tok := range map[string]TokenResponse{"refreshed": refreshed, "old code": later, "ticket": ticket} {
		if tok.Scope != "read steer" {
			t.Fatalf("%s: scope %q", name, tok.Scope)
		}
		for verb, perm := range VerbPermissions {
			if perm == PermSteer {
				continue
			}
			sid := "a1"
			if verb == VerbNewWorkgroup || verb == VerbNewWorkgroupEmpty {
				sid = ""
			}
			w := do(s, "POST", map[bool]string{true: PathWorkgroups, false: PathSession + "/" + sid + "/actions"}[sid == ""], tok.AccessToken,
				body(verb, nil, name+verb))
			if apiCode(t, w) != CodeDenied || !strings.Contains(w.Header().Get("WWW-Authenticate"), `scope="`+perm+`"`) {
				t.Errorf("%s %s: %d %s %q", name, verb, w.Code, w.Body, w.Header().Get("WWW-Authenticate"))
			}
		}
		// The S1 inventory bytes: caps carry only prompt and stop.
		w := do(s, "GET", PathSession, tok.AccessToken, "")
		if !strings.Contains(w.Body.String(), `"caps":{"prompt":true,"stop":true}`) || strings.Contains(w.Body.String(), "interject") {
			t.Errorf("%s: S1 caps changed: %s", name, w.Body)
		}
	}
	if f.applies.Load() != 0 || f.catalogs.Load() != 0 {
		t.Fatalf("denied verbs reached the backend: applies=%d catalogs=%d", f.applies.Load(), f.catalogs.Load())
	}
}

// A permission this binary does not know (a future binary's, or garbage) is
// inert on load: it neither breaks the grant nor admits anything. This is the
// same property an older binary has for this change's permissions: its
// loadState decodes without validating perms and its Admit is an exact
// slices.Contains match (see docs/direct-access.md, Compatibility).
func TestUnknownStoredPermissionIsInert(t *testing.T) {
	root := t.TempDir()
	a, err := OpenAuthority(root, Lifetimes{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tok := enroll(t, a, []string{PermRead}, allSessions)
	a.mu.Lock()
	a.state.Grants[tok.ClientID].Perms = []string{PermRead, "future-power"}
	if err := a.commitLocked(); err != nil {
		t.Fatal(err)
	}
	a.mu.Unlock()
	a.Close()
	a = openTestAuthority(t, root, Lifetimes{}, nil)
	next, err := a.Refresh(tok.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	f := workflowFake()
	s := newTestServer(t, a, f)
	for _, verb := range ServerVerbs() {
		sid := "a1"
		if verb == VerbNewWorkgroup || verb == VerbNewWorkgroupEmpty {
			sid = ""
		}
		if code, ec, _ := act(s, next.AccessToken, sid, verb, nil, "k"+verb); code != 403 || ec != CodeDenied {
			t.Errorf("%s: %d %s", verb, code, ec)
		}
	}
	if f.applies.Load() != 0 {
		t.Fatal("unknown permission admitted an action")
	}
	if _, err := ValidatePerms([]string{PermRead, "future-power"}); err == nil {
		t.Fatal("ValidatePerms accepted an unknown permission")
	}
	if p, _ := ValidatePerms([]string{PermSteer, PermRead}); !slices.Equal(p, []string{PermRead, PermSteer}) {
		t.Fatalf("S1 normalization changed: %v", p)
	}
}

func TestWorkflowVerbsMapExactlyToDaemonActions(t *testing.T) {
	f := workflowFake()
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	s := newTestServer(t, a, f)
	tok := enroll(t, a, allPerms, allSessions)
	cases := []struct {
		sid, verb string
		fields    map[string]string
		want      core.Action
	}{
		{"a1", VerbInterject, map[string]string{"text": "also"}, core.Action{Action: core.ActionSteer, ID: "a1",
			Fields: map[string]string{"verb": "interject", "text": "also"}}},
		{"a1", VerbPermission, map[string]string{"request_id": "r1", "runtime_generation": "g7", "decision": "deny", "reason": "no"},
			core.Action{Action: core.ActionSteer, ID: "a1", Fields: map[string]string{"verb": "permission", "request_id": "r1",
				"runtime_generation": "g7", "decision": "deny", "reason": "no"}}},
		{"wg1", VerbGoal, map[string]string{"status": "paused"}, core.Action{Action: core.ActionSteer, ID: "wg1",
			Fields: map[string]string{"verb": "goal", "status": "paused"}}},
		{"a1", VerbStart, nil, core.Action{Action: core.ActionStart, ID: "a1"}},
		{"a1", VerbRename, map[string]string{"name": "Builder"}, core.Action{Action: core.ActionRename, ID: "a1", Fields: map[string]string{"name": "Builder"}}},
		{"a1", VerbArchive, nil, core.Action{Action: core.ActionSetArchived, ID: "a1", Fields: map[string]string{"archived": "true"}}},
		{"wg2", VerbUnarchive, nil, core.Action{Action: core.ActionSetArchived, ID: "wg2", Fields: map[string]string{"archived": "false"}}},
	}
	for i, c := range cases {
		code, ec, _ := act(s, tok.AccessToken, c.sid, c.verb, c.fields, "m"+string(rune('a'+i)))
		if code != 200 {
			t.Fatalf("%s: %d %s", c.verb, code, ec)
		}
		got := <-f.applied
		if got.Action != c.want.Action || got.ID != c.want.ID || !mapsEqual(got.Fields, c.want.Fields) {
			t.Fatalf("%s: applied %+v want %+v", c.verb, got, c.want)
		}
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// Permission answers must carry the exact correlation pair; anything else is
// refused before the backend. The daemon then binds it to the open request on
// the live runtime generation (internal/daemon runtimePermissionGate.consume).
func TestPermissionReplyRequiresRequestAndGeneration(t *testing.T) {
	f := workflowFake()
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	s := newTestServer(t, a, f)
	tok := enroll(t, a, []string{PermRead, PermApprove}, allSessions)
	bad := []map[string]string{
		{"request_id": "r1", "decision": "allow"},                                         // no generation
		{"runtime_generation": "g", "decision": "allow"},                                  // no request
		{"request_id": "r1", "runtime_generation": "g", "decision": "maybe"},              // not a decision
		{"request_id": "r1", "runtime_generation": "g"},                                   // no decision
		{"request_id": "r1", "runtime_generation": "g", "decision": "allow", "text": "x"}, // extra field
		{"request_id": "r\n1", "runtime_generation": "g", "decision": "allow"},            // control character
		{"request_id": " ", "runtime_generation": "g", "decision": "allow"},               // blank
	}
	for i, fields := range bad {
		if code, ec, _ := act(s, tok.AccessToken, "a1", VerbPermission, fields, "p"+string(rune('a'+i))); code != 400 || ec != CodeBadRequest {
			t.Errorf("%v: %d %s", fields, code, ec)
		}
	}
	// A session whose runtime cannot correlate approvals is refused.
	if code, ec, _ := act(s, tok.AccessToken, "wg1", VerbPermission,
		map[string]string{"request_id": "r1", "runtime_generation": "g", "decision": "allow"}, "pz"); code != 409 || ec != CodeUnsupportedVerb {
		t.Errorf("uncorrelated runtime: %d %s", code, ec)
	}
	if f.applies.Load() != 0 {
		t.Fatalf("refused permission replies reached Apply: %d", f.applies.Load())
	}
	// approve alone does not grant prompt.
	if code, ec, _ := act(s, tok.AccessToken, "a1", VerbPrompt, map[string]string{"text": "hi"}, "pp"); code != 403 || ec != CodeDenied {
		t.Errorf("approve granted prompt: %d %s", code, ec)
	}
}

// Scope, console exclusion and archive state come from the fresh inventory and
// gate every new verb before Apply.
func TestWorkflowScopeConsoleAndArchiveBeforeApply(t *testing.T) {
	f := workflowFake()
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	s := newTestServer(t, a, f)
	all := enroll(t, a, allPerms, allSessions)
	group := enroll(t, a, allPerms, ResourceScope{Kind: ResourceWorkgroups, Workgroups: []string{"wg1", "wg2"}})
	cases := []struct {
		name, tok, sid, verb string
		fields               map[string]string
		status               int
		code                 string
	}{
		{"console rename", all.AccessToken, "console", VerbRename, map[string]string{"name": "x"}, 404, CodeNotFound},
		{"console archive", all.AccessToken, "console", VerbArchive, nil, 404, CodeNotFound},
		{"console add-agent", all.AccessToken, "console", VerbAddAgent, nil, 404, CodeNotFound},
		{"console goal", all.AccessToken, "console", VerbGoal, map[string]string{"status": "paused"}, 404, CodeNotFound},
		{"outside workgroup", group.AccessToken, "wg3", VerbAddAgent, nil, 404, CodeNotFound},
		{"outside archive", group.AccessToken, "wg3", VerbArchive, nil, 404, CodeNotFound},
		{"workgroup grant cannot create roots", group.AccessToken, "", VerbNewWorkgroup, nil, 403, CodeDenied},
		{"workgroup grant cannot create empty roots", group.AccessToken, "", VerbNewWorkgroupEmpty, map[string]string{"repos": ""}, 403, CodeDenied},
		{"archived root add-agent", all.AccessToken, "wg2", VerbAddAgent, nil, 409, CodeArchived},
		{"inherited archive rename", all.AccessToken, "b1", VerbRename, map[string]string{"name": "x"}, 409, CodeArchived},
		{"inherited archive start", all.AccessToken, "b1", VerbStart, nil, 409, CodeArchived},
		{"inherited archive interject", all.AccessToken, "b1", VerbInterject, map[string]string{"text": "x"}, 409, CodeArchived},
		{"inherited archive permission", all.AccessToken, "b1", VerbPermission,
			map[string]string{"request_id": "r", "runtime_generation": "g", "decision": "allow"}, 409, CodeArchived},
		{"unarchive member of archived root", all.AccessToken, "b1", VerbUnarchive, nil, 409, CodeArchived},
		{"re-archive archived", all.AccessToken, "wg2", VerbArchive, nil, 409, CodeArchived},
		{"unarchive live", all.AccessToken, "a1", VerbUnarchive, nil, 409, CodeArchived},
		{"add-agent to member", all.AccessToken, "a1", VerbAddAgent, nil, 409, CodeUnsupportedVerb},
		{"repo home archive", all.AccessToken, "api", VerbArchive, nil, 409, CodeUnsupportedVerb},
		{"detached rename", all.AccessToken, "d1", VerbRename, map[string]string{"name": "x"}, 409, CodeUnsupportedVerb},
		{"goal on non-goal session", all.AccessToken, "a1", VerbGoal, map[string]string{"status": "paused"}, 409, CodeUnsupportedVerb},
		{"archive force field", all.AccessToken, "a1", VerbArchive, map[string]string{"force": "true"}, 400, CodeBadRequest},
		{"add-agent api-key identity", all.AccessToken, "wg1", VerbAddAgent, map[string]string{"identity_mode": "api-key"}, 400, CodeBadRequest},
		{"add-agent unknown harness", all.AccessToken, "wg1", VerbAddAgent, map[string]string{"agent": "bash"}, 400, CodeBadRequest},
		{"add-agent repo path", all.AccessToken, "wg1", VerbAddAgent, map[string]string{"repos": "../etc"}, 400, CodeBadRequest},
		{"new-workgroup linear", all.AccessToken, "", VerbNewWorkgroup, map[string]string{"linear": "x"}, 400, CodeBadRequest},
		{"new-workgroup source", all.AccessToken, "", VerbNewWorkgroup, map[string]string{"source": "/etc"}, 400, CodeBadRequest},
		{"empty with member", all.AccessToken, "", VerbNewWorkgroupEmpty, map[string]string{"repos": "", "agent": "claude"}, 400, CodeBadRequest},
		{"empty without repos", all.AccessToken, "", VerbNewWorkgroupEmpty, nil, 400, CodeBadRequest},
		{"empty with defaultAgent", all.AccessToken, "", VerbNewWorkgroupEmpty, map[string]string{"repos": "", "defaultAgent": "1"}, 400, CodeBadRequest},
	}
	for i, c := range cases {
		code, ec, _ := act(s, c.tok, c.sid, c.verb, c.fields, "s"+string(rune('a'+i)))
		if code != c.status || ec != c.code {
			t.Errorf("%s: %d %s, want %d %s", c.name, code, ec, c.status, c.code)
		}
	}
	if f.applies.Load() != 0 {
		t.Fatalf("refused actions reached Apply: %d %+v", f.applies.Load(), drainApplied(f))
	}
	// The workgroup grant still manages its own (archived) workgroup.
	if code, ec, _ := act(s, group.AccessToken, "wg2", VerbUnarchive, nil, "ok-unarchive"); code != 200 {
		t.Fatalf("in-scope unarchive: %d %s", code, ec)
	}
}

// Creation is checked against the machine catalog and the target's current
// repo ceiling, both read fresh for every request.
func TestCreationCatalogAndCeilingAreFresh(t *testing.T) {
	f := workflowFake()
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	s := newTestServer(t, a, f)
	tok := enroll(t, a, allPerms, allSessions)
	f.newID = "n1"
	if code, ec, _ := act(s, tok.AccessToken, "wg1", VerbAddAgent, map[string]string{"repos": "web"}, "c1"); code != 422 || ec != CodeNotInCatalog {
		t.Fatalf("repo outside ceiling: %d %s", code, ec)
	}
	if code, ec, _ := act(s, tok.AccessToken, "wg1", VerbAddAgent, map[string]string{"agent": "hermes"}, "c2"); code != 422 || ec != CodeNotInCatalog {
		t.Fatalf("unverified harness: %d %s", code, ec)
	}
	if code, ec, _ := act(s, tok.AccessToken, "", VerbNewWorkgroup, map[string]string{"repos": "nope"}, "c3"); code != 422 || ec != CodeNotInCatalog {
		t.Fatalf("untracked repo: %d %s", code, ec)
	}
	if f.applies.Load() != 0 {
		t.Fatal("catalog refusals reached Apply")
	}
	// The ceiling widens on the host: the next request sees it.
	f.mu.Lock()
	f.catalog.Ceilings["wg1"] = []string{"api", "web"}
	f.mu.Unlock()
	code, ec, r := act(s, tok.AccessToken, "wg1", VerbAddAgent, map[string]string{"repos": "web", "agent": "codex", "mode": "task", "identity_mode": "machine"}, "c4")
	if code != 200 || r.NewID != "n1" {
		t.Fatalf("add-agent after widening: %d %s %+v", code, ec, r)
	}
	got := <-f.applied
	if got.Action != core.ActionAddAgent || got.ID != "wg1" || got.Fields["identity_mode"] != "" || got.Fields["repos"] != "web" {
		t.Fatalf("applied %+v", got)
	}
	// ...and narrows again.
	f.mu.Lock()
	f.catalog.Ceilings["wg1"] = []string{}
	f.mu.Unlock()
	if code, ec, _ := act(s, tok.AccessToken, "wg1", VerbAddAgent, map[string]string{"repos": "web"}, "c5"); code != 422 || ec != CodeNotInCatalog {
		t.Fatalf("after narrowing: %d %s", code, ec)
	}
	// Root archived between requests: refused from the fresh inventory.
	rows := workflowRows()
	rows[0].Archived, rows[0].Section = true, hp.SectionArchived
	f.setRows(rows...)
	if code, ec, _ := act(s, tok.AccessToken, "wg1", VerbAddAgent, nil, "c6"); code != 409 || ec != CodeArchived {
		t.Fatalf("archived since: %d %s", code, ec)
	}
}

// A creation's idempotency record carries the new id; a replay answers with it
// and never creates again. An unknown outcome is permanent for its key.
func TestCreationIdempotency(t *testing.T) {
	f := workflowFake()
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	s := newTestServer(t, a, f)
	tok := enroll(t, a, allPerms, allSessions)
	fields := map[string]string{"name": "New", "repos": "api", "agent": "claude"}

	f.newID = "root-1"
	code, _, r := act(s, tok.AccessToken, "", VerbNewWorkgroup, fields, "same")
	if code != 200 || r.NewID != "root-1" {
		t.Fatalf("create: %d %+v", code, r)
	}
	if got := <-f.applied; got.Action != core.ActionNewWorkgroup || got.Fields["repos"] != "api" {
		t.Fatalf("applied %+v", got)
	}
	f.newID = "root-2"
	code, _, r = act(s, tok.AccessToken, "", VerbNewWorkgroup, fields, "same")
	if code != 200 || r.NewID != "root-1" || r.RootID != "root-1" || f.applies.Load() != 1 {
		t.Fatalf("replay: %d %+v applies=%d", code, r, f.applies.Load())
	}
	// The same key with a different body conflicts.
	if code, ec, _ := act(s, tok.AccessToken, "", VerbNewWorkgroup, map[string]string{"name": "Other"}, "same"); code != 422 || ec != CodeIdemConflict {
		t.Fatalf("conflict: %d %s", code, ec)
	}
	// A transport failure after dispatch: ambiguous, and never re-run.
	f.applyErr = errors.New("connection reset after dispatch")
	if code, ec, _ := act(s, tok.AccessToken, "wg1", VerbAddAgent, nil, "lost"); code != 409 || ec != CodeIdemAmbiguous {
		t.Fatalf("lost outcome: %d %s", code, ec)
	}
	f.applyErr = nil
	if code, ec, _ := act(s, tok.AccessToken, "wg1", VerbAddAgent, nil, "lost"); code != 409 || ec != CodeIdemAmbiguous {
		t.Fatalf("lost replay: %d %s", code, ec)
	}
	// Success with no usable id: something may exist, so ambiguous as well.
	f.newID = ""
	if code, ec, _ := act(s, tok.AccessToken, "wg1", VerbAddAgent, nil, "noid"); code != 409 || ec != CodeIdemAmbiguous {
		t.Fatalf("no id: %d %s", code, ec)
	}
	if code, ec, _ := act(s, tok.AccessToken, "wg1", VerbAddAgent, nil, "noid"); code != 409 || ec != CodeIdemAmbiguous {
		t.Fatalf("no id replay: %d %s", code, ec)
	}
	// The daemon's refusal is a known answer, replayed.
	f.applyErr = &daemon.RejectedError{Msg: "no such workgroup"}
	if code, ec, _ := act(s, tok.AccessToken, "wg1", VerbAddAgent, nil, "rej"); code != 422 || ec != CodeRejected {
		t.Fatalf("rejected: %d %s", code, ec)
	}
	f.applyErr = nil
	if code, ec, _ := act(s, tok.AccessToken, "wg1", VerbAddAgent, nil, "rej"); code != 422 || ec != CodeRejected {
		t.Fatalf("rejected replay: %d %s", code, ec)
	}
	// Total: one create, one lost, one id-less, one rejected.
	if n := f.applies.Load(); n != 4 {
		t.Fatalf("applies=%d", n)
	}
}

// The creation record, with its id, survives a restart: the replay after it
// answers with the original id and creates nothing.
func TestCreationReplaySurvivesRestart(t *testing.T) {
	root := t.TempDir()
	a, err := OpenAuthority(root, Lifetimes{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tok := enroll(t, a, allPerms, allSessions)
	f := workflowFake()
	f.newID = "agent-7"
	if code, ec, r := act(newTestServer(t, a, f), tok.AccessToken, "wg1", VerbAddAgent, map[string]string{"repos": "api"}, "k"); code != 200 || r.NewID != "agent-7" {
		t.Fatalf("create: %d %s %+v", code, ec, r)
	}
	a.Close()
	a = openTestAuthority(t, root, Lifetimes{}, nil)
	next, err := a.Refresh(tok.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	f.newID = "agent-8"
	code, _, r := act(newTestServer(t, a, f), next.AccessToken, "wg1", VerbAddAgent, map[string]string{"repos": "api"}, "k")
	if code != 200 || r.NewID != "agent-7" || r.RootID != "wg1" || f.applies.Load() != 1 {
		t.Fatalf("replay after restart: %d %+v applies=%d", code, r, f.applies.Load())
	}
}

// Archiving a root reports every member whose effective state changes; a
// replay reports the same list.
func TestArchiveReportsAffectedMembers(t *testing.T) {
	f := workflowFake()
	rows := append(workflowRows(), core.Session{ID: "a2", Source: "workspace", RootID: "wg1", Archived: true, Section: hp.SectionArchived})
	f.setRows(rows...)
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	s := newTestServer(t, a, f)
	tok := enroll(t, a, []string{PermRead, PermManage}, allSessions)
	code, ec, r := act(s, tok.AccessToken, "wg1", VerbArchive, nil, "ar")
	if code != 200 || !slices.Equal(r.Affected, []string{"wg1", "a1"}) {
		t.Fatalf("archive root: %d %s %+v", code, ec, r)
	}
	if _, _, r := act(s, tok.AccessToken, "wg1", VerbArchive, nil, "ar"); !slices.Equal(r.Affected, []string{"wg1", "a1"}) || f.applies.Load() != 1 {
		t.Fatalf("replay: %+v", r)
	}
	if _, _, r := act(s, tok.AccessToken, "a1", VerbArchive, nil, "am"); !slices.Equal(r.Affected, []string{"a1"}) {
		t.Fatalf("archive member: %+v", r)
	}
	if _, _, r := act(s, tok.AccessToken, "wg2", VerbUnarchive, nil, "un"); !slices.Equal(r.Affected, []string{"wg2", "b1"}) {
		t.Fatalf("unarchive root: %+v", r)
	}
}

// respond without steer: interject yes, prompt and stop no.
func TestRespondDoesNotImplySteer(t *testing.T) {
	f := workflowFake()
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	s := newTestServer(t, a, f)
	tok := enroll(t, a, []string{PermRead, PermRespond}, allSessions)
	for _, verb := range []string{VerbPrompt, VerbStop} {
		fields := map[string]string{}
		if verb == VerbPrompt {
			fields["text"] = "x"
		}
		if code, ec, _ := act(s, tok.AccessToken, "a1", verb, fields, "r"+verb); code != 403 || ec != CodeDenied {
			t.Errorf("%s: %d %s", verb, code, ec)
		}
	}
	if code, ec, _ := act(s, tok.AccessToken, "a1", "delete", nil, "rdel"); code != 403 || ec != CodeDenied {
		t.Errorf("unknown verb without steer: %d %s", code, ec)
	}
	if f.applies.Load() != 0 || f.snapshots.Load() != 0 {
		t.Fatal("denied verbs reached the backend")
	}
	if code, ec, _ := act(s, tok.AccessToken, "a1", VerbInterject, map[string]string{"text": "x"}, "ri"); code != 200 {
		t.Fatalf("interject: %d %s", code, ec)
	}
}

// A creation admitted by a previous process with no recorded outcome is
// ambiguous after a restart: the key never creates again.
func TestCreationAdmittedBeforeCrashIsAmbiguous(t *testing.T) {
	root := t.TempDir()
	a, err := OpenAuthority(root, Lifetimes{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tok := enroll(t, a, allPerms, allSessions)
	f := workflowFake()
	s := newTestServer(t, a, f)
	f.newID = "root-9"
	fields := map[string]string{"name": "x", "repos": ""}
	l, err := a.Admit(t.Context(), tok.AccessToken, PermCreate)
	if err != nil {
		t.Fatal(err)
	}
	hash := actionHash("", ActionRequest{Verb: VerbNewWorkgroupEmpty, Fields: fields})
	if out, _, err := a.BeginAction(l, "crash", hash, false); err != nil || out != idemProceed {
		t.Fatalf("begin: %v %v", out, err)
	}
	l.Done()
	_ = s
	a.Close() // "crash": admitted, no outcome

	a = openTestAuthority(t, root, Lifetimes{}, nil)
	next, err := a.Refresh(tok.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	s = newTestServer(t, a, f)
	if code, ec, _ := act(s, next.AccessToken, "", VerbNewWorkgroupEmpty, fields, "crash"); code != 409 || ec != CodeIdemAmbiguous {
		t.Fatalf("after crash: %d %s", code, ec)
	}
	if f.applies.Load() != 0 {
		t.Fatal("an ambiguous creation ran again")
	}
}

// Capabilities let a client tell an old server (no route), an implemented but
// ungranted verb (server_verbs but not verbs) and a granted verb apart, and
// carry only allowlisted catalog fields.
func TestCapabilities(t *testing.T) {
	f := workflowFake()
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	s := newTestServer(t, a, f)
	steer := enroll(t, a, []string{PermRead, PermSteer}, allSessions)
	group := enroll(t, a, allPerms, ResourceScope{Kind: ResourceWorkgroups, Workgroups: []string{"wg1", "wg2"}})
	all := enroll(t, a, allPerms, allSessions)

	get := func(tok string) (CapabilitiesResponse, string) {
		w := do(s, "GET", PathCapabilities, tok, "")
		if w.Code != 200 {
			t.Fatalf("capabilities: %d %s", w.Code, w.Body)
		}
		var c CapabilitiesResponse
		if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		return c, w.Body.String()
	}
	c, _ := get(steer.AccessToken)
	if !slices.Equal(c.Verbs, []string{VerbPrompt, VerbStop}) || c.CreateWorkgroup || c.Catalog != nil ||
		!slices.Equal(c.ServerVerbs, ServerVerbs()) {
		t.Fatalf("steer capabilities: %+v", c)
	}
	if f.catalogs.Load() != 0 {
		t.Fatal("a grant without create read the catalog")
	}
	c, raw := get(group.AccessToken)
	if c.CreateWorkgroup || c.Catalog == nil || c.Catalog.Repos != nil || len(c.Catalog.Workgroups) != 1 ||
		c.Catalog.Workgroups[0].ID != "wg1" || !slices.Equal(c.Catalog.Workgroups[0].Repos, []string{"api"}) {
		t.Fatalf("workgroup capabilities: %s", raw)
	}
	for _, leak := range []string{"secret-repo", "wg3", "web", "console"} {
		if strings.Contains(raw, leak) {
			t.Fatalf("workgroup capabilities leak %q: %s", leak, raw)
		}
	}
	c, raw = get(all.AccessToken)
	if !c.CreateWorkgroup || !slices.Equal(c.Catalog.Repos, []string{"api", "web", "secret-repo"}) ||
		!slices.Equal(c.Catalog.IdentityModes, []string{hp.IdentityMachine}) || len(c.Catalog.Harnesses) != 2 ||
		!slices.Equal(c.Verbs, []string{VerbPrompt, VerbStop, VerbInterject, VerbGoal, VerbPermission, VerbStart,
			VerbRename, VerbArchive, VerbUnarchive, VerbAddAgent}) {
		t.Fatalf("all capabilities: %s", raw)
	}
	for _, leak := range []string{"/home", `"source"`, "account", "token", "secret/wt", "branch", "force"} {
		if strings.Contains(raw, leak) {
			t.Fatalf("capabilities leak %q: %s", leak, raw)
		}
	}
	// Per-row caps follow the grant and the row.
	w := do(s, "GET", PathSession, all.AccessToken, "")
	var sr SessionsResponse
	_ = json.Unmarshal(w.Body.Bytes(), &sr)
	caps := map[string]SessionCaps{}
	for _, row := range sr.Sessions {
		caps[row.ID] = row.Caps
	}
	if _, ok := caps["console"]; ok {
		t.Fatal("console in inventory")
	}
	if c := caps["wg2"]; !c.Unarchive || c.Archive || c.AddAgent || c.Rename || c.Prompt {
		t.Fatalf("archived root caps %+v", c)
	}
	if c := caps["b1"]; c.Unarchive || c.Prompt || c.Permission {
		t.Fatalf("inherited-archive member caps %+v", c)
	}
	if c := caps["a1"]; !c.Permission || !c.Interject || !c.Rename || !c.Archive || c.AddAgent || c.Goal || c.Unarchive {
		t.Fatalf("member caps %+v", c)
	}
	if c := caps["wg1"]; !c.AddAgent || !c.Goal || c.Permission {
		t.Fatalf("root caps %+v", c)
	}
	if c := caps["api"]; c.Archive || c.Rename || c.AddAgent {
		t.Fatalf("repo home caps %+v", c)
	}
}

// Revocation still cuts an admitted creation short and leaves its key
// ambiguous: the barrier and idempotency hold for the new verbs.
func TestRevokeDuringCreationIsAmbiguous(t *testing.T) {
	f := workflowFake()
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	s := newTestServer(t, a, f)
	tok := enroll(t, a, allPerms, allSessions)
	f.blockApply.Store(true)
	done := make(chan int)
	go func() {
		code, _, _ := act(s, tok.AccessToken, "", VerbNewWorkgroup, map[string]string{"name": "x"}, "rv")
		done <- code
	}()
	deadline := time.Now().Add(5 * time.Second)
	for f.activeApplies.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("apply never started")
		}
		time.Sleep(time.Millisecond)
	}
	if err := a.Revoke(tok.ClientID, "test"); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != 401 {
		t.Fatalf("revoked creation: %d", code)
	}
	if f.activeApplies.Load() != 0 {
		t.Fatal("revoke returned before the admitted creation finished")
	}
}

func TestCatalogModelsAreSanitized(t *testing.T) {
	got := catalogModels([]string{"claude-opus-5-5", "claude-opus-5-5", "", "bad model", "x\n", "gpt-6", "/etc/passwd"})
	if !slices.Equal(got, []string{"claude-opus-5-5", "gpt-6"}) {
		t.Fatalf("models %v", got)
	}
}
