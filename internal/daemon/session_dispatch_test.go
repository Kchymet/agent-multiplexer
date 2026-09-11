package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"amux/internal/access"
	"amux/internal/amuxcfg"
	"amux/internal/core"
	"amux/internal/sessionrpc"
	"amux/internal/store"
)

type completionRaceStore struct {
	*fakePolicyStore
	lookups atomic.Int32
}

func (s *completionRaceStore) GetSession(id string) (store.Session, bool, error) {
	session, ok, err := s.fakePolicyStore.GetSession(id)
	if s.lookups.Add(1) >= 3 {
		session.Archived = true
	}
	return session, ok, err
}

func sessionRuntimeFixture(t *testing.T, sessions ...store.Session) (*Daemon, *sessionRuntime, map[string]access.Principal) {
	t.Helper()
	isolateHome(t)
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range sessions {
		if err := db.PutSession(session); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	d := New("", nil, time.Hour)
	d.authority, err = access.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.authority.Close() })
	principals := make(map[string]access.Principal)
	for _, session := range sessions {
		if session.Archived {
			continue
		}
		if _, err := d.authority.EnsureSession(context.Background(), session.ID, session.Dir); err != nil {
			t.Fatal(err)
		}
		credential, err := access.LoadCredential(d.authority.CredentialDir(access.SubjectSession, session.ID))
		if err != nil {
			t.Fatal(err)
		}
		principals[session.ID] = credentialPrincipal(credential)
	}
	runtime := newSessionRuntime(d)
	return d, runtime, principals
}

func TestSessionDispatchRechecksCurrentMembershipAtExecution(t *testing.T) {
	root := store.Session{ID: "root1", Name: "root", Agent: "claude", Scope: store.ScopeWork, Dir: t.TempDir()}
	member := store.Session{ID: "a1", RootID: root.ID, Name: "agent", Agent: "claude", Dir: t.TempDir()}
	_, runtime, principals := sessionRuntimeFixture(t, root, member)
	call := sessionrpc.Call{Kind: sessionrpc.CallOperation, Route: access.RouteAction, Verb: core.ActionRename, ID: member.ID, Fields: map[string]string{"name": "renamed"}}
	if err := runtime.authorize(context.Background(), principals[root.ID], call); err != nil {
		t.Fatalf("initial authorize: %v", err)
	}
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetRootID(member.ID, "different-root"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	result, err := runtime.dispatch(context.Background(), sessionrpc.DispatchRequest{Principal: principals[root.ID], RequestID: "0123456789abcdef0123456789abcdef", Call: call})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != sessionrpc.StatusDenied {
		t.Fatalf("dispatch after membership change = %+v", result)
	}
}

func TestRestrictedDiagnosticsOmitHostPathsAndRawOverrides(t *testing.T) {
	session := store.Session{ID: "a1", RootID: "root1", Agent: "claude", Dir: t.TempDir()}
	d, runtime, principals := sessionRuntimeFixture(t, session)
	d.codexControl = amuxcfg.Control{
		ConfigPath: "/host/private/config.json", Persisted: amuxcfg.PTY,
		Override: "planted-secret", OverrideSet: true, Effective: amuxcfg.PTY,
		Source: "environment", Warning: "planted-secret from /host/private/config.json",
	}
	body, err := runtime.restrictedQuery(context.Background(), principals[session.ID], core.Action{Query: core.QueryCodexControl})
	if err != nil {
		t.Fatal(err)
	}
	var control amuxcfg.Control
	if err := json.Unmarshal(body, &control); err != nil {
		t.Fatal(err)
	}
	if control.ConfigPath != "" || control.Override != "" || control.Warning != "" {
		t.Fatalf("restricted control leaked host diagnostics: %+v", control)
	}
}

func TestSelfCompletionPersistsBeforeBoundedRuntimeStopAndRevoke(t *testing.T) {
	session := store.Session{
		ID: "a1", RootID: "root1", Name: "agent", Agent: "claude", Dir: t.TempDir(), ClaudeID: "completion-conversation",
	}
	d, runtime, principals := sessionRuntimeFixture(t, session)
	eng := newFakeEngine()
	d.engine = eng
	instance := eng.running(session.ID)
	if _, err := d.permissions.observe(session.ID, instance); err != nil {
		t.Fatal(err)
	}
	if err := core.WriteHookState(session.ClaudeID, core.StateReady, ""); err != nil {
		t.Fatal(err)
	}
	runtime.completions.minimum = 30 * time.Millisecond
	runtime.completions.runtime = 200 * time.Millisecond
	runtime.completions.poll = time.Millisecond
	runtime.completions.abandon = time.Second
	call := sessionrpc.Call{
		Kind: sessionrpc.CallOperation, Route: access.RouteAction, Verb: core.ActionSetArchived,
		ID: session.ID, Fields: map[string]string{"archived": "true"},
	}
	requestID := "0123456789abcdef0123456789abcdef"
	result, err := runtime.dispatch(context.Background(), sessionrpc.DispatchRequest{Principal: principals[session.ID], RequestID: requestID, Call: call})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != sessionrpc.StatusOK || result.Receipt == nil {
		t.Fatalf("completion result = %+v", result)
	}
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	archived, ok, err := db.GetSession(session.ID)
	_ = db.Close()
	if err != nil || !ok || !archived.Archived {
		t.Fatalf("completion was not committed before response: %+v, %v", archived, err)
	}
	if !instance.Alive() {
		t.Fatal("completion stopped runtime before durable response/receipt settlement")
	}
	result.Receipt.ResponsePersisted(sessionrpc.PersistedResponse{RequestID: requestID, ResponseDigest: "abcd"})
	if !runtime.completions.authorizeReceipt(principals[session.ID], sessionrpc.Receipt{RequestID: requestID, ResponseDigest: "abcd"}) {
		t.Fatal("matching persisted completion receipt was not narrowly authorized")
	}
	if runtime.completions.authorizeReceipt(principals[session.ID], sessionrpc.Receipt{RequestID: requestID, ResponseDigest: "different"}) {
		t.Fatal("mismatched completion receipt was authorized")
	}
	if err := runtime.authorize(context.Background(), principals[session.ID], sessionrpc.Call{Kind: sessionrpc.CallOperation, Route: access.RouteQuery, Verb: core.QuerySnapshot}); err == nil {
		t.Fatal("archived completion subject retained general query authority")
	}
	result.Receipt.Settled(sessionrpc.ReceiptSettlement{RequestID: requestID, Received: true})
	time.Sleep(10 * time.Millisecond)
	if !instance.Alive() {
		t.Fatal("receipt alone stopped runtime before minimum CLI-observation grace")
	}
	key := instance.Key()
	deadline := time.Now().Add(time.Second)
	_, live := eng.Lookup(key)
	for live && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		_, live = eng.Lookup(key)
	}
	if live {
		t.Fatal("settled completion did not stop idle runtime")
	}
	if err := d.authority.Valid(context.Background(), principals[session.ID]); err == nil {
		t.Fatal("completion credential remained valid after settled runtime stop")
	}
}

func TestSelfCompletionNeverFallsThroughToPreStopDispatchWhenStateChanges(t *testing.T) {
	session := store.Session{ID: "a1", RootID: "root1", Agent: "claude", Dir: t.TempDir()}
	_, runtime, principals := sessionRuntimeFixture(t, session)
	db := &completionRaceStore{fakePolicyStore: &fakePolicyStore{
		sessions: map[string]store.Session{session.ID: session},
		repos:    map[string]store.Repo{},
	}}
	runtime.resolver.open = func() (policyStoreHandle, error) {
		return policyStoreHandle{policyStore: db}, nil
	}
	call := sessionrpc.Call{
		Kind: sessionrpc.CallOperation, Route: access.RouteAction, Verb: core.ActionSetArchived,
		ID: session.ID, Fields: map[string]string{"archived": "true"},
	}
	result, err := runtime.dispatch(context.Background(), sessionrpc.DispatchRequest{
		Principal: principals[session.ID], RequestID: "0123456789abcdef0123456789abcdef", Call: call,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != sessionrpc.StatusDenied || result.Code != "completion_not_current" {
		t.Fatalf("changed self-completion = %+v", result)
	}
	storeDB, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	stored, ok, err := storeDB.GetSession(session.ID)
	_ = storeDB.Close()
	if err != nil || !ok || stored.Archived {
		t.Fatalf("denied completion reached ordinary archive dispatch: %+v ok=%v err=%v", stored, ok, err)
	}
}

func TestRevokedSessionCredentialIsNeverReissuedByLaunch(t *testing.T) {
	session := store.Session{ID: "a1", RootID: "root1", Agent: "claude", Dir: t.TempDir()}
	d, _, principals := sessionRuntimeFixture(t, session)
	credentialDir := d.authority.CredentialDir(access.SubjectSession, session.ID)
	before, err := access.LoadCredential(credentialDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.authority.Revoke(context.Background(), access.SubjectSession, session.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.sessionAccessForLaunch(context.Background(), session.ID); err == nil {
		t.Fatal("revoked subject was silently reissued at launch")
	}
	after, err := access.LoadCredential(credentialDir)
	if err != nil {
		t.Fatal(err)
	}
	if after.KeyID != before.KeyID || after.Generation != before.Generation {
		t.Fatalf("revoked credential changed from %s/%d to %s/%d", before.KeyID, before.Generation, after.KeyID, after.Generation)
	}
	if err := d.authority.Valid(context.Background(), principals[session.ID]); err == nil {
		t.Fatal("revoked principal became valid")
	}
}

func TestCompletionRuntimeGraceIsBoundedWhenAcknowledgementUnknown(t *testing.T) {
	session := store.Session{ID: "a1", RootID: "root1", Agent: "claude", Dir: t.TempDir(), ClaudeID: "unknown-completion"}
	d, runtime, principals := sessionRuntimeFixture(t, session)
	eng := newFakeEngine()
	d.engine = eng
	instance := eng.running(session.ID)
	if _, err := d.permissions.observe(session.ID, instance); err != nil {
		t.Fatal(err)
	}
	runtime.completions.minimum = time.Millisecond
	runtime.completions.runtime = 25 * time.Millisecond
	runtime.completions.poll = time.Millisecond
	hooks := runtime.completions.begin(principals[session.ID], "0123456789abcdef0123456789abcdef", session.ID)
	hooks.ResponsePersisted(sessionrpc.PersistedResponse{RequestID: "0123456789abcdef0123456789abcdef", ResponseDigest: "abcd"})
	hooks.Settled(sessionrpc.ReceiptSettlement{RequestID: "0123456789abcdef0123456789abcdef", Received: false})
	key := instance.Key()
	deadline := time.Now().Add(time.Second)
	_, live := eng.Lookup(key)
	for live && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		_, live = eng.Lookup(key)
	}
	if live {
		t.Fatal("unknown runtime acknowledgement exceeded bounded stop grace")
	}
	if _, ok := d.permissions.generation(session.ID); ok {
		t.Fatal("runtime generation survived completion stop")
	}
}

func TestSessionRuntimePublishesOnlyValidCurrentSubjectsAndRevokesArchive(t *testing.T) {
	isolateHome(t)
	dir := filepath.Join(core.SessionsDir(), "root1", "a1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	session := store.Session{ID: "a1", RootID: "root1", Agent: "claude", Dir: dir}
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PutSession(session); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	d := New("", nil, time.Hour)
	d.authority, err = access.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer d.authority.Close()
	runtime := newSessionRuntime(d)
	if err := runtime.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer runtime.close()
	if runtime.servers[session.ID] == nil || runtime.servers["console"] == nil {
		t.Fatalf("mailbox servers = %v", runtime.servers)
	}
	credential, err := access.LoadCredential(d.authority.CredentialDir(access.SubjectSession, session.ID))
	if err != nil {
		t.Fatal(err)
	}
	principal := credentialPrincipal(credential)
	db, err = store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetArchivedFlag(session.ID, true, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if err := runtime.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runtime.servers[session.ID] != nil {
		t.Fatal("archived subject retained mailbox server without a completion receipt")
	}
	if err := d.authority.Valid(context.Background(), principal); err == nil {
		t.Fatal("archived subject retained current credential")
	}
}

func TestCredentialRenewalIsProactiveButNeverRenewsArchivedSubject(t *testing.T) {
	session := store.Session{ID: "a1", RootID: "root1", Agent: "claude", Dir: t.TempDir()}
	_, runtime, _ := sessionRuntimeFixture(t, session)
	before, err := runtime.loadCredential(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	runtime.now = func() time.Time { return time.UnixMilli(before.NotAfter).Add(-credentialRenewBefore / 2) }
	if err := runtime.validateOrRenewCredential(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	after, err := runtime.loadCredential(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Generation != before.Generation+1 || after.KeyID == before.KeyID {
		t.Fatalf("credential was not proactively rotated: before=%+v after=%+v", before, after)
	}
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetArchivedFlag(session.ID, true, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	archived := session
	archived.Archived = true
	if err := runtime.validateOrRenewCredential(context.Background(), archived); err == nil {
		t.Fatal("archived subject was eligible for renewal")
	}
	final, err := runtime.loadCredential(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Generation != after.Generation || final.KeyID != after.KeyID {
		t.Fatal("archived subject credential rotated")
	}
}

func TestHostCredentialRenewsProactivelyButNotAfterRevocation(t *testing.T) {
	d := New("", nil, time.Hour)
	var err error
	d.authority, err = access.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer d.authority.Close()
	if err := d.ensureHostCredential(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	dir := d.authority.CredentialDir(access.SubjectHost, access.LocalHostSubject)
	before, err := access.LoadCredential(dir)
	if err != nil {
		t.Fatal(err)
	}
	nearExpiry := time.UnixMilli(before.NotAfter).Add(-credentialRenewBefore / 2)
	if err := d.ensureHostCredential(context.Background(), nearExpiry); err != nil {
		t.Fatal(err)
	}
	after, err := access.LoadCredential(dir)
	if err != nil {
		t.Fatal(err)
	}
	if after.Generation != before.Generation+1 {
		t.Fatalf("host generation = %d, want %d", after.Generation, before.Generation+1)
	}
	if err := d.authority.Revoke(context.Background(), access.SubjectHost, access.LocalHostSubject); err != nil {
		t.Fatal(err)
	}
	if err := d.ensureHostCredential(context.Background(), nearExpiry); err == nil {
		t.Fatal("revoked host credential was reissued")
	}
	final, err := access.LoadCredential(dir)
	if err != nil {
		t.Fatal(err)
	}
	if final.KeyID != after.KeyID || final.Generation != after.Generation {
		t.Fatal("revoked host credential changed")
	}
}
