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
		if err != nil || !ok || !s.Archived {
			t.Fatalf("response preceded durable %s archive: %+v %v", id, s, err)
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
	// Host-only restoration regrants the selected session, never its children.
	restore := func(id string) core.Result {
		return d.handle(ctx, core.Action{Action: core.ActionSetArchived, ID: id, Fields: map[string]string{"archived": "false"}})
	}
	if got := restore("member"); got.OK {
		t.Fatal("host restored member before its group")
	}
	if got := restore("root"); !got.OK {
		t.Fatalf("host root restore: %+v", got)
	}
	current, err := d.authority.Current(ctx, access.SubjectSession, "root")
	if err != nil || current.Generation <= principals["root"].Generation {
		t.Fatalf("root regrant: %+v %v", current, err)
	}
	for _, id := range []string{"member", "done"} {
		s, _, err := db.GetSession(id)
		if err != nil || !s.Archived {
			t.Fatalf("root restore revived %s: %+v %v", id, s, err)
		}
		if _, err := d.authority.Current(ctx, access.SubjectSession, id); err == nil {
			t.Fatalf("root restore regranted %s", id)
		}
	}
	if got := restore("member"); !got.OK {
		t.Fatalf("explicit member restore: %+v", got)
	}
	if _, err := d.authority.Current(ctx, access.SubjectSession, "member"); err != nil {
		t.Fatalf("member regrant: %v", err)
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
