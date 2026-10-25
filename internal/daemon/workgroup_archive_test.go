package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"amux/internal/access"
	"amux/internal/core"
	"amux/internal/sessionrpc"
	"amux/internal/store"
	"amux/internal/wsops"
)

func archiveGroupFixture(t *testing.T) (*Daemon, *sessionRuntime, map[string]access.Principal, map[string]*fakeInstance) {
	t.Helper()
	d, runtime, principals := sessionRuntimeFixture(t,
		store.Session{ID: "root", Scope: store.ScopeWork, Agent: "claude"},
		store.Session{ID: "member", RootID: "root", Agent: "claude"},
		store.Session{ID: "done", RootID: "root", Agent: "claude", Archived: true, ArchivedAt: 123},
		store.Session{ID: "foreign", Scope: store.ScopeWork, Agent: "claude"},
		store.Session{ID: "moved", RootID: "foreign", Agent: "claude"},
	)
	d.sessionRPC = runtime
	eng := newFakeEngine()
	d.engine = eng
	instances := make(map[string]*fakeInstance)
	for _, id := range []string{"root", "member", "done", "foreign", "moved"} {
		instances[id] = eng.running(id)
		if _, err := d.permissions.observe(id, instances[id]); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(runtime.close)
	return d, runtime, principals, instances
}

func groupArchiveRequest(p access.Principal) sessionrpc.DispatchRequest {
	return sessionrpc.DispatchRequest{Principal: p, RequestID: "0123456789abcdef0123456789abcdef",
		Call: sessionrpc.Call{Kind: sessionrpc.CallOperation, Route: access.RouteAction,
			Verb: core.ActionSetArchived, ID: p.SubjectID, Fields: map[string]string{"archived": "true"}}}
}

func assertGroupStopped(t *testing.T, d *Daemon, principals map[string]access.Principal, instances map[string]*fakeInstance) {
	t.Helper()
	for _, id := range []string{"root", "member", "done"} {
		if instances[id].Alive() {
			t.Errorf("archived %s runtime survived", id)
		}
		if p, ok := principals[id]; ok && d.authority.Valid(context.Background(), p) == nil {
			t.Errorf("archived %s credential survived", id)
		}
	}
	for _, id := range []string{"foreign", "moved"} {
		if !instances[id].Alive() {
			t.Errorf("foreign %s runtime stopped", id)
		}
		if err := d.authority.Valid(context.Background(), principals[id]); err != nil {
			t.Errorf("foreign %s credential revoked: %v", id, err)
		}
	}
}

func TestCoordinatorArchiveKeepsReceiptUntilGroupCommitAndCleanup(t *testing.T) {
	d, runtime, principals, instances := archiveGroupFixture(t)
	ctx := context.Background()
	runtime.completions.minimum = 20 * time.Millisecond
	runtime.completions.runtime = 20 * time.Millisecond
	runtime.completions.poll = time.Millisecond
	request := groupArchiveRequest(principals["root"])
	result, err := runtime.dispatch(ctx, request)
	if err != nil || result.Status != sessionrpc.StatusOK || result.Receipt == nil {
		t.Fatalf("completion = %+v, %v", result, err)
	}
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, id := range []string{"root", "member", "done"} {
		s, ok, err := db.GetSession(id)
		if err != nil || !ok || s.Archived != (id != "member") {
			t.Fatalf("response changed member completion state: %s %+v %v", id, s, err)
		}
		effective, found, err := runtime.resolver.Lookup(ctx, id)
		if err != nil || !found || !effective.Archived {
			t.Fatalf("%s not effectively archived before response: %+v %v", id, effective, err)
		}
		if _, _, err := d.sessionAccessForLaunch(ctx, id); err == nil {
			t.Fatalf("archived %s eligible for launch", id)
		}
		if err := runtime.authorize(ctx, principals[id], sessionrpc.Call{Kind: sessionrpc.CallOperation, Route: access.RouteQuery, Verb: core.QuerySnapshot}); err == nil {
			t.Fatalf("archived %s retained general authority", id)
		}
		if id == "done" && s.ArchivedAt != 123 {
			t.Fatal("changed completed member timestamp")
		}
	}
	if !instances["root"].Alive() {
		t.Fatal("caller stopped before response")
	}
	result.Receipt.ResponsePersisted(sessionrpc.PersistedResponse{RequestID: request.RequestID, ResponseDigest: "digest"})
	if err := runtime.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if !instances["root"].Alive() {
		t.Fatal("reconcile killed pending receipt caller")
	}
	if instances["member"].Alive() || d.authority.Valid(ctx, principals["member"]) == nil {
		t.Fatal("member survived workgroup archive reconciliation")
	}
	receipt := sessionrpc.Receipt{RequestID: request.RequestID, ResponseDigest: "digest"}
	if !runtime.completions.authorizeReceipt(principals["root"], receipt) {
		t.Fatal("matching receipt denied")
	}
	receipt.ResponseDigest = "other"
	if runtime.completions.authorizeReceipt(principals["root"], receipt) {
		t.Fatal("unrelated receipt allowed")
	}
	if err := runtime.authorize(ctx, principals["root"], sessionrpc.Call{Kind: sessionrpc.CallOperation, Route: access.RouteQuery, Verb: core.QuerySnapshot}); err == nil {
		t.Fatal("archived coordinator retained general access")
	}
	result.Receipt.Settled(sessionrpc.ReceiptSettlement{RequestID: request.RequestID, Received: true})
	deadline := time.Now().Add(time.Second)
	for instances["root"].Alive() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	assertGroupStopped(t, d, principals, instances)

	// One host group restore brings back previously active members, while
	// independently completed members retain their archive state and timestamps.
	resultHost := d.handle(ctx, core.Action{Action: core.ActionSetArchived, ID: "root", Fields: map[string]string{"archived": "false"}})
	if !resultHost.OK {
		t.Fatalf("host group restore: %+v", resultHost)
	}
	for _, id := range []string{"root", "member"} {
		current, err := d.authority.Current(ctx, access.SubjectSession, id)
		if err != nil || current.Generation <= principals[id].Generation {
			t.Fatalf("%s regrant: %+v %v", id, current, err)
		}
		if err := d.authority.Valid(ctx, principals[id]); err == nil {
			t.Fatalf("old %s principal revived", id)
		}
		effective, found, err := runtime.resolver.Lookup(ctx, id)
		if err != nil || !found || effective.Archived {
			t.Fatalf("%s remained archived after group restore: %+v %v", id, effective, err)
		}
		if _, _, err := d.sessionAccessForLaunch(ctx, id); err != nil {
			t.Fatalf("restored %s ineligible for launch: %v", id, err)
		}
	}
	if _, err := d.authority.Current(ctx, access.SubjectSession, "done"); err == nil {
		t.Fatal("group restore regranted completed member")
	}

	done, _, err := db.GetSession("done")
	if err != nil || !done.Archived || done.ArchivedAt != 123 {
		t.Fatalf("completed member restored: %+v %v", done, err)
	}
}

func TestCoordinatorArchiveFailureAndRestartCleanup(t *testing.T) {
	for _, mode := range []string{"post-commit error", "unknown commit", "encode error", "lost response", "restart"} {
		t.Run(mode, func(t *testing.T) {
			d, runtime, principals, instances := archiveGroupFixture(t)
			ctx := context.Background()
			runtime.completions.minimum = time.Millisecond
			runtime.completions.runtime = 10 * time.Millisecond
			runtime.completions.poll = time.Millisecond
			runtime.completions.abandon = 5 * time.Millisecond
			want := sessionrpc.StatusOK
			switch mode {
			case "post-commit error", "unknown commit":
				want = sessionrpc.StatusIndeterminate
				runtime.applyResult = func(ctx context.Context, a core.Action) (string, error) {
					if _, err := wsops.ApplyResult(ctx, a); err != nil {
						return "", err
					}
					if mode == "unknown commit" {
						runtime.resolver.open = func() (policyStoreHandle, error) {
							return policyStoreHandle{}, errors.New("injected unavailable archive state")
						}
					}
					return "", errors.New("injected uncertain commit")
				}
			case "encode error":
				want = sessionrpc.StatusFailed
				runtime.encodeResult = func(core.Result) ([]byte, error) { return nil, errors.New("injected encode error") }
			}
			if mode == "restart" {
				// Crash after committing the group, before registering any receipt
				// owner. Reopen the authority and runtime from durable state only.
				if err := wsops.SetArchived(ctx, "root", true); err != nil {
					t.Fatal(err)
				}
				runtime.close()
				authorityDir := d.authority.Root()
				if err := d.authority.Close(); err != nil {
					t.Fatal(err)
				}
				reopened, err := access.Open(authorityDir)
				if err != nil {
					t.Fatal(err)
				}
				d.authority = reopened
				replacement := newSessionRuntime(d)
				d.sessionRPC = replacement
				runtime = replacement
				t.Cleanup(replacement.close)
			} else {
				result, err := runtime.dispatch(ctx, groupArchiveRequest(principals["root"]))
				if err != nil || result.Status != want {
					t.Fatalf("completion = %+v, %v; want %s", result, err, want)
				}
				// No hooks: cleanup must not depend on response delivery.
			}
			if err := runtime.reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(time.Second)
			for instances["root"].Alive() && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			assertGroupStopped(t, d, principals, instances)
		})
	}
}

func TestHostArchiveUsesStoredMembershipAndMemberDoneStaysLocal(t *testing.T) {
	for _, host := range []bool{true, false} {
		t.Run(map[bool]string{true: "host group", false: "ordinary member"}[host], func(t *testing.T) {
			d, runtime, principals, instances := archiveGroupFixture(t)
			// Stale rail omits the actual member and still claims a moved member.
			d.sessions = []core.Session{{ID: "root"}, {ID: "moved", RootID: "root"}}
			ctx := context.Background()
			if host {
				result := d.handle(ctx, core.Action{Action: core.ActionSetArchived, ID: "root", Fields: map[string]string{"archived": "true"}})
				if !result.OK {
					t.Fatalf("host archive: %+v", result)
				}
				if err := runtime.reconcile(ctx); err != nil {
					t.Fatal(err)
				}
				assertGroupStopped(t, d, principals, instances)
			} else {
				result, err := runtime.dispatch(ctx, groupArchiveRequest(principals["member"]))
				if err != nil || result.Status != sessionrpc.StatusOK {
					t.Fatalf("member done: %+v %v", result, err)
				}
				db, err := store.Open()
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				root, _, err := db.GetSession("root")
				if err != nil || root.Archived || !instances["root"].Alive() {
					t.Fatal("member done archived coordinator")
				}
				if err := d.authority.Valid(ctx, principals["root"]); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestWorkgroupRestorePartialMemberRegrantRemainsFencedAndRetryable(t *testing.T) {
	d, runtime, principals, _ := archiveGroupFixture(t)
	ctx := context.Background()
	if err := wsops.SetArchived(ctx, "root", true); err != nil {
		t.Fatal(err)
	}
	if err := runtime.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	// Model a root runtime that survived shutdown. Member regrant succeeds,
	// but failure to prove the root quiescent must keep the whole group fenced.
	eng := d.engine.(*fakeEngine)
	eng.running("root")
	eng.killRefuses = true
	action := core.Action{Action: core.ActionSetArchived, ID: "root", Fields: map[string]string{"archived": "false"}}
	if got := d.handle(ctx, action); got.OK {
		t.Fatal("restore succeeded without root quiescence")
	}
	current, err := d.authority.Current(ctx, access.SubjectSession, "member")
	if err != nil || current.Generation <= principals["member"].Generation {
		t.Fatalf("partial member publication not reached: %+v %v", current, err)
	}
	cred, err := access.LoadCredential(d.authority.CredentialDir(access.SubjectSession, "member"))
	if err != nil {
		t.Fatal(err)
	}
	call := sessionrpc.Call{Kind: sessionrpc.CallOperation, Route: access.RouteQuery, Verb: core.QuerySnapshot}
	if err := runtime.authorize(ctx, credentialPrincipal(cred), call); err == nil {
		t.Fatal("partial restore granted member authority before root restored")
	}
	if _, _, err := d.sessionAccessForLaunch(ctx, "member"); err == nil {
		t.Fatal("partial restore allowed member launch")
	}
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	root, _, err := db.GetSession("root")
	if err != nil || !root.Archived {
		t.Fatalf("failed restore lost durable parent archive: %+v %v", root, err)
	}
	member, _, err := db.GetSession("member")
	if err != nil || member.Archived {
		t.Fatalf("restore overwrote independent member state: %+v %v", member, err)
	}
	eng.killRefuses = false
	if err := runtime.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.authority.Valid(ctx, credentialPrincipal(cred)); err == nil {
		t.Fatal("reconcile retained partial successor")
	}
	if got := d.handle(ctx, action); !got.OK {
		t.Fatalf("explicit group restore retry failed: %+v", got)
	}
	for _, id := range []string{"root", "member"} {
		if _, _, err := d.sessionAccessForLaunch(ctx, id); err != nil {
			t.Fatalf("restored %s launch: %v", id, err)
		}
	}
	done, _, err := db.GetSession("done")
	if err != nil || !done.Archived || done.ArchivedAt != 123 {
		t.Fatalf("retry revived independently completed member: %+v %v", done, err)
	}
}

func TestHostMemberToggleUsesIndependentArchiveFlag(t *testing.T) {
	d, _, _, _ := archiveGroupFixture(t)
	ctx := context.Background()
	if err := wsops.SetArchived(ctx, "root", true); err != nil {
		t.Fatal(err)
	}
	// Parent archive affects runtime eligibility, not the member's own toggle.
	got := d.handle(ctx, core.Action{Action: core.ActionArchive, ID: "member"})
	if !got.OK {
		t.Fatalf("member archive under archived group: %+v", got)
	}
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	member, _, err := db.GetSession("member")
	if err != nil || !member.Archived {
		t.Fatalf("member toggle changed wrong state: %+v %v", member, err)
	}
	got = d.handle(ctx, core.Action{Action: core.ActionSetArchived, ID: "root", Fields: map[string]string{"archived": "false"}})
	if !got.OK {
		t.Fatalf("restore group: %+v", got)
	}
	member, _, err = db.GetSession("member")
	if err != nil || !member.Archived {
		t.Fatalf("group restore revived separately archived member: %+v %v", member, err)
	}
}
