package direct

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"amux/internal/core"
	"amux/internal/hostbackend"
	amuxstore "amux/internal/store"
)

// Acceptance for creation replay identity (creation_identity.go). The fake
// backend's incarnations are "inc-<id>" unless a test replaces them with
// setInc; "" deletes the session.

type identityRig struct {
	t    *testing.T
	root string
	a    *Authority
	f    *fakeBackend
	s    *Server
	tok  string
}

func newIdentityRig(t *testing.T) *identityRig {
	t.Helper()
	r := &identityRig{t: t, root: t.TempDir(), f: workflowFake()}
	r.a = openTestAuthority(t, r.root, Lifetimes{}, nil)
	r.s = newTestServer(t, r.a, r.f)
	r.tok = enroll(t, r.a, allPerms, allSessions).AccessToken
	return r
}

// post sends one synchronous action/creation and checks that no incarnation
// ever reaches the client.
func (r *identityRig) post(sid, verb string, fields map[string]string, key string) (int, string, ActionResponse) {
	r.t.Helper()
	w := do(r.s, "POST", createPath(sid), r.tok, body(verb, fields, key))
	noLeak(r.t, w.Body.Bytes())
	var res ActionResponse
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	return w.Code, apiCode(r.t, w), res
}

func (r *identityRig) postAsync(sid, verb string, key string) (int, CreationStatus, string) {
	r.t.Helper()
	w := do(r.s, "POST", createPath(sid), r.tok, asyncBody(verb, map[string]string{"repos": "api"}, key))
	noLeak(r.t, w.Body.Bytes())
	var st CreationStatus
	_ = json.Unmarshal(w.Body.Bytes(), &st)
	return w.Code, st, apiCode(r.t, w)
}

func (r *identityRig) status(id string) (int, CreationStatus, string) {
	r.t.Helper()
	w := do(r.s, "GET", PathCreations+"/"+id, r.tok, "")
	noLeak(r.t, w.Body.Bytes())
	var st CreationStatus
	_ = json.Unmarshal(w.Body.Bytes(), &st)
	return w.Code, st, apiCode(r.t, w)
}

func (r *identityRig) grantsFile() []byte {
	r.t.Helper()
	b, err := os.ReadFile(filepath.Join(r.root, grantsFile))
	if err != nil {
		r.t.Fatal(err)
	}
	return b
}

// noLeak fails if a client-visible body carries an incarnation or its field.
func noLeak(t *testing.T, b []byte) {
	t.Helper()
	for _, s := range []string{"inc-", "repl-", "incarnation", "newInc", "rootInc"} {
		if bytes.Contains(b, []byte(s)) {
			t.Fatalf("client body leaks %q: %s", s, b)
		}
	}
}

// The success record keeps both incarnations, exactly as the daemon returned
// them; the client sees ids only.
func TestCreationRecordsIdentityOnce(t *testing.T) {
	r := newIdentityRig(t)
	r.f.newID = "agent-1"
	code, ec, res := r.post("wg1", VerbAddAgent, map[string]string{"repos": "api"}, "k")
	if code != 200 || res.NewID != "agent-1" || res.RootID != "wg1" {
		t.Fatalf("create: %d %s %+v", code, ec, res)
	}
	if r.f.lastExpect != "inc-wg1" {
		t.Fatalf("add-agent was not pinned to the admitted root: %q", r.f.lastExpect)
	}
	rec := recordOf(t, r.a, "k")
	if rec.NewIncarnation != "inc-agent-1" || rec.RootIncarnation != "inc-wg1" {
		t.Fatalf("record identity = %q/%q", rec.NewIncarnation, rec.RootIncarnation)
	}
	if !bytes.Contains(r.grantsFile(), []byte(`"newInc":"inc-agent-1"`)) {
		t.Fatal("identity not persisted")
	}
	r.f.newID = "root-9"
	code, _, res = r.post("", VerbNewWorkgroupEmpty, map[string]string{"name": "x", "repos": "api"}, "w")
	if code != 200 || res.NewID != "root-9" || res.RootID != "root-9" {
		t.Fatalf("new workgroup: %d %+v", code, res)
	}
	if rec := recordOf(t, r.a, "w"); rec.NewIncarnation != "inc-root-9" || rec.RootIncarnation != "inc-root-9" {
		t.Fatalf("root record identity = %+v", rec)
	}
}

// recordOf copies the single grant's record for key under the lock.
func recordOf(t *testing.T, a *Authority, key string) idemRecord {
	t.Helper()
	cid := clientOf(t, a)
	a.mu.Lock()
	defer a.mu.Unlock()
	return *a.state.Grants[cid].Idem[key]
}

func clientOf(t *testing.T, a *Authority) string {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	for id := range a.state.Grants {
		return id
	}
	t.Fatal("no grant")
	return ""
}

// All three replay paths (sync POST, async POST, GET status) answer with the
// ids only while both identities match; a deleted or reused session, a
// replaced root, or a member moved to another root answers
// created_target_unavailable without ids, and the creation never runs again.
func TestReplayChecksCurrentIdentity(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(f *fakeBackend)
	}{
		{"session deleted", func(f *fakeBackend) { f.setInc("agent-1", "") }},
		{"session id reused", func(f *fakeBackend) { f.setInc("agent-1", "repl-a") }},
		{"root deleted", func(f *fakeBackend) { f.setInc("wg1", "") }},
		{"root id reused", func(f *fakeBackend) { f.setInc("wg1", "repl-r") }},
		{"member moved", func(f *fakeBackend) { f.mu.Lock(); f.parents["agent-1"] = "wg2"; f.mu.Unlock() }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newIdentityRig(t)
			r.f.newID = "agent-1"
			if code, ec, _ := r.post("wg1", VerbAddAgent, map[string]string{"repos": "api"}, "k"); code != 200 {
				t.Fatalf("create: %d %s", code, ec)
			}
			code, st, ec := r.postAsync("wg1", VerbAddAgent, "k")
			if code != 202 || st.Phase != CreatePhaseReady || st.NewID != "agent-1" {
				t.Fatalf("async replay before change: %d %s %+v", code, ec, st)
			}
			// Unchanged: every path replays the ids.
			if code, _, res := r.post("wg1", VerbAddAgent, map[string]string{"repos": "api"}, "k"); code != 200 || res.NewID != "agent-1" {
				t.Fatalf("sync replay before change: %d", code)
			}
			c.mutate(r.f)
			if code, ec, res := r.post("wg1", VerbAddAgent, map[string]string{"repos": "api"}, "k"); code != 409 || ec != CodeCreatedTargetUnavailable || res.NewID != "" {
				t.Fatalf("sync replay: %d %s %+v", code, ec, res)
			}
			for _, got := range []func() (int, CreationStatus, string){
				func() (int, CreationStatus, string) { return r.postAsync("wg1", VerbAddAgent, "k") },
				func() (int, CreationStatus, string) { return r.status(st.CreationID) },
			} {
				code, s2, ec := got()
				if (code != 200 && code != 202) || s2.Phase != CreatePhaseFailed || s2.Code != CodeCreatedTargetUnavailable ||
					s2.Uncertain || s2.NewID != "" || s2.RootID != "" || s2.Version != 3 {
					t.Fatalf("async replay: %d %s %+v", code, ec, s2)
				}
			}
			if n := r.f.applies.Load(); n != 1 {
				t.Fatalf("applies=%d: a replay ran the creation again", n)
			}
		})
	}
}

// Archived but existing with the same identity is still the created session.
func TestReplayArchivedSameIdentity(t *testing.T) {
	r := newIdentityRig(t)
	r.f.newID = "agent-1"
	r.post("wg1", VerbAddAgent, map[string]string{"repos": "api"}, "k")
	rows := workflowRows()
	for i := range rows {
		rows[i].Archived = true
	}
	r.f.setRows(rows...)
	if code, _, res := r.post("wg1", VerbAddAgent, map[string]string{"repos": "api"}, "k"); code != 200 || res.NewID != "agent-1" {
		t.Fatalf("archived replay: %d", code)
	}
}

// A record without incarnations (an older server's success) never replays
// its ids: created_identity_unverifiable on every path, nothing read.
func TestReplayOldRecordUnverifiable(t *testing.T) {
	r := newIdentityRig(t)
	r.f.newID = "agent-1"
	r.post("wg1", VerbAddAgent, map[string]string{"repos": "api"}, "k")
	_, st, _ := r.postAsync("wg1", VerbAddAgent, "k")
	cid := clientOf(t, r.a) // takes r.a.mu itself: before the lock below
	r.a.mu.Lock()
	rec := r.a.state.Grants[cid].Idem["k"]
	rec.NewIncarnation, rec.RootIncarnation = "", ""
	r.a.mu.Unlock()
	reads := r.f.identReads.Load()
	if code, ec, res := r.post("wg1", VerbAddAgent, map[string]string{"repos": "api"}, "k"); code != 409 || ec != CodeCreatedIdentityUnverifiable || res.NewID != "" {
		t.Fatalf("sync: %d %s", code, ec)
	}
	for _, got := range []func() (int, CreationStatus, string){
		func() (int, CreationStatus, string) { return r.postAsync("wg1", VerbAddAgent, "k") },
		func() (int, CreationStatus, string) { return r.status(st.CreationID) },
	} {
		_, s2, _ := got()
		if s2.Phase != CreatePhaseFailed || s2.Code != CodeCreatedIdentityUnverifiable || s2.Uncertain || s2.NewID != "" {
			t.Fatalf("async: %+v", s2)
		}
	}
	if r.f.identReads.Load() != reads {
		t.Fatal("an unverifiable record read identities")
	}
}

// A failure to read identities (transient, or a downgraded daemon) answers
// 503 created_identity_unavailable — never unavailable, a not-run code — on
// every replay path and leaves the record (and grants.json) byte-identical; a
// later replay answers normally.
func TestReplayTransientIdentityFailure(t *testing.T) {
	r := newIdentityRig(t)
	r.f.newID = "agent-1"
	r.post("wg1", VerbAddAgent, map[string]string{"repos": "api"}, "k")
	_, st, _ := r.postAsync("wg1", VerbAddAgent, "k")
	before := r.grantsFile()
	for _, fail := range []error{errors.New("socket reset"), nil} {
		r.f.mu.Lock()
		r.f.identErr = fail
		r.f.mu.Unlock()
		r.f.noIdentity.Store(fail == nil) // the second round: daemon downgraded
		code, ec, _ := r.post("wg1", VerbAddAgent, map[string]string{"repos": "api"}, "k")
		// Never unavailable (a "did not run" code): the creation ran.
		want := CodeCreatedIdentityUnavailable
		if notRunCodes(t)[ec] {
			t.Fatalf("replay verification failure answered a not-run code %q", ec)
		}
		if code != 503 || ec != want {
			t.Fatalf("sync: %d %s", code, ec)
		}
		if code, _, ec := r.postAsync("wg1", VerbAddAgent, "k"); code != 503 || ec != want {
			t.Fatalf("async POST: %d %s", code, ec)
		}
		if code, _, ec := r.status(st.CreationID); code != 503 || ec != want {
			t.Fatalf("GET: %d %s", code, ec)
		}
		if after := r.grantsFile(); !bytes.Equal(before, after) {
			t.Fatal("a failed identity read changed the record")
		}
	}
	r.f.noIdentity.Store(false)
	if code, _, res := r.post("wg1", VerbAddAgent, map[string]string{"repos": "api"}, "k"); code != 200 || res.NewID != "agent-1" {
		t.Fatalf("recovered replay: %d", code)
	}
}

// An older daemon is refused before anything is sent or recorded: no create
// effect, the same key works once the daemon is upgraded, and capabilities
// report it.
func TestOldDaemonRefusedBeforeCreation(t *testing.T) {
	r := newIdentityRig(t)
	r.f.noIdentity.Store(true)
	w := do(r.s, "GET", PathCapabilities, r.tok, "")
	var caps CapabilitiesResponse
	_ = json.Unmarshal(w.Body.Bytes(), &caps)
	if !caps.DaemonUpgradeRequired || caps.CreateWorkgroup || caps.AsyncCreate {
		t.Fatalf("capabilities: %s", w.Body.String())
	}
	r.f.newID = "agent-1"
	if code, ec, _ := r.post("wg1", VerbAddAgent, map[string]string{"repos": "api"}, "k"); code != 503 || ec != CodeDaemonUpgradeRequired {
		t.Fatalf("sync add-agent: %d %s", code, ec)
	}
	if code, ec, _ := r.post("", VerbNewWorkgroupEmpty, map[string]string{"name": "x", "repos": "api"}, "w"); code != 503 || ec != CodeDaemonUpgradeRequired {
		t.Fatalf("sync new-workgroup: %d %s", code, ec)
	}
	if code, _, ec := r.postAsync("wg1", VerbAddAgent, "a"); code != 503 || ec != CodeDaemonUpgradeRequired {
		t.Fatalf("async: %d %s", code, ec)
	}
	if n := r.f.applies.Load(); n != 0 {
		t.Fatalf("applies=%d on an old daemon", n)
	}
	cid := clientOf(t, r.a)
	r.a.mu.Lock()
	n := len(r.a.state.Grants[cid].Idem)
	r.a.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d keys recorded for refused creations", n)
	}
	// Ordinary actions keep working.
	if code, ec, _ := r.post("a1", VerbRename, map[string]string{"name": "n"}, "r"); code != 200 {
		t.Fatalf("rename on old daemon: %d %s", code, ec)
	}
	r.f.noIdentity.Store(false)
	if code, _, res := r.post("wg1", VerbAddAgent, map[string]string{"repos": "api"}, "k"); code != 200 || res.NewID != "agent-1" {
		t.Fatalf("same key after upgrade: %d", code)
	}
}

// The daemon is downgraded between the precheck and the dispatch connection:
// the dispatch negotiates again and refuses; the key records a known refusal
// (not proven-not-run in the allowlist) and nothing ran.
func TestDowngradeBetweenPrecheckAndDispatch(t *testing.T) {
	r := newIdentityRig(t)
	r.f.newID = "agent-1"
	f := &downgradingFake{fakeBackend: r.f}
	r.s = newTestServer(t, r.a, f)
	code, ec, _ := r.post("wg1", VerbAddAgent, map[string]string{"repos": "api"}, "k")
	if code != 503 || ec != CodeDaemonUpgradeRequired || r.f.applies.Load() != 0 {
		t.Fatalf("%d %s applies=%d", code, ec, r.f.applies.Load())
	}
	if code, ec, _ := r.post("wg1", VerbAddAgent, map[string]string{"repos": "api"}, "k"); ec != CodeDaemonUpgradeRequired || code != 503 {
		t.Fatalf("replayed refusal: %d %s", code, ec)
	}
}

type downgradingFake struct{ *fakeBackend }

func (d *downgradingFake) ApplyIdentity(ctx context.Context, a core.Action, root string) (amuxstore.Identity, error) {
	return amuxstore.Identity{}, hostbackend.ErrUpgradeRequired
}

// An async add-agent is pinned to the root admitted at POST: a root replaced
// while the job waits for a slot is refused before dispatch (not_found, a
// proven-not-run code), never created under the replacement.
func TestAsyncJobPinnedToAdmittedRoot(t *testing.T) {
	r := newIdentityRig(t)
	r.f.newID = "agent-1"
	release := occupySlots(r.s)
	code, st, ec := r.postAsync("wg1", VerbAddAgent, "k")
	if code != 202 || st.Phase != CreatePhaseQueued {
		t.Fatalf("queued: %d %s %+v", code, ec, st)
	}
	r.f.setInc("wg1", "repl-r")
	release()
	got := awaitTerminal(t, r.s, r.tok, st.CreationID)
	if got.Phase != CreatePhaseFailed || got.Code != CodeNotFound || got.Uncertain {
		t.Fatalf("pinned job: %+v", got)
	}
	if n := r.f.applies.Load(); n != 0 {
		t.Fatalf("applies=%d under a replaced root", n)
	}
}

// targetSnapshot waits for a just-created session only while its current
// identity is the created one; a replacement (or an unreadable identity) is
// never waited for.
func TestCreatedWaitOnlyForSameIncarnation(t *testing.T) {
	r := newIdentityRig(t)
	r.f.newID = "agent-1"
	r.post("wg1", VerbAddAgent, map[string]string{"repos": "api"}, "k")
	// agent-1 is not in the fake's rows (the inventory has not published it).
	r.f.setInc("agent-1", "repl-a")
	start := time.Now()
	code, ec, _ := r.post("agent-1", VerbRename, map[string]string{"name": "n"}, "r1")
	if code != 404 || ec != CodeNotFound || time.Since(start) > createdWait/2 {
		t.Fatalf("replacement waited: %d %s after %v", code, ec, time.Since(start))
	}
	r.f.setInc("agent-1", "inc-agent-1")
	start = time.Now()
	code, _, _ = r.post("agent-1", VerbRename, map[string]string{"name": "n"}, "r2")
	if code != 404 || time.Since(start) < createdWait/2 {
		t.Fatalf("the created session was not waited for: %d after %v", code, time.Since(start))
	}
}


// notRunCodes is the published set of creation failure codes that prove the
// creation never ran (CreationStatus' doc comment), which clients may answer
// with a fresh idem_key.
func notRunCodes(t *testing.T) map[string]bool {
	t.Helper()
	src, err := os.ReadFile("wire.go")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(strings.ReplaceAll(string(src), "//", " ")), " ")
	const lead = "Only these failed codes prove the creation was never sent (zero effects): "
	i := strings.Index(text, lead)
	if i < 0 {
		t.Fatal("the published not-run list moved")
	}
	list := text[i+len(lead):]
	list = list[:strings.Index(list, ". ")]
	out := map[string]bool{}
	for _, c := range strings.FieldsFunc(list, func(r rune) bool { return r == ',' || r == ' ' }) {
		if c != "and" {
			out[c] = true
		}
	}
	return out
}

// The identity replay codes and daemon_upgrade_required are outside the
// published not-run set: a client never answers them with a fresh key.
func TestIdentityCodesAreNotNotRun(t *testing.T) {
	set := notRunCodes(t)
	if !set[CodeUnavailable] || !set[CodeNotInCatalog] || len(set) != 8 {
		t.Fatalf("parsed not-run set = %v", set)
	}
	for _, c := range []string{CodeCreatedTargetUnavailable, CodeCreatedIdentityUnverifiable, CodeCreatedIdentityUnavailable, CodeDaemonUpgradeRequired} {
		if set[c] {
			t.Fatalf("%s is in the not-run set", c)
		}
	}
}

// Access ending during the identity lookup answers 401, not a 503.
func TestReplayRevokedDuringLookup(t *testing.T) {
	r := newIdentityRig(t)
	r.f.newID = "agent-1"
	r.post("wg1", VerbAddAgent, map[string]string{"repos": "api"}, "k")
	r.f.revokeOnRead = func() { r.a.Revoke(clientOf(t, r.a), "test") }
	code, ec, res := r.post("wg1", VerbAddAgent, map[string]string{"repos": "api"}, "k")
	if code != 401 || ec != CodeAuthRevoked || res.NewID != "" {
		t.Fatalf("revoked during lookup: %d %s", code, ec)
	}
}
