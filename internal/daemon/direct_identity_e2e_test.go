package daemon_test

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"amux/internal/core"
	"amux/internal/daemon"
	"amux/internal/direct"
	"amux/internal/store"
)

// Creation replay identity against a real daemon and store (only the agent
// runtime is fake): live create, archive, delete and id reuse, through the
// direct server and over authenticated host connections.

func noIncarnation(t *testing.T, b []byte, idents ...store.Identity) {
	t.Helper()
	for _, id := range idents {
		for _, inc := range []string{id.Incarnation, id.RootIncarnation} {
			if inc != "" && strings.Contains(string(b), inc) {
				t.Fatalf("client body leaks an incarnation: %s", b)
			}
		}
	}
}

func currentIdentity(t *testing.T, id string) (store.SessionIdentity, bool) {
	t.Helper()
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m, err := db.SessionIdentities([]string{id})
	if err != nil {
		t.Fatal(err)
	}
	s, ok := m[id]
	return s, ok
}

func TestDirectLiveReplayIdentity(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required to track a repository")
	}
	e := newE2E(t)
	trackLocalRepo(t, "app")
	all := e.enroll(everyPerm, direct.ResourceScope{Kind: direct.ResourceAllSessions})

	code, wg, bad := e.act(all, "", direct.VerbNewWorkgroupEmpty, map[string]string{"name": "W", "repos": "app"}, "wg")
	if code != 200 || wg.NewID == "" {
		t.Fatalf("new workgroup: %d %+v", code, bad)
	}
	root := wg.NewID
	e.eventually(all, "root", func(inv map[string]direct.Session) bool { return inv[root].IsRoot })
	code, ag, bad := e.act(all, root, direct.VerbAddAgent, map[string]string{"repos": "app", "agent": "claude"}, "ag")
	if code != 200 || ag.NewID == "" || ag.RootID != root {
		t.Fatalf("add-agent: %d %+v", code, bad)
	}
	member := ag.NewID
	rootIdent, _ := currentIdentity(t, root)
	memberIdent, _ := currentIdentity(t, member)
	if memberIdent.RootID != root || rootIdent.Incarnation == "" || memberIdent.Incarnation == "" {
		t.Fatalf("store identities: %+v %+v", rootIdent, memberIdent)
	}
	_, statusBody := e.call("POST", direct.PathSession+"/"+root+"/actions", all,
		direct.ActionRequest{Verb: direct.VerbAddAgent, Fields: map[string]string{"repos": "app", "agent": "claude"}, IdemKey: "ag", Async: true})
	noIncarnation(t, statusBody, store.Identity{Incarnation: rootIdent.Incarnation, RootIncarnation: memberIdent.Incarnation})
	_, st := e.createAsync(all, root, direct.VerbAddAgent, map[string]string{"repos": "app", "agent": "claude"}, "ag")
	if st.Phase != direct.CreatePhaseReady || st.NewID != member {
		t.Fatalf("async replay: %+v", st)
	}

	// Archived but the same session: still replays.
	if code, _, bad := e.act(all, member, direct.VerbArchive, nil, "arch"); code != 200 {
		t.Fatalf("archive: %d %+v", code, bad)
	}
	e.eventually(all, "archived", func(inv map[string]direct.Session) bool { return inv[member].Archived })
	if code, again, bad := e.act(all, root, direct.VerbAddAgent, map[string]string{"repos": "app", "agent": "claude"}, "ag"); code != 200 || again.NewID != member {
		t.Fatalf("archived replay: %d %+v %+v", code, again, bad)
	}

	// Deleted on the host: every replay path refuses without ids.
	c, err := e.h.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Dispatch(core.Action{Action: core.ActionDelete, ID: member}); err != nil {
		t.Fatalf("host delete: %v", err)
	}
	c.Close()
	checkUnavailable := func(what string) {
		t.Helper()
		code, again, bad := e.act(all, root, direct.VerbAddAgent, map[string]string{"repos": "app", "agent": "claude"}, "ag")
		if code != 409 || bad.Code != direct.CodeCreatedTargetUnavailable || again.NewID != "" {
			t.Fatalf("%s: sync replay %d %+v", what, code, bad)
		}
		_, s2 := e.createAsync(all, root, direct.VerbAddAgent, map[string]string{"repos": "app", "agent": "claude"}, "ag")
		if s2.Phase != direct.CreatePhaseFailed || s2.Code != direct.CodeCreatedTargetUnavailable || s2.Uncertain || s2.NewID != "" {
			t.Fatalf("%s: async replay %+v", what, s2)
		}
		code, s3, _ := e.creation(all, st.CreationID, "")
		if code != 200 || s3.Code != direct.CodeCreatedTargetUnavailable || s3.NewID != "" {
			t.Fatalf("%s: status %d %+v", what, code, s3)
		}
	}
	checkUnavailable("deleted")

	// The id reused by an unrelated session (the store's own insert path).
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateSession(store.Session{ID: member, RootID: root, Agent: "claude"}, ""); err != nil {
		t.Fatal(err)
	}
	db.Close()
	e.h.Poll()
	checkUnavailable("reused")

	// The root replaced: the new-workgroup record refuses too.
	db, _ = store.Open()
	old, _, _ := db.GetSession(root)
	db.DeleteSession(member)
	db.DeleteSession(root)
	// Same harness, so the existing account binding for the id is reused.
	if _, err := db.CreateSession(store.Session{ID: root, Scope: store.ScopeWork, Agent: old.Agent}, ""); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if code, again, bad := e.act(all, "", direct.VerbNewWorkgroupEmpty, map[string]string{"name": "W", "repos": "app"}, "wg"); code != 409 || bad.Code != direct.CodeCreatedTargetUnavailable || again.NewID != "" {
		t.Fatalf("replaced root replay: %d %+v", code, bad)
	}
}

// The host identity protocol on a live daemon: negotiation per connection,
// the identity is exactly the store's, a pinned root can only refuse, a
// non-creation verb is refused, and ordinary results carry no identity.
func TestLiveHostCreationIdentityProtocol(t *testing.T) {
	h := daemon.NewLiveHostForTest(t)
	ctx := context.Background()
	c, err := h.Dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	empty := core.Action{Action: core.ActionCreateWorkspace, Fields: map[string]string{"name": "x", "repos": ""}}
	// Not negotiated on this connection: refused without sending.
	if _, err := c.DispatchIdentity(empty, daemon.IdentityRequest{}); !errors.Is(err, daemon.ErrDaemonUpgradeRequired) {
		t.Fatalf("un-negotiated dispatch = %v", err)
	}
	if n := countRoots(t); n != 0 {
		t.Fatalf("un-negotiated dispatch created %d roots", n)
	}
	if err := c.NegotiateCreationIdentity(); err != nil {
		t.Fatal(err)
	}
	root, err := c.DispatchIdentity(empty, daemon.IdentityRequest{})
	if err != nil || !root.Complete() || root.RootID != root.ID {
		t.Fatalf("root = %+v %v", root, err)
	}
	if cur, _ := currentIdentity(t, root.ID); cur.Incarnation != root.Incarnation {
		t.Fatalf("returned %q, store has %q", root.Incarnation, cur.Incarnation)
	}
	add := core.Action{Action: core.ActionAddAgent, ID: root.ID, Fields: map[string]string{"agent": "claude"}}
	// A forged pin refuses and creates nothing.
	if _, err := c.DispatchIdentity(add, daemon.IdentityRequest{ExpectRoot: "00000000000000000000000000000000"}); err == nil {
		t.Fatal("forged expected root accepted")
	}
	if n := countMembers(t, root.ID); n != 0 {
		t.Fatalf("forged pin created %d members", n)
	}
	m, err := c.DispatchIdentity(add, daemon.IdentityRequest{ExpectRoot: root.Incarnation})
	if err != nil || m.RootID != root.ID || m.RootIncarnation != root.Incarnation {
		t.Fatalf("member = %+v %v", m, err)
	}
	ids, err := c.SessionIdentities([]string{m.ID, root.ID, "missing"})
	if err != nil || ids[m.ID].Incarnation != m.Incarnation || ids[m.ID].RootID != root.ID || len(ids) != 2 {
		t.Fatalf("identities = %+v %v", ids, err)
	}
	// A non-creation verb with the opt-in is refused before it runs.
	if _, err := c.DispatchIdentity(core.Action{Action: core.ActionRename, ID: m.ID, Fields: map[string]string{"name": "n"}}, daemon.IdentityRequest{}); err == nil {
		t.Fatal("rename with identity accepted")
	}
	// Ordinary dispatch on the same connection: legacy id-only result.
	if err := c.Send(core.Action{Action: core.ActionAddAgent, ID: root.ID, Fields: map[string]string{"agent": "claude"}}); err != nil {
		t.Fatal(err)
	}
	for {
		f, err := c.Next()
		if err != nil {
			t.Fatal(err)
		}
		if f.Result != nil {
			if !f.Result.OK || f.Result.NewID == "" || f.Identity != nil {
				t.Fatalf("legacy result = %+v identity=%+v", f.Result, f.Identity)
			}
			break
		}
	}
	// A reconnect negotiates again: a fresh connection starts un-negotiated.
	c2, err := h.Dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if _, err := c2.DispatchIdentity(add, daemon.IdentityRequest{ExpectRoot: root.Incarnation}); !errors.Is(err, daemon.ErrDaemonUpgradeRequired) {
		t.Fatalf("fresh connection dispatch = %v", err)
	}
}

func countRoots(t *testing.T) int {
	t.Helper()
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	roots, _ := db.Roots()
	n := 0
	for _, r := range roots {
		if r.Scope == store.ScopeWork {
			n++
		}
	}
	return n
}

func countMembers(t *testing.T, root string) int {
	t.Helper()
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	kids, _ := db.Children(root)
	return len(kids)
}
