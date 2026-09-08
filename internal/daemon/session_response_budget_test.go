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
