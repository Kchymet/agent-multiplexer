package daemon

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"amux/internal/access"
	"amux/internal/sessionrpc"
)

func reserveTestResponse(t *testing.T, budget *sessionResponseBudget, subject, request string, bytes int64, receipt, existing bool) sessionrpc.ResponseLease {
	t.Helper()
	lease, err := budget.ReserveResponse(context.Background(), sessionrpc.ResponseReservation{
		SubjectID: subject, RequestID: request, MaxBytes: bytes,
		ReceiptSlot: receipt, Existing: existing,
	})
	if err != nil {
		t.Fatalf("reserve %s/%s: %v", subject, request, err)
	}
	return lease
}

func TestSessionResponseBudgetIsSharedAcrossSubjects(t *testing.T) {
	budget := newSessionResponseBudgetWithLimits(2, 100, 2)
	first := reserveTestResponse(t, budget, "one", "request-one", 60, true, false)

	if _, err := budget.ReserveResponse(context.Background(), sessionrpc.ResponseReservation{
		SubjectID: "two", RequestID: "request-two", MaxBytes: 41,
	}); !errors.Is(err, errSessionResponseCapacity) {
		t.Fatalf("aggregate byte admission error = %v", err)
	}
	first.Commit(20, true)
	second := reserveTestResponse(t, budget, "two", "request-two", 70, true, false)
	if _, err := budget.ReserveResponse(context.Background(), sessionrpc.ResponseReservation{
		SubjectID: "three", RequestID: "request-three", MaxBytes: 1,
	}); !errors.Is(err, errSessionResponseCapacity) {
		t.Fatalf("aggregate file admission error = %v", err)
	}

	first.ReleaseReceipt()
	second.ReleaseReceipt()
	first.Release()
	second.Release()
	if budget.files != 0 || budget.bytes != 0 || budget.receipts != 0 {
		t.Fatalf("budget not fully released: files=%d bytes=%d receipts=%d", budget.files, budget.bytes, budget.receipts)
	}
}

func TestSessionResponseBudgetReceiptAndExistingLeaseLifecycle(t *testing.T) {
	budget := newSessionResponseBudgetWithLimits(3, 100, 1)
	lease := reserveTestResponse(t, budget, "one", "request-one", 50, true, false)
	if _, err := budget.ReserveResponse(context.Background(), sessionrpc.ResponseReservation{
		SubjectID: "two", RequestID: "request-two", MaxBytes: 10, ReceiptSlot: true,
	}); !errors.Is(err, errSessionResponseCapacity) {
		t.Fatalf("aggregate receipt admission error = %v", err)
	}
	if _, err := budget.ReserveResponse(context.Background(), sessionrpc.ResponseReservation{
		SubjectID: "one", RequestID: "request-one", MaxBytes: 50,
	}); !errors.Is(err, errSessionResponseCapacity) {
		t.Fatalf("duplicate new reservation error = %v", err)
	}

	lease.Commit(25, true)
	reopened := reserveTestResponse(t, budget, "one", "request-one", 25, false, true)
	if reopened != lease {
		t.Fatal("existing response did not adopt the retained lease")
	}
	reopened.Commit(25, false)
	reopened.ReleaseReceipt()
	reopened.ReleaseReceipt()
	reopened.Release()
	reopened.Release()
	if budget.files != 0 || budget.bytes != 0 || budget.receipts != 0 {
		t.Fatalf("idempotent lifecycle leaked capacity: files=%d bytes=%d receipts=%d", budget.files, budget.bytes, budget.receipts)
	}
}

func TestSessionResponseBudgetAdoptsUnknownExistingOverCurrentLimits(t *testing.T) {
	budget := newSessionResponseBudgetWithLimits(1, 10, 0)
	first := reserveTestResponse(t, budget, "one", "request-one", 20, true, true)
	second := reserveTestResponse(t, budget, "two", "request-two", 30, false, true)
	first.Commit(20, true)
	second.Commit(30, false)
	if budget.files != 2 || budget.bytes != 50 || budget.receipts != 1 {
		t.Fatalf("retained overage not accounted: files=%d bytes=%d receipts=%d", budget.files, budget.bytes, budget.receipts)
	}
	if _, err := budget.ReserveResponse(context.Background(), sessionrpc.ResponseReservation{
		SubjectID: "three", RequestID: "request-three", MaxBytes: 1,
	}); !errors.Is(err, errSessionResponseCapacity) {
		t.Fatalf("new admission while retained set is over limit = %v", err)
	}
	first.Release()
	second.Release()
	newLease := reserveTestResponse(t, budget, "three", "request-three", 10, false, false)
	newLease.Release()
}

func TestSessionResponseBudgetRejectsCancelledOrInvalidReservation(t *testing.T) {
	budget := newSessionResponseBudgetWithLimits(1, 1, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := budget.ReserveResponse(ctx, sessionrpc.ResponseReservation{
		SubjectID: "one", RequestID: "request-one", MaxBytes: 1,
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled reservation error = %v", err)
	}
	if _, err := budget.ReserveResponse(context.Background(), sessionrpc.ResponseReservation{}); !errors.Is(err, errSessionResponseCapacity) {
		t.Fatalf("invalid reservation error = %v", err)
	}
}

func TestSessionResponseBudgetAdmitsAcrossRealSubjectServers(t *testing.T) {
	authority, err := access.Open(filepath.Join(t.TempDir(), "authority"))
	if err != nil {
		t.Fatal(err)
	}
	defer authority.Close()
	budget := newSessionResponseBudgetWithLimits(1, 32<<20, 1)
	type subjectServer struct {
		grant    access.SessionAccess
		server   *sessionrpc.Server
		dispatch int
	}
	open := func(id string) *subjectServer {
		s := &subjectServer{}
		sessionDir := filepath.Join(t.TempDir(), id)
		if err := os.MkdirAll(sessionDir, 0o700); err != nil {
			t.Fatal(err)
		}
		grant, err := authority.EnsureSession(context.Background(), id, sessionDir)
		if err != nil {
			t.Fatal(err)
		}
		s.grant = grant
		s.server, err = sessionrpc.OpenServerMailbox(id, grant.MailboxHostDir, authority, authority,
			sessionrpc.Callbacks{
				Authorize: func(context.Context, access.Principal, sessionrpc.Call) error { return nil },
				Dispatch: func(context.Context, sessionrpc.DispatchRequest) (sessionrpc.DispatchResult, error) {
					s.dispatch++
					return sessionrpc.DispatchResult{Status: sessionrpc.StatusOK, Body: []byte(`{"ok":true}`)}, nil
				},
			}, sessionrpc.ServerOptions{ResponseBudget: budget})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.server.PublishService(context.Background()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.server.Close() })
		return s
	}
	first := open("first")
	second := open("second")
	publishDaemonBudgetCall(t, authority, first.grant)
	if processed, err := first.server.ServeOnce(context.Background()); err != nil || processed != 1 {
		t.Fatalf("first ServeOnce = %v, %v", processed, err)
	}
	publishDaemonBudgetCall(t, authority, second.grant)
	if _, err := second.server.ServeOnce(context.Background()); !errors.Is(err, sessionrpc.ErrResponseCapacity) {
		t.Fatalf("second aggregate admission error = %v", err)
	}
	if first.dispatch != 1 || second.dispatch != 0 {
		t.Fatalf("dispatch counts after aggregate capacity = %d, %d", first.dispatch, second.dispatch)
	}
}

func TestSessionResponseBudgetRestartScrubsTemporaryBeforeAdoption(t *testing.T) {
	authority, err := access.Open(filepath.Join(t.TempDir(), "authority"))
	if err != nil {
		t.Fatal(err)
	}
	defer authority.Close()
	sessionDir := filepath.Join(t.TempDir(), "session")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	grant, err := authority.EnsureSession(context.Background(), "subject", sessionDir)
	if err != nil {
		t.Fatal(err)
	}
	temporary := filepath.Join(grant.MailboxHostDir, sessionrpc.ResponsesDirName, ".tmp-"+strings.Repeat("a", 32))
	if err := os.WriteFile(temporary, []byte("crash-left"), 0o600); err != nil {
		t.Fatal(err)
	}
	budget := newSessionResponseBudgetWithLimits(1, 32<<20, 1)
	server, err := sessionrpc.OpenServerMailbox("subject", grant.MailboxHostDir, authority, authority,
		sessionrpc.Callbacks{
			Authorize: func(context.Context, access.Principal, sessionrpc.Call) error { return nil },
			Dispatch: func(context.Context, sessionrpc.DispatchRequest) (sessionrpc.DispatchResult, error) {
				return sessionrpc.DispatchResult{Status: sessionrpc.StatusOK}, nil
			},
		}, sessionrpc.ServerOptions{ResponseBudget: budget})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if _, err := server.ServeOnce(context.Background()); err != nil {
		t.Fatalf("restart scrub/adoption: %v", err)
	}
	if _, err := os.Stat(temporary); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("crash-left response temporary survived: %v", err)
	}
	if budget.files != 0 || budget.bytes != 0 || budget.receipts != 0 {
		t.Fatalf("temporary was adopted into budget: files=%d bytes=%d receipts=%d", budget.files, budget.bytes, budget.receipts)
	}
}

func TestSessionResponseBudgetRestartAdoptsRetainedMultiSubjectOverLimit(t *testing.T) {
	authority, err := access.Open(filepath.Join(t.TempDir(), "authority"))
	if err != nil {
		t.Fatal(err)
	}
	defer authority.Close()
	type retainedSubject struct {
		grant access.SessionAccess
	}
	createResponse := func(id string) retainedSubject {
		sessionDir := filepath.Join(t.TempDir(), id)
		if err := os.MkdirAll(sessionDir, 0o700); err != nil {
			t.Fatal(err)
		}
		grant, err := authority.EnsureSession(context.Background(), id, sessionDir)
		if err != nil {
			t.Fatal(err)
		}
		server, err := sessionrpc.OpenServerMailbox(id, grant.MailboxHostDir, authority, authority,
			sessionrpc.Callbacks{
				Authorize: func(context.Context, access.Principal, sessionrpc.Call) error { return nil },
				Dispatch: func(context.Context, sessionrpc.DispatchRequest) (sessionrpc.DispatchResult, error) {
					return sessionrpc.DispatchResult{Status: sessionrpc.StatusOK, Body: []byte(`{"retained":true}`)}, nil
				},
			}, sessionrpc.ServerOptions{})
		if err != nil {
			t.Fatal(err)
		}
		publishDaemonBudgetCall(t, authority, grant)
		if processed, err := server.ServeOnce(context.Background()); err != nil || processed != 1 {
			t.Fatalf("create retained response for %s = %d, %v", id, processed, err)
		}
		if err := server.Close(); err != nil {
			t.Fatal(err)
		}
		return retainedSubject{grant: grant}
	}
	first := createResponse("first")
	second := createResponse("second")

	// Simulate a daemon restart with stricter limits than the two responses
	// already retained across independently served subject mailboxes.
	budget := newSessionResponseBudgetWithLimits(1, 1, 1)
	reopen := func(id string, grant access.SessionAccess, dispatch *int) *sessionrpc.Server {
		server, err := sessionrpc.OpenServerMailbox(id, grant.MailboxHostDir, authority, authority,
			sessionrpc.Callbacks{
				Authorize: func(context.Context, access.Principal, sessionrpc.Call) error { return nil },
				Dispatch: func(context.Context, sessionrpc.DispatchRequest) (sessionrpc.DispatchResult, error) {
					*dispatch++
					return sessionrpc.DispatchResult{Status: sessionrpc.StatusOK}, nil
				},
			}, sessionrpc.ServerOptions{ResponseBudget: budget})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = server.Close() })
		return server
	}
	var firstDispatch, secondDispatch int
	firstServer := reopen("first", first.grant, &firstDispatch)
	secondServer := reopen("second", second.grant, &secondDispatch)
	if _, err := firstServer.ServeOnce(context.Background()); err != nil {
		t.Fatalf("adopt first retained response: %v", err)
	}
	if _, err := secondServer.ServeOnce(context.Background()); err != nil {
		t.Fatalf("adopt second retained response over aggregate limit: %v", err)
	}
	if firstDispatch != 0 || secondDispatch != 0 {
		t.Fatalf("restart adoption dispatched work: %d, %d", firstDispatch, secondDispatch)
	}
	if budget.files != 2 || budget.bytes <= budget.maxBytes {
		t.Fatalf("retained multi-subject overage not accounted: files=%d bytes=%d", budget.files, budget.bytes)
	}

	thirdDir := filepath.Join(t.TempDir(), "third")
	if err := os.MkdirAll(thirdDir, 0o700); err != nil {
		t.Fatal(err)
	}
	thirdGrant, err := authority.EnsureSession(context.Background(), "third", thirdDir)
	if err != nil {
		t.Fatal(err)
	}
	thirdDispatch := 0
	thirdServer := reopen("third", thirdGrant, &thirdDispatch)
	publishDaemonBudgetCall(t, authority, thirdGrant)
	if _, err := thirdServer.ServeOnce(context.Background()); !errors.Is(err, sessionrpc.ErrResponseCapacity) {
		t.Fatalf("new response admitted while retained set exceeds limit: %v", err)
	}
	if thirdDispatch != 0 {
		t.Fatalf("new over-limit request dispatched %d times", thirdDispatch)
	}
}

func TestSessionRuntimeRearmsGlobalInitializationBeforeFreshDispatch(t *testing.T) {
	authority, err := access.Open(filepath.Join(t.TempDir(), "authority"))
	if err != nil {
		t.Fatal(err)
	}
	defer authority.Close()
	makeGrant := func(id string) access.SessionAccess {
		dir := filepath.Join(t.TempDir(), id)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		grant, err := authority.EnsureSession(context.Background(), id, dir)
		if err != nil {
			t.Fatal(err)
		}
		return grant
	}
	open := func(id string, grant access.SessionAccess, budget sessionrpc.ResponseBudget, dispatch *int) *sessionrpc.Server {
		server, err := sessionrpc.OpenServerMailbox(id, grant.MailboxHostDir, authority, authority,
			sessionrpc.Callbacks{
				Authorize: func(context.Context, access.Principal, sessionrpc.Call) error { return nil },
				Dispatch: func(context.Context, sessionrpc.DispatchRequest) (sessionrpc.DispatchResult, error) {
					*dispatch++
					return sessionrpc.DispatchResult{Status: sessionrpc.StatusOK}, nil
				},
			}, sessionrpc.ServerOptions{ResponseBudget: budget})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = server.Close() })
		return server
	}

	budget := newSessionResponseBudgetWithLimits(1, 32<<20, 1)
	freshGrant := makeGrant("a-fresh")
	freshDispatch := 0
	fresh := open("a-fresh", freshGrant, budget, &freshDispatch)
	runtime := &sessionRuntime{
		servers:     map[string]*sessionrpc.Server{"a-fresh": fresh},
		initialized: map[string]bool{},
	}
	if err := runtime.serveCurrent(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !runtime.initialized["a-fresh"] {
		t.Fatal("initial mailbox did not finish initialization")
	}

	// Produce a real signed retained response without the restarted daemon's
	// budget, then reopen that mailbox with the shared budget.
	retainedGrant := makeGrant("z-retained")
	retainedDispatch := 0
	old := open("z-retained", retainedGrant, nil, &retainedDispatch)
	publishDaemonBudgetCall(t, authority, retainedGrant)
	if processed, err := old.ServeOnce(context.Background()); err != nil || processed != 1 {
		t.Fatalf("produce retained response = %d, %v", processed, err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	retainedDispatch = 0
	retained := open("z-retained", retainedGrant, budget, &retainedDispatch)
	runtime.servers["z-retained"] = retained // later discovery re-arms the global gate
	runtime.initialized["z-retained"] = false

	publishDaemonBudgetCall(t, authority, freshGrant)
	if err := runtime.serveCurrent(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !runtime.initialized["z-retained"] {
		t.Fatal("later mailbox was not initialized")
	}
	if budget.files != 1 {
		t.Fatalf("retained response was not adopted before serving fresh requests: files=%d", budget.files)
	}
	if freshDispatch != 0 {
		t.Fatalf("fresh request dispatched before retained response admission: %d", freshDispatch)
	}
	if retainedDispatch != 0 {
		t.Fatalf("retained response restart dispatched work: %d", retainedDispatch)
	}
}

func publishDaemonBudgetCall(t *testing.T, authority *access.FileAuthority, grant access.SessionAccess) {
	t.Helper()
	credential, err := access.LoadCredential(grant.CredentialHostDir)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(sessionrpc.Call{
		Kind: sessionrpc.CallOperation, Route: access.RouteQuery, Verb: "snapshot",
	})
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := access.SignRequest(credential, authority.BootID(), body, time.Now(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(grant.RequestsHostDir, envelope.RequestID+".req")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}
