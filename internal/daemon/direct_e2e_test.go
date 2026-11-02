package daemon_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"amux/internal/daemon"
	"amux/internal/direct"
	"amux/internal/hostbackend"
	"amux/internal/wsops"
)

// These tests drive the direct workflow verbs through the whole production
// path: the direct HTTP server (authorization boundary) -> direct.HostBackend
// -> hostbackend over authenticated host connections -> a real daemon
// (workspace source, store, wsops creation, steer dispatch) -> the daemon's
// own inventory, read back through the direct API. Only the agent runtime is
// fake: it records keystrokes instead of running a harness.

const e2eHost = "amux.test:8443"

type e2e struct {
	t    *testing.T
	h    *daemon.LiveHost
	auth *direct.Authority
	srv  *direct.Server
}

func newE2E(t *testing.T) *e2e {
	t.Helper()
	h := daemon.NewLiveHostForTest(t)
	auth, err := direct.OpenAuthority(t.TempDir(), direct.Lifetimes{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(auth.Close)
	backend := direct.HostBackend{
		H:    hostbackend.WithDialer(func(ctx context.Context) (hostbackend.Conn, error) { return h.Dial(ctx) }),
		Dial: h.Dial,
		Probe: func(context.Context) []direct.CatalogHarness {
			return []direct.CatalogHarness{{Name: "claude"}, {Name: "codex"}}
		},
	}
	srv, err := direct.NewServer(direct.Config{Authority: auth, Backend: backend, Endpoints: []string{"https://" + e2eHost}})
	if err != nil {
		t.Fatal(err)
	}
	return &e2e{t: t, h: h, auth: auth, srv: srv}
}

func (e *e2e) enroll(perms []string, res direct.ResourceScope) string {
	e.t.Helper()
	code, _, err := e.auth.NewCode("e2e", perms, res, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	tok, err := e.auth.RedeemCode(code, "phone", direct.ClientAndroid)
	if err != nil {
		e.t.Fatal(err)
	}
	return tok.AccessToken
}

func (e *e2e) call(method, path, tok string, body any) (int, []byte) {
	e.t.Helper()
	var rd *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	} else {
		rd = strings.NewReader("")
	}
	r := httptest.NewRequest(method, path, rd)
	r.Host = e2eHost
	r.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	e.srv.ServeHTTP(w, r)
	return w.Code, w.Body.Bytes()
}

func (e *e2e) act(tok, sid, verb string, fields map[string]string, key string) (int, direct.ActionResponse, direct.APIError) {
	e.t.Helper()
	if fields == nil {
		fields = map[string]string{}
	}
	path := direct.PathWorkgroups
	if sid != "" {
		path = direct.PathSession + "/" + sid + "/actions"
	}
	code, b := e.call("POST", path, tok, direct.ActionRequest{Verb: verb, Fields: fields, IdemKey: key})
	var ok direct.ActionResponse
	var bad direct.APIError
	_ = json.Unmarshal(b, &ok)
	_ = json.Unmarshal(b, &bad)
	return code, ok, bad
}

func (e *e2e) inventory(tok string) map[string]direct.Session {
	e.t.Helper()
	e.h.Poll()
	code, b := e.call("GET", direct.PathSession, tok, nil)
	if code != 200 {
		e.t.Fatalf("inventory: %d %s", code, b)
	}
	var sr direct.SessionsResponse
	if err := json.Unmarshal(b, &sr); err != nil {
		e.t.Fatal(err)
	}
	out := map[string]direct.Session{}
	for _, s := range sr.Sessions {
		out[s.ID] = s
	}
	return out
}

// eventually polls the inventory until cond holds.
func (e *e2e) eventually(tok, what string, cond func(map[string]direct.Session) bool) map[string]direct.Session {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		inv := e.inventory(tok)
		if cond(inv) {
			return inv
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("%s: inventory never matched: %+v", what, inv)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func membersOf(inv map[string]direct.Session, root string) []string {
	var out []string
	for id, s := range inv {
		if s.RootID == root && id != root {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

// trackLocalRepo makes a one-commit git repository and tracks it on the host.
func trackLocalRepo(t *testing.T, name string) {
	t.Helper()
	t.Setenv("AMUX_GIT_TRUST_LOCAL_SOURCE", "1")
	src := filepath.Join(t.TempDir(), name)
	if err := os.Mkdir(src, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "base"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = src
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if _, err := wsops.AddRepoSource(context.Background(), src); err != nil {
		t.Fatal(err)
	}
}

var everyPerm = []string{direct.PermRead, direct.PermSteer, direct.PermRespond, direct.PermApprove, direct.PermManage, direct.PermCreate}

func TestDirectLiveWorkflowParity(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required to track a repository")
	}
	e := newE2E(t)
	trackLocalRepo(t, "app")
	trackLocalRepo(t, "lib")
	all := e.enroll(everyPerm, direct.ResourceScope{Kind: direct.ResourceAllSessions})

	// Discovery: the catalog comes from the live daemon.
	code, b := e.call("GET", direct.PathCapabilities, all, nil)
	var caps direct.CapabilitiesResponse
	if code != 200 || json.Unmarshal(b, &caps) != nil || caps.Catalog == nil || !slices.Contains(caps.Catalog.Repos, "app") || !caps.CreateWorkgroup {
		t.Fatalf("capabilities: %d %s", code, b)
	}
	if strings.Contains(string(b), `"source"`) || strings.Contains(string(b), os.TempDir()) {
		t.Fatalf("capabilities leak a source or path: %s", b)
	}

	// Root creation with an initial member.
	code, created, bad := e.act(all, "", direct.VerbNewWorkgroup, map[string]string{
		"name": "Payments", "prompt": "fix the idempotency bug", "repos": "app", "agent": "claude", "identity_mode": "machine",
	}, "wg-1")
	if code != 200 || created.NewID == "" || created.RootID != created.NewID {
		t.Fatalf("new-workgroup: %d %+v %+v", code, created, bad)
	}
	root := created.NewID
	inv := e.eventually(all, "root with first member", func(inv map[string]direct.Session) bool {
		return inv[root].IsRoot && len(membersOf(inv, root)) == 1
	})
	if inv[root].Title != "Payments" || !e.h.Running(root) {
		t.Fatalf("root row %+v running=%v", inv[root], e.h.Running(root))
	}
	first := membersOf(inv, root)[0]
	if inv[first].Repos != "app" {
		t.Fatalf("first member %+v", inv[first])
	}
	// Idempotent replay: same id, no second workgroup.
	code, again, _ := e.act(all, "", direct.VerbNewWorkgroup, map[string]string{
		"name": "Payments", "prompt": "fix the idempotency bug", "repos": "app", "agent": "claude", "identity_mode": "machine",
	}, "wg-1")
	roots := 0
	for _, s := range e.inventory(all) {
		if s.IsRoot && s.Title == "Payments" {
			roots++
		}
	}
	if code != 200 || again.NewID != root || roots != 1 {
		t.Fatalf("replay: %d %+v roots=%d", code, again, roots)
	}

	// Empty roots. repos="" is the daemon's "every tracked repo, live" ceiling
	// (wsops.createWorkspace); naming repos limits it to exactly those.
	code, empty, bad := e.act(all, "", direct.VerbNewWorkgroupEmpty, map[string]string{"name": "Triage", "repos": ""}, "wg-2")
	if code != 200 || empty.NewID == "" {
		t.Fatalf("new-workgroup-empty: %d %+v %+v", code, empty, bad)
	}
	inv = e.eventually(all, "empty root", func(inv map[string]direct.Session) bool { return inv[empty.NewID].IsRoot })
	if m := membersOf(inv, empty.NewID); len(m) != 0 {
		t.Fatalf("empty root has members %v", m)
	}
	code, narrow, bad := e.act(all, "", direct.VerbNewWorkgroupEmpty, map[string]string{"name": "App only", "repos": "app"}, "wg-3")
	if code != 200 || narrow.NewID == "" {
		t.Fatalf("narrow new-workgroup-empty: %d %+v %+v", code, narrow, bad)
	}
	e.eventually(all, "narrow root", func(inv map[string]direct.Session) bool { return inv[narrow.NewID].IsRoot })
	code, b = e.call("GET", direct.PathCapabilities, all, nil)
	_ = json.Unmarshal(b, &caps)
	ceilings := map[string][]string{}
	for _, w := range caps.Catalog.Workgroups {
		ceilings[w.ID] = w.Repos
	}
	if !slices.Equal(ceilings[narrow.NewID], []string{"app"}) || !slices.Contains(ceilings[empty.NewID], "lib") {
		t.Fatalf("live ceilings %v", ceilings)
	}
	// The narrow ceiling refuses lib, from the live catalog, before Apply.
	if code, _, bad := e.act(all, narrow.NewID, direct.VerbAddAgent, map[string]string{"repos": "lib"}, "aa-0"); code != 422 || bad.Code != direct.CodeNotInCatalog {
		t.Fatalf("add-agent outside narrow ceiling: %d %+v", code, bad)
	}
	if m := membersOf(e.inventory(all), narrow.NewID); len(m) != 0 {
		t.Fatalf("refused add-agent created %v", m)
	}

	// add-agent into the first workgroup, inside its ceiling; it starts.
	code, added, bad := e.act(all, root, direct.VerbAddAgent, map[string]string{"agent": "claude", "repos": "app", "mode": "task"}, "aa-1")
	if code != 200 || added.NewID == "" || added.RootID != root {
		t.Fatalf("add-agent: %d %+v %+v", code, added, bad)
	}
	e.eventually(all, "added member", func(inv map[string]direct.Session) bool { return inv[added.NewID].RootID == root })
	if !e.h.Running(added.NewID) {
		t.Fatal("added agent is not running")
	}

	// Steering reaches the runtime as keystrokes.
	if code, _, bad := e.act(all, added.NewID, direct.VerbPrompt, map[string]string{"text": "run the tests"}, "p-1"); code != 200 {
		t.Fatalf("prompt: %d %+v", code, bad)
	}
	if code, _, bad := e.act(all, added.NewID, direct.VerbInterject, map[string]string{"text": "skip the flaky one"}, "i-1"); code != 200 {
		t.Fatalf("interject: %d %+v", code, bad)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(e.h.Typed(added.NewID), "run the tests") || !strings.Contains(e.h.Typed(added.NewID), "skip the flaky one") {
		if time.Now().After(deadline) {
			t.Fatalf("runtime received %q", e.h.Typed(added.NewID))
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Lifecycle.
	if code, _, bad := e.act(all, added.NewID, direct.VerbRename, map[string]string{"name": "Builder"}, "r-1"); code != 200 {
		t.Fatalf("rename: %d %+v", code, bad)
	}
	e.eventually(all, "renamed", func(inv map[string]direct.Session) bool { return strings.Contains(inv[added.NewID].Title, "Builder") })
	if code, _, bad := e.act(all, added.NewID, direct.VerbStart, nil, "s-1"); code != 200 {
		t.Fatalf("start: %d %+v", code, bad)
	}
	code, arch, bad := e.act(all, root, direct.VerbArchive, nil, "ar-1")
	if code != 200 || !slices.Contains(arch.Affected, root) || !slices.Contains(arch.Affected, added.NewID) {
		t.Fatalf("archive root: %d %+v %+v", code, arch, bad)
	}
	e.eventually(all, "archived", func(inv map[string]direct.Session) bool { return inv[root].Archived })
	// Inherited archive: the member cannot be steered or restored on its own.
	if code, _, bad := e.act(all, added.NewID, direct.VerbPrompt, map[string]string{"text": "x"}, "p-2"); code != 409 || bad.Code != direct.CodeArchived {
		t.Fatalf("prompt archived member: %d %+v", code, bad)
	}
	if code, _, bad := e.act(all, added.NewID, direct.VerbUnarchive, nil, "u-0"); code != 409 || bad.Code != direct.CodeArchived {
		t.Fatalf("unarchive member of archived root: %d %+v", code, bad)
	}
	if code, _, bad := e.act(all, root, direct.VerbUnarchive, nil, "u-1"); code != 200 {
		t.Fatalf("unarchive root: %d %+v", code, bad)
	}
	e.eventually(all, "restored", func(inv map[string]direct.Session) bool {
		return !inv[root].Archived && inv[added.NewID].RootID == root
	})

	// A workgroup-scoped grant acts only inside its workgroup and never creates roots.
	group := e.enroll(everyPerm, direct.ResourceScope{Kind: direct.ResourceWorkgroups, Workgroups: []string{root}})
	if code, _, bad := e.act(group, "", direct.VerbNewWorkgroupEmpty, map[string]string{"repos": ""}, "g-1"); code != 403 || bad.Code != direct.CodeDenied {
		t.Fatalf("scoped root creation: %d %+v", code, bad)
	}
	if code, _, bad := e.act(group, empty.NewID, direct.VerbAddAgent, nil, "g-2"); code != 404 {
		t.Fatalf("scoped add-agent outside: %d %+v", code, bad)
	}
	if code, ga, bad := e.act(group, root, direct.VerbAddAgent, map[string]string{"repos": "app"}, "g-3"); code != 200 || ga.RootID != root {
		t.Fatalf("scoped add-agent inside: %d %+v %+v", code, ga, bad)
	}
	for id := range e.inventory(group) {
		if id == empty.NewID || id == "console" {
			t.Fatalf("scoped inventory leaks %s", id)
		}
	}

	// An S1 grant still has only its S1 verbs against the live daemon.
	steer := e.enroll([]string{direct.PermRead, direct.PermSteer}, direct.ResourceScope{Kind: direct.ResourceAllSessions})
	for _, verb := range []string{direct.VerbRename, direct.VerbArchive, direct.VerbAddAgent} {
		if code, _, bad := e.act(steer, root, verb, map[string]string{}, "s1-"+verb); code != 403 || bad.Code != direct.CodeDenied {
			t.Fatalf("S1 %s: %d %+v", verb, code, bad)
		}
	}
	if code, _, _ := e.act(steer, "", direct.VerbNewWorkgroupEmpty, map[string]string{"repos": ""}, "s1-root"); code != 403 {
		t.Fatalf("S1 root creation: %d", code)
	}
	_ = http.StatusOK
}

// A client that creates a workgroup and immediately acts on the returned id
// (before any explicit inventory poll) must not be told the id does not exist.
func TestDirectLiveFollowUpOnNewIDBeforePoll(t *testing.T) {
	e := newE2E(t)
	all := e.enroll(everyPerm, direct.ResourceScope{Kind: direct.ResourceAllSessions})
	for i := range 20 {
		code, created, bad := e.act(all, "", direct.VerbNewWorkgroupEmpty, map[string]string{"repos": ""}, "c"+string(rune('a'+i)))
		if code != 200 {
			t.Fatalf("create: %d %+v", code, bad)
		}
		code, added, bad := e.act(all, created.NewID, direct.VerbAddAgent, nil, "a"+string(rune('a'+i)))
		if code != 200 || added.RootID != created.NewID {
			t.Fatalf("iteration %d: immediate add-agent to new root %s: %d %+v", i, created.NewID, code, bad)
		}
		code, _, bad = e.act(all, added.NewID, direct.VerbPrompt, map[string]string{"text": "go"}, "p"+string(rune('a'+i)))
		if code != 200 {
			t.Fatalf("iteration %d: immediate prompt to new agent: %d %+v", i, code, bad)
		}
	}
}
