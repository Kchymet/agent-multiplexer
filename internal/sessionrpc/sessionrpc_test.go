package sessionrpc

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"amux/internal/access"
	"golang.org/x/sys/unix"
)

type fixture struct {
	t       *testing.T
	root    string
	authDir string
	auth    *access.FileAuthority
	grant   access.SessionAccess
	server  *Server
}

func newFixture(t *testing.T, callbacks Callbacks, options ServerOptions) *fixture {
	t.Helper()
	root := t.TempDir()
	authDir := filepath.Join(root, "authority")
	authority, err := access.Open(authDir)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := authority.EnsureSession(context.Background(), "subject-a", filepath.Join(root, "namespace"))
	if err != nil {
		authority.Close()
		t.Fatal(err)
	}
	writeTestContext(t, grant.CredentialHostDir, grant.SubjectID, grant.MailboxHostDir)
	if callbacks.Authorize == nil {
		callbacks.Authorize = func(context.Context, access.Principal, Call) error { return nil }
	}
	if callbacks.Dispatch == nil {
		callbacks.Dispatch = func(_ context.Context, request DispatchRequest) (DispatchResult, error) {
			return DispatchResult{Status: StatusOK, Body: []byte("ok:" + request.Call.Fields["value"])}, nil
		}
	}
	server, err := OpenServerMailbox(grant.SubjectID, grant.MailboxHostDir, authority, authority, callbacks, options)
	if err != nil {
		authority.Close()
		t.Fatal(err)
	}
	if err := server.PublishService(context.Background()); err != nil {
		server.Close()
		authority.Close()
		t.Fatal(err)
	}
	f := &fixture{t: t, root: root, authDir: authDir, auth: authority, grant: grant, server: server}
	t.Cleanup(func() {
		if f.server != nil {
			_ = f.server.Close()
		}
		if f.auth != nil {
			_ = f.auth.Close()
		}
	})
	return f
}

func writeTestContext(t *testing.T, credentialDir, subjectID, mailboxDir string) {
	t.Helper()
	contextBytes, err := json.Marshal(SessionContext{Protocol: ProtocolVersion, SubjectID: subjectID, MailboxDir: mailboxDir})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(credentialDir, ContextFileName)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, contextBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) client(options clientOptions) *Client {
	f.t.Helper()
	client, err := openClientAt(f.grant.CredentialHostDir, options)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = client.Close() })
	return client
}

func pumpClient(t *testing.T, server *Server, operation func(context.Context) (Result, error)) (Result, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	type outcome struct {
		result Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := operation(ctx)
		done <- outcome{result: result, err: err}
	}()
	for {
		_, err := server.ServeOnce(ctx)
		if err != nil && !errors.Is(err, ErrQueueFull) {
			t.Fatal(err)
		}
		select {
		case got := <-done:
			return got.result, got.err
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
}

func TestClientServerOneShotAndExactBindings(t *testing.T) {
	var dispatched atomic.Int32
	f := newFixture(t, Callbacks{
		Authorize: func(_ context.Context, principal access.Principal, call Call) error {
			if principal.SubjectID != "subject-a" || call.AccessRequest().Route != access.RouteQuery {
				return errors.New("denied")
			}
			return nil
		},
		Dispatch: func(_ context.Context, request DispatchRequest) (DispatchResult, error) {
			dispatched.Add(1)
			return DispatchResult{Status: StatusOK, Body: []byte("reply:" + request.Call.Fields["value"])}, nil
		},
	}, ServerOptions{})
	client := f.client(clientOptions{})
	result, err := pumpClient(t, f.server, func(ctx context.Context) (Result, error) {
		return client.Query(ctx, Query{Verb: "snapshot", Fields: map[string]string{"value": "exact"}})
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(result.Body); got != "reply:exact" || result.Status != StatusOK || dispatched.Load() != 1 {
		t.Fatalf("result = %+v, dispatched=%d", result, dispatched.Load())
	}
}

func TestAuthorizeCannotRewriteDispatchedCanonicalOperation(t *testing.T) {
	f := newFixture(t, Callbacks{
		Authorize: func(_ context.Context, _ access.Principal, call Call) error {
			call.ID = "rewritten"
			call.Fields["scope"] = "rewritten"
			return nil
		},
		Dispatch: func(_ context.Context, request DispatchRequest) (DispatchResult, error) {
			if request.Call.ID != "original" || request.Call.Fields["scope"] != "original" {
				return DispatchResult{}, fmt.Errorf("dispatch changed to %+v", request.Call)
			}
			return DispatchResult{Status: StatusOK}, nil
		},
	}, ServerOptions{})
	client := f.client(clientOptions{})
	if _, err := pumpClient(t, f.server, func(ctx context.Context) (Result, error) {
		return client.Query(ctx, Query{Verb: "snapshot", ID: "original", Fields: map[string]string{"scope": "original"}})
	}); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentOneShotCalls(t *testing.T) {
	f := newFixture(t, Callbacks{
		Dispatch: func(_ context.Context, request DispatchRequest) (DispatchResult, error) {
			return DispatchResult{Status: StatusOK, Body: []byte(request.Call.Fields["index"])}, nil
		},
	}, ServerOptions{})
	client := f.client(clientOptions{poll: time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const calls = 24
	type outcome struct {
		index int
		value string
		err   error
	}
	done := make(chan outcome, calls)
	for index := 0; index < calls; index++ {
		go func(index int) {
			result, err := client.Query(ctx, Query{Verb: "snapshot", Fields: map[string]string{"index": fmt.Sprint(index)}})
			done <- outcome{index: index, value: string(result.Body), err: err}
		}(index)
	}
	for remaining := calls; remaining > 0; {
		if _, err := f.server.ServeOnce(ctx); err != nil && !errors.Is(err, ErrQueueFull) {
			t.Fatal(err)
		}
		for {
			select {
			case got := <-done:
				if got.err != nil || got.value != fmt.Sprint(got.index) {
					t.Fatalf("call %d value=%q err=%v", got.index, got.value, got.err)
				}
				remaining--
			default:
				goto next
			}
		}
	next:
		if remaining > 0 {
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(time.Millisecond):
			}
		}
	}
}

func TestServerScanPreservesSyncedInFlightAtomicPublication(t *testing.T) {
	f := newFixture(t, Callbacks{}, ServerOptions{})
	readyToPublish := make(chan struct{})
	resume := make(chan struct{})
	client := f.client(clientOptions{
		poll: time.Millisecond,
		beforeRequestPublish: func() {
			close(readyToPublish)
			<-resume
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	type outcome struct {
		result Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := client.Query(ctx, Query{Verb: "snapshot", Fields: map[string]string{"value": "race"}})
		done <- outcome{result: result, err: err}
	}()
	select {
	case <-readyToPublish:
	case <-ctx.Done():
		t.Fatal("client did not sync publication temporary")
	}
	processed, err := f.server.ServeOnce(ctx)
	if err != nil || processed != 1 {
		t.Fatalf("scan with in-flight temporary processed=%d err=%v", processed, err)
	}
	entries, err := os.ReadDir(f.grant.RequestsHostDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !requestTemporaryFromFile(entries[0].Name()) {
		t.Fatalf("scanner removed or altered in-flight temporary: %v", entries)
	}
	info, err := entries[0].Info()
	if err != nil || info.Size() == 0 {
		t.Fatalf("publication temporary was not fully written before scan: info=%v err=%v", info, err)
	}
	close(resume)
	for {
		if _, err := f.server.ServeOnce(ctx); err != nil && !errors.Is(err, ErrQueueFull) {
			t.Fatal(err)
		}
		select {
		case got := <-done:
			if got.err != nil || string(got.result.Body) != "ok:race" {
				t.Fatalf("call after scan result=%+v err=%v", got.result, got.err)
			}
			return
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
}

func TestServerRevalidatesCredentialAfterAuthorize(t *testing.T) {
	var authority *access.FileAuthority
	var dispatched atomic.Int32
	f := newFixture(t, Callbacks{
		Authorize: func(_ context.Context, principal access.Principal, _ Call) error {
			return authority.Revoke(context.Background(), principal.Kind, principal.SubjectID)
		},
		Dispatch: countDispatch(&dispatched),
	}, ServerOptions{})
	authority = f.auth
	envelope, encoded := signedCall(t, f.auth, f.grant.CredentialHostDir, Call{
		Kind: CallOperation, Route: access.RouteAction, Verb: "mutate", ID: f.grant.SubjectID,
	})
	publishEnvelope(t, f.grant.RequestsHostDir, envelope, encoded)
	if _, err := f.server.ServeOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if dispatched.Load() != 0 {
		t.Fatalf("revoked principal dispatched %d times", dispatched.Load())
	}
	response := readTestResponse(t, f.grant.MailboxHostDir, envelope.RequestID)
	if response.Status != StatusDenied || response.Code != "credential_invalid" {
		t.Fatalf("response after authorize-time revoke = %+v", response)
	}
}

type failingIssuerSigner struct {
	IssuerSigner
	err error
}

func (s failingIssuerSigner) SignIssuer([]byte) ([]byte, error) { return nil, s.err }

type testResponseBudget struct {
	mu       sync.Mutex
	limit    int
	active   int
	commits  int
	releases int
	receipts int
	leases   map[string]*testResponseLease
}

func (b *testResponseBudget) ReserveResponse(_ context.Context, request ResponseReservation) (ResponseLease, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.leases == nil {
		b.leases = make(map[string]*testResponseLease)
	}
	key := request.SubjectID + "/" + request.RequestID
	if existing := b.leases[key]; existing != nil {
		if request.Existing {
			return existing, nil
		}
		return nil, ErrResponseCapacity
	}
	// Existing reservations adopt durable disk reality even when it already
	// exceeds today's admission limit. Only new amplification is rejected.
	if b.active >= b.limit && !request.Existing {
		return nil, ErrResponseCapacity
	}
	b.active++
	if request.ReceiptSlot {
		b.receipts++
	}
	lease := &testResponseLease{budget: b, key: key, receipt: request.ReceiptSlot}
	b.leases[key] = lease
	return lease, nil
}

func (b *testResponseBudget) counts() (active, commits, releases int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.active, b.commits, b.releases
}

func (b *testResponseBudget) receiptCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.receipts
}

type testResponseLease struct {
	mu        sync.Mutex
	budget    *testResponseBudget
	key       string
	committed bool
	released  bool
	receipt   bool
}

func (l *testResponseLease) Commit(_ int64, receiptPending bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return
	}
	l.budget.mu.Lock()
	if !l.committed {
		l.committed = true
		l.budget.commits++
	}
	if l.receipt && !receiptPending {
		l.receipt = false
		l.budget.receipts--
	}
	l.budget.mu.Unlock()
}

func (l *testResponseLease) ReleaseReceipt() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released || !l.receipt {
		return
	}
	l.receipt = false
	l.budget.mu.Lock()
	l.budget.receipts--
	l.budget.mu.Unlock()
}

func (l *testResponseLease) Release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return
	}
	l.released = true
	l.budget.mu.Lock()
	if l.receipt {
		l.budget.receipts--
		l.receipt = false
	}
	l.budget.active--
	l.budget.releases++
	delete(l.budget.leases, l.key)
	l.budget.mu.Unlock()
}

func TestPostCommitResponseFailureSettlesExactlyOnceWithoutPersisted(t *testing.T) {
	f := newFixture(t, Callbacks{}, ServerOptions{})
	if err := f.server.Close(); err != nil {
		t.Fatal(err)
	}
	f.server = nil
	var persisted atomic.Int32
	settled := make(chan ReceiptSettlement, 2)
	server, err := OpenServerMailbox(f.grant.SubjectID, f.grant.MailboxHostDir, f.auth,
		failingIssuerSigner{IssuerSigner: f.auth, err: errors.New("injected signing failure")}, Callbacks{
			Authorize: func(context.Context, access.Principal, Call) error { return nil },
			Dispatch: func(context.Context, DispatchRequest) (DispatchResult, error) {
				return DispatchResult{Status: StatusOK, Receipt: &ReceiptHooks{
					ResponsePersisted: func(PersistedResponse) { persisted.Add(1) },
					Settled:           func(value ReceiptSettlement) { settled <- value },
				}}, nil
			},
		}, ServerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	f.server = server
	envelope, encoded := signedCall(t, f.auth, f.grant.CredentialHostDir, Call{
		Kind: CallOperation, Route: access.RouteAction, Verb: "done", ID: f.grant.SubjectID,
	})
	publishEnvelope(t, f.grant.RequestsHostDir, envelope, encoded)
	if _, err := server.ServeOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "injected signing failure") {
		t.Fatalf("response publication error = %v", err)
	}
	if persisted.Load() != 0 {
		t.Fatalf("failed response reported persisted %d times", persisted.Load())
	}
	select {
	case value := <-settled:
		if value.RequestID != envelope.RequestID || value.Received || value.Reason != SettlementResponseFailed {
			t.Fatalf("failure settlement = %+v", value)
		}
	default:
		t.Fatal("post-commit response failure did not settle")
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case duplicate := <-settled:
		t.Fatalf("response failure settled twice: %+v", duplicate)
	default:
	}
}

func TestPostCommitUncertainResponseDurabilitySettlesWithoutPersisted(t *testing.T) {
	var persisted atomic.Int32
	settled := make(chan ReceiptSettlement, 2)
	f := newFixture(t, Callbacks{
		Dispatch: func(context.Context, DispatchRequest) (DispatchResult, error) {
			return DispatchResult{Status: StatusOK, Receipt: &ReceiptHooks{
				ResponsePersisted: func(PersistedResponse) { persisted.Add(1) },
				Settled:           func(value ReceiptSettlement) { settled <- value },
			}}, nil
		},
	}, ServerOptions{})
	injected := errors.New("injected directory fsync uncertainty")
	f.server.responseWriter = func(dir *os.File, name string, data []byte, random io.Reader) error {
		if err := atomicWriteAt(dir, name, data, random); err != nil {
			return err
		}
		return injected
	}
	envelope, encoded := signedCall(t, f.auth, f.grant.CredentialHostDir, Call{
		Kind: CallOperation, Route: access.RouteAction, Verb: "done", ID: f.grant.SubjectID,
	})
	publishEnvelope(t, f.grant.RequestsHostDir, envelope, encoded)
	if _, err := f.server.ServeOnce(context.Background()); !errors.Is(err, injected) {
		t.Fatalf("uncertain response publication error = %v", err)
	}
	if persisted.Load() != 0 {
		t.Fatalf("uncertain response reported persisted %d times", persisted.Load())
	}
	if _, err := os.Stat(filepath.Join(f.grant.MailboxHostDir, ResponsesDirName, responseFileName(envelope.RequestID))); err != nil {
		t.Fatalf("injected post-rename response missing: %v", err)
	}
	select {
	case value := <-settled:
		if value.Received || value.Reason != SettlementResponseFailed {
			t.Fatalf("uncertain durability settlement = %+v", value)
		}
	default:
		t.Fatal("uncertain response durability did not settle")
	}
	if err := f.server.Close(); err != nil {
		t.Fatal(err)
	}
	f.server = nil
	select {
	case duplicate := <-settled:
		t.Fatalf("uncertain durability settled twice: %+v", duplicate)
	default:
	}
}

func TestAggregateResponseBudgetAdmitsBeforeDispatchAndReleasesOnRemoval(t *testing.T) {
	budget := &testResponseBudget{limit: 1}
	clock := newTestClock(time.Now())
	var firstDispatch atomic.Int32
	firstCallbacks := Callbacks{
		Authorize: func(context.Context, access.Principal, Call) error { return nil },
		Dispatch:  countDispatch(&firstDispatch),
	}
	first := newFixture(t, firstCallbacks, ServerOptions{Clock: clock.Now, ResponseBudget: budget})
	firstEnvelope, firstBytes := signedCall(t, first.auth, first.grant.CredentialHostDir, Call{
		Kind: CallOperation, Route: access.RouteQuery, Verb: "snapshot",
	})
	publishEnvelope(t, first.grant.RequestsHostDir, firstEnvelope, firstBytes)
	if _, err := first.server.ServeOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if active, commits, releases := budget.counts(); active != 1 || commits != 1 || releases != 0 {
		t.Fatalf("budget after durable response active=%d commits=%d releases=%d", active, commits, releases)
	}

	var secondDispatch atomic.Int32
	second := newFixture(t, Callbacks{Dispatch: countDispatch(&secondDispatch)}, ServerOptions{ResponseBudget: budget})
	secondEnvelope, secondBytes := signedCall(t, second.auth, second.grant.CredentialHostDir, Call{
		Kind: CallOperation, Route: access.RouteAction, Verb: "mutate", ID: second.grant.SubjectID,
	})
	publishEnvelope(t, second.grant.RequestsHostDir, secondEnvelope, secondBytes)
	if _, err := second.server.ServeOnce(context.Background()); !errors.Is(err, ErrResponseCapacity) {
		t.Fatalf("aggregate admission error = %v", err)
	}
	if secondDispatch.Load() != 0 {
		t.Fatalf("aggregate budget rejected after dispatch: %d", secondDispatch.Load())
	}
	if err := first.server.Close(); err != nil {
		t.Fatal(err)
	}
	first.server = nil
	if active, commits, releases := budget.counts(); active != 1 || commits != 1 || releases != 0 {
		t.Fatalf("close released durable response active=%d commits=%d releases=%d", active, commits, releases)
	}
	reopened, err := OpenServerMailbox(first.grant.SubjectID, first.grant.MailboxHostDir, first.auth, first.auth,
		firstCallbacks, ServerOptions{Clock: clock.Now, ResponseBudget: budget})
	if err != nil {
		t.Fatal(err)
	}
	first.server = reopened
	if _, err := reopened.ServeOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if active, commits, releases := budget.counts(); active != 1 || commits != 1 || releases != 0 {
		t.Fatalf("reopen did not idempotently adopt response active=%d commits=%d releases=%d", active, commits, releases)
	}
	clock.Advance(time.Duration(access.MaxResponseAge+1) * time.Second)
	if _, err := reopened.ServeOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if active, commits, releases := budget.counts(); active != 0 || commits != 1 || releases != 1 {
		t.Fatalf("budget after anchored response removal active=%d commits=%d releases=%d", active, commits, releases)
	}
	if _, err := second.server.ServeOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if secondDispatch.Load() != 1 {
		t.Fatalf("released aggregate capacity did not dispatch: %d", secondDispatch.Load())
	}
	if err := second.server.Close(); err != nil {
		t.Fatal(err)
	}
	second.server = nil
	if active, _, releases := budget.counts(); active != 1 || releases != 1 {
		t.Fatalf("close released second durable response active=%d releases=%d", active, releases)
	}
}

func TestRestartBudgetScrubsResponseTemporaryBeforeAdoption(t *testing.T) {
	budget := &testResponseBudget{limit: 1}
	var dispatched atomic.Int32
	callbacks := Callbacks{
		Authorize: func(context.Context, access.Principal, Call) error { return nil },
		Dispatch:  countDispatch(&dispatched),
	}
	f := newFixture(t, callbacks, ServerOptions{ResponseBudget: budget})
	envelope, encoded := signedCall(t, f.auth, f.grant.CredentialHostDir, Call{
		Kind: CallOperation, Route: access.RouteQuery, Verb: "snapshot",
	})
	publishEnvelope(t, f.grant.RequestsHostDir, envelope, encoded)
	if _, err := f.server.ServeOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.server.Close(); err != nil {
		t.Fatal(err)
	}
	f.server = nil

	temporary := filepath.Join(f.grant.MailboxHostDir, ResponsesDirName, ".tmp-"+strings.Repeat("a", 32))
	if err := os.WriteFile(temporary, []byte("crash-left"), regularFileMode); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenServerMailbox(f.grant.SubjectID, f.grant.MailboxHostDir, f.auth, f.auth,
		callbacks, ServerOptions{ResponseBudget: budget})
	if err != nil {
		t.Fatal(err)
	}
	f.server = reopened
	if _, err := reopened.ServeOnce(context.Background()); err != nil {
		t.Fatalf("restart adoption after response temporary: %v", err)
	}
	if _, err := os.Stat(temporary); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("crash-left response temporary remains: %v", err)
	}
	if active, commits, releases := budget.counts(); active != 1 || commits != 1 || releases != 0 {
		t.Fatalf("retained response accounting active=%d commits=%d releases=%d", active, commits, releases)
	}
}

func TestRestartCleanupAndAdoptionProgressBeyondOneChunk(t *testing.T) {
	clock := newTestClock(time.Now())
	f := newFixture(t, Callbacks{}, ServerOptions{Clock: clock.Now})
	if err := f.server.Close(); err != nil {
		t.Fatal(err)
	}
	f.server = nil
	responseDir := filepath.Join(f.grant.MailboxHostDir, ResponsesDirName)
	old := clock.Now().Add(-time.Duration(access.MaxResponseAge+1) * time.Second)
	for index := 0; index < MaxResponseFiles+1; index++ {
		name := fmt.Sprintf("%032x.res", index+1)
		path := filepath.Join(responseDir, name)
		if err := os.WriteFile(path, []byte("{}"), regularFileMode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	for index := 0; index < 2*MaxQueuedRequests+1; index++ {
		name := fmt.Sprintf(".tmp-%032x", index+MaxResponseFiles+2)
		if err := os.WriteFile(filepath.Join(responseDir, name), []byte("crash-left"), regularFileMode); err != nil {
			t.Fatal(err)
		}
	}

	budget := &testResponseBudget{limit: 1}
	reopened, err := OpenServerMailbox(f.grant.SubjectID, f.grant.MailboxHostDir, f.auth, f.auth, Callbacks{
		Authorize: func(context.Context, access.Principal, Call) error { return nil },
		Dispatch: func(context.Context, DispatchRequest) (DispatchResult, error) {
			return DispatchResult{Status: StatusOK}, nil
		},
	}, ServerOptions{Clock: clock.Now, ResponseBudget: budget})
	if err != nil {
		t.Fatal(err)
	}
	f.server = reopened
	previous := 3*MaxQueuedRequests + 2
	madeProgress := false
	for attempt := 0; attempt < 32; attempt++ {
		_, serveErr := reopened.ServeOnce(context.Background())
		entries, readErr := os.ReadDir(responseDir)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if len(entries) > previous {
			t.Fatalf("startup cleanup grew: before=%d after=%d err=%v", previous, len(entries), serveErr)
		}
		if len(entries) < previous {
			madeProgress = true
		}
		previous = len(entries)
		if serveErr == nil {
			break
		}
		if !errors.Is(serveErr, ErrResponseCapacity) {
			t.Fatalf("startup cleanup error: %v", serveErr)
		}
	}
	if previous != 0 {
		t.Fatalf("startup cleanup left %d response artifacts", previous)
	}
	if !madeProgress {
		t.Fatal("startup cleanup never advanced beyond its first bounded scan")
	}
	if active, commits, releases := budget.counts(); active != 0 || commits != MaxResponseFiles+1 || releases != MaxResponseFiles+1 {
		t.Fatalf("over-capacity adoption accounting active=%d commits=%d releases=%d", active, commits, releases)
	}
}

func TestAggregateReceiptLeaseReleasedOnSettlement(t *testing.T) {
	budget := &testResponseBudget{limit: 4}
	clock := newTestClock(time.Now())
	settled := make(chan ReceiptSettlement, 1)
	f := newFixture(t, Callbacks{
		Dispatch: func(context.Context, DispatchRequest) (DispatchResult, error) {
			return DispatchResult{Status: StatusOK, Receipt: &ReceiptHooks{
				Grace: time.Second, Settled: func(value ReceiptSettlement) { settled <- value },
			}}, nil
		},
	}, ServerOptions{Clock: clock.Now, SettlementInterval: time.Millisecond, ResponseBudget: budget})
	envelope, encoded := signedCall(t, f.auth, f.grant.CredentialHostDir, Call{
		Kind: CallOperation, Route: access.RouteAction, Verb: "done", ID: f.grant.SubjectID,
	})
	publishEnvelope(t, f.grant.RequestsHostDir, envelope, encoded)
	if _, err := f.server.ServeOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := budget.receiptCount(); got != 1 {
		t.Fatalf("committed receipt slots = %d", got)
	}
	clock.Advance(2 * time.Second)
	select {
	case value := <-settled:
		if value.Reason != SettlementGraceExpired {
			t.Fatalf("settlement = %+v", value)
		}
	case <-time.After(time.Second):
		t.Fatal("receipt did not settle")
	}
	if got := budget.receiptCount(); got != 0 {
		t.Fatalf("settled receipt slots = %d", got)
	}
	if active, _, releases := budget.counts(); active != 1 || releases != 0 {
		t.Fatalf("settlement released durable response active=%d releases=%d", active, releases)
	}
}

func TestPendingReceiptAdmissionBlocksDispatchAtLimit(t *testing.T) {
	var dispatched atomic.Int32
	f := newFixture(t, Callbacks{Dispatch: countDispatch(&dispatched)}, ServerOptions{})
	deadline := time.Now().Add(time.Hour)
	f.server.mu.Lock()
	for index := 0; index < MaxPendingReceipts; index++ {
		f.server.pending[fmt.Sprintf("%032x", index)] = pendingReceipt{deadline: deadline}
	}
	f.server.mu.Unlock()
	envelope, encoded := signedCall(t, f.auth, f.grant.CredentialHostDir, Call{
		Kind: CallOperation, Route: access.RouteAction, Verb: "mutate", ID: f.grant.SubjectID,
	})
	publishEnvelope(t, f.grant.RequestsHostDir, envelope, encoded)
	if _, err := f.server.ServeOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if dispatched.Load() != 0 {
		t.Fatalf("dispatch ran with saturated pending receipts: %d", dispatched.Load())
	}
	response := readTestResponse(t, f.grant.MailboxHostDir, envelope.RequestID)
	if response.Status != StatusIndeterminate || response.Code != "receipt_capacity" {
		t.Fatalf("capacity response = %+v", response)
	}
}

func TestResponseCountAdmissionAndProgressingCleanup(t *testing.T) {
	t.Run("count-admission-before-dispatch", func(t *testing.T) {
		var dispatched atomic.Int32
		f := newFixture(t, Callbacks{Dispatch: countDispatch(&dispatched)}, ServerOptions{})
		for index := 0; index < MaxResponseFiles+1; index++ {
			envelope, encoded := signedCall(t, f.auth, f.grant.CredentialHostDir, Call{
				Kind: CallOperation, Route: access.RouteQuery, Verb: "snapshot", ID: fmt.Sprint(index),
			})
			publishEnvelope(t, f.grant.RequestsHostDir, envelope, encoded)
			_, err := f.server.ServeOnce(context.Background())
			if index < MaxResponseFiles && err != nil {
				t.Fatalf("call %d: %v", index, err)
			}
			if index == MaxResponseFiles && !errors.Is(err, ErrResponseCapacity) {
				t.Fatalf("over-capacity call error = %v", err)
			}
		}
		entries, err := os.ReadDir(filepath.Join(f.grant.MailboxHostDir, ResponsesDirName))
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != MaxResponseFiles || dispatched.Load() != MaxResponseFiles {
			t.Fatalf("responses=%d dispatches=%d", len(entries), dispatched.Load())
		}
	})

	t.Run("byte-admission-before-dispatch", func(t *testing.T) {
		var dispatched atomic.Int32
		f := newFixture(t, Callbacks{Dispatch: countDispatch(&dispatched)}, ServerOptions{})
		responsesDir := filepath.Join(f.grant.MailboxHostDir, ResponsesDirName)
		files := int(MaxResponseBytes / int64(maxEnvelopeFileBytes))
		content := bytes.Repeat([]byte{'x'}, maxEnvelopeFileBytes)
		for index := 0; index < files; index++ {
			path := filepath.Join(responsesDir, responseFileName(fmt.Sprintf("%032x", index)))
			if err := os.WriteFile(path, content, regularFileMode); err != nil {
				t.Fatal(err)
			}
		}
		envelope, encoded := signedCall(t, f.auth, f.grant.CredentialHostDir, Call{
			Kind: CallOperation, Route: access.RouteAction, Verb: "mutate", ID: f.grant.SubjectID,
		})
		publishEnvelope(t, f.grant.RequestsHostDir, envelope, encoded)
		if _, err := f.server.ServeOnce(context.Background()); !errors.Is(err, ErrResponseCapacity) {
			t.Fatalf("byte-capacity error = %v", err)
		}
		if dispatched.Load() != 0 {
			t.Fatalf("dispatch ran without worst-case response bytes: %d", dispatched.Load())
		}
	})

	t.Run("cleanup-advances-past-first-chunk", func(t *testing.T) {
		now := time.Now()
		f := newFixture(t, Callbacks{}, ServerOptions{Clock: func() time.Time { return now }})
		responsesDir := filepath.Join(f.grant.MailboxHostDir, ResponsesDirName)
		old := now.Add(-time.Duration(access.MaxResponseAge+1) * time.Second)
		for index := 0; index < MaxResponseFiles+17; index++ {
			path := filepath.Join(responsesDir, responseFileName(fmt.Sprintf("%032x", index)))
			if err := os.WriteFile(path, []byte("stale"), regularFileMode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
		}
		for attempt := 0; attempt < 3; attempt++ {
			if _, err := f.server.ServeOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		entries, err := os.ReadDir(responsesDir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("progressing cleanup left %d stale responses", len(entries))
		}
	})
}

func TestReceiptDeadlineAtAcceptanceAndIndependentSettlement(t *testing.T) {
	t.Run("deadline-rechecked-after-authorize", func(t *testing.T) {
		var nowMillis atomic.Int64
		nowMillis.Store(time.Now().UnixMilli())
		clock := func() time.Time { return time.UnixMilli(nowMillis.Load()) }
		settled := make(chan ReceiptSettlement, 1)
		f := newFixture(t, Callbacks{
			Authorize: func(_ context.Context, _ access.Principal, call Call) error {
				if call.Kind == CallReceipt {
					nowMillis.Add(int64(2 * time.Second / time.Millisecond))
				}
				return nil
			},
			Dispatch: func(context.Context, DispatchRequest) (DispatchResult, error) {
				return DispatchResult{Status: StatusOK, Receipt: &ReceiptHooks{
					Grace: time.Second, Settled: func(value ReceiptSettlement) { settled <- value },
				}}, nil
			},
		}, ServerOptions{Clock: clock})
		// Isolate the acceptance-time check from the independent expiry loop;
		// the next subtest covers that loop itself.
		f.server.settlementStopOnce.Do(func() { close(f.server.settlementStop) })
		<-f.server.settlementDone
		operation, encoded := signedCall(t, f.auth, f.grant.CredentialHostDir, Call{
			Kind: CallOperation, Route: access.RouteAction, Verb: "done", ID: f.grant.SubjectID,
		})
		publishEnvelope(t, f.grant.RequestsHostDir, operation, encoded)
		if _, err := f.server.ServeOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		response := readTestResponse(t, f.grant.MailboxHostDir, operation.RequestID)
		receipt, receiptBytes := signedCall(t, f.auth, f.grant.CredentialHostDir, Call{
			Kind: CallReceipt, Receipt: &Receipt{RequestID: operation.RequestID, ResponseDigest: responseDigest(response)},
		})
		publishEnvelope(t, f.grant.RequestsHostDir, receipt, receiptBytes)
		if _, err := f.server.ServeOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		value := <-settled
		if value.Received || value.Reason != SettlementGraceExpired {
			t.Fatalf("late receipt settlement = %+v", value)
		}
		receiptResponse := readTestResponse(t, f.grant.MailboxHostDir, receipt.RequestID)
		if receiptResponse.Status != StatusInvalid || receiptResponse.Code != "receipt_expired" {
			t.Fatalf("late receipt response = %+v", receiptResponse)
		}
	})

	t.Run("settles-without-another-server-tick", func(t *testing.T) {
		var nowMillis atomic.Int64
		nowMillis.Store(time.Now().UnixMilli())
		clock := func() time.Time { return time.UnixMilli(nowMillis.Load()) }
		settled := make(chan ReceiptSettlement, 1)
		f := newFixture(t, Callbacks{
			Dispatch: func(context.Context, DispatchRequest) (DispatchResult, error) {
				return DispatchResult{Status: StatusOK, Receipt: &ReceiptHooks{
					Grace: time.Second, Settled: func(value ReceiptSettlement) { settled <- value },
				}}, nil
			},
		}, ServerOptions{Clock: clock, SettlementInterval: time.Millisecond})
		operation, encoded := signedCall(t, f.auth, f.grant.CredentialHostDir, Call{
			Kind: CallOperation, Route: access.RouteAction, Verb: "done", ID: f.grant.SubjectID,
		})
		publishEnvelope(t, f.grant.RequestsHostDir, operation, encoded)
		if _, err := f.server.ServeOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		nowMillis.Add(int64(2 * time.Second / time.Millisecond))
		select {
		case value := <-settled:
			if value.Received || value.Reason != SettlementGraceExpired {
				t.Fatalf("independent settlement = %+v", value)
			}
		case <-time.After(time.Second):
			t.Fatal("receipt grace did not settle independently")
		}
	})
}

func TestStrictJSONRejectsUnknownDuplicateAndTrailingInput(t *testing.T) {
	for name, body := range map[string][]byte{
		"unknown":                  []byte(`{"kind":"call","route":"query","verb":"snapshot","unknown":true}`),
		"duplicate":                []byte(`{"kind":"call","route":"query","verb":"first","verb":"last"}`),
		"case_folded_duplicate":    []byte(`{"kind":"receipt","Kind":"call","route":"query","verb":"snapshot"}`),
		"case_folded_alias":        []byte(`{"Kind":"call","route":"query","verb":"snapshot"}`),
		"nested_duplicate":         []byte(`{"kind":"call","route":"query","verb":"snapshot","fields":{"x":"first","x":"last"}}`),
		"nested_case_folded_alias": []byte(`{"kind":"receipt","receipt":{"RequestID":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","responseDigest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`),
		"trailing":                 []byte(`{"kind":"call","route":"query","verb":"snapshot"} {}`),
	} {
		t.Run(name, func(t *testing.T) {
			var call Call
			if err := unmarshalBounded(body, access.MaxBodyBytes, &call); !errors.Is(err, ErrInvalidRecord) {
				t.Fatalf("strict decode error = %v", err)
			}
			var dispatched atomic.Int32
			f := newFixture(t, Callbacks{Dispatch: countDispatch(&dispatched)}, ServerOptions{})
			envelope, encoded := signedBody(t, f.auth, f.grant.CredentialHostDir, body)
			publishEnvelope(t, f.grant.RequestsHostDir, envelope, encoded)
			if _, err := f.server.ServeOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if dispatched.Load() != 0 {
				t.Fatalf("strictly invalid signed call dispatched %d times", dispatched.Load())
			}
			response := readTestResponse(t, f.grant.MailboxHostDir, envelope.RequestID)
			if response.Status != StatusInvalid || response.Code != "invalid_call" {
				t.Fatalf("strictly invalid response = %+v", response)
			}
		})
	}
}

func TestStrictJSONAllowsArbitraryCaseSensitiveFieldMapKeys(t *testing.T) {
	var call Call
	body := []byte(`{"kind":"call","route":"query","verb":"snapshot","fields":{"UserKey":"value"}}`)
	if err := unmarshalBounded(body, access.MaxBodyBytes, &call); err != nil {
		t.Fatal(err)
	}
	if call.Fields["UserKey"] != "value" {
		t.Fatalf("field map = %#v", call.Fields)
	}
}

func TestStrictJSONRequiresCanonicalNamesAcrossWireRecords(t *testing.T) {
	tests := []struct {
		name      string
		canonical []byte
		alias     []byte
		value     func() any
	}{
		{"context", []byte(`{"subjectId":"subject-a"}`), []byte(`{"SubjectID":"subject-a"}`), func() any { return new(SessionContext) }},
		{"credential", []byte(`{"keyId":"aa"}`), []byte(`{"KeyID":"aa"}`), func() any { return new(access.Credential) }},
		{"service", []byte(`{"bootId":"aa"}`), []byte(`{"BootID":"aa"}`), func() any { return new(Service) }},
		{"request", []byte(`{"requestId":"aa"}`), []byte(`{"RequestID":"aa"}`), func() any { return new(access.SignedRequest) }},
		{"response", []byte(`{"completedAt":1}`), []byte(`{"CompletedAt":1}`), func() any { return new(Response) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := unmarshalBounded(test.canonical, maxEnvelopeFileBytes, test.value()); err != nil {
				t.Fatalf("canonical field rejected: %v", err)
			}
			if err := unmarshalBounded(test.alias, maxEnvelopeFileBytes, test.value()); !errors.Is(err, ErrInvalidRecord) {
				t.Fatalf("case-folded alias error = %v", err)
			}
		})
	}
}

func TestServerRemovesAbandonedAtomicPublication(t *testing.T) {
	clock := newTestClock(time.Now())
	f := newFixture(t, Callbacks{}, ServerOptions{Clock: clock.Now})
	name := ".tmp-" + strings.Repeat("a", 32)
	path := filepath.Join(f.grant.RequestsHostDir, name)
	if err := os.WriteFile(path, []byte("abandoned"), regularFileMode); err != nil {
		t.Fatal(err)
	}
	clock.Advance(requestTemporaryMaxAge + time.Second)
	if _, err := f.server.ServeOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("abandoned temporary remains: %v", err)
	}
}

func TestClientRejectsForgedResponsePath(t *testing.T) {
	f := newFixture(t, Callbacks{}, ServerOptions{})
	responseName := responseFileName(string(bytes.Repeat([]byte{'0'}, 32)))
	if err := os.Symlink("/etc/passwd", filepath.Join(f.grant.MailboxHostDir, ResponsesDirName, responseName)); err != nil {
		t.Fatal(err)
	}
	client := f.client(clientOptions{random: zeroReader{}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := client.Query(ctx, Query{Verb: "snapshot"}); err == nil {
		t.Fatal("forged response symlink was accepted")
	}
}

type zeroReader struct{}

func (zeroReader) Read(buffer []byte) (int, error) {
	clear(buffer)
	return len(buffer), nil
}

type testClock struct{ millis atomic.Int64 }

func newTestClock(now time.Time) *testClock {
	clock := &testClock{}
	clock.millis.Store(now.UnixMilli())
	return clock
}

func (c *testClock) Now() time.Time { return time.UnixMilli(c.millis.Load()) }
func (c *testClock) Advance(delta time.Duration) {
	c.millis.Add(int64(delta / time.Millisecond))
}

func TestServerRejectsSymlinkFIFOHardlinkAndBoundsQueue(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		var dispatch atomic.Int32
		f := newFixture(t, Callbacks{Dispatch: countDispatch(&dispatch)}, ServerOptions{})
		name := requestFileName(string(bytes.Repeat([]byte{'1'}, 32)))
		if err := os.Symlink("/etc/passwd", filepath.Join(f.grant.RequestsHostDir, name)); err != nil {
			t.Fatal(err)
		}
		if _, err := f.server.ServeOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if dispatch.Load() != 0 {
			t.Fatal("symlink dispatched")
		}
	})

	t.Run("fifo", func(t *testing.T) {
		var dispatch atomic.Int32
		f := newFixture(t, Callbacks{Dispatch: countDispatch(&dispatch)}, ServerOptions{})
		name := requestFileName(string(bytes.Repeat([]byte{'2'}, 32)))
		if err := unix.Mkfifo(filepath.Join(f.grant.RequestsHostDir, name), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := f.server.ServeOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if dispatch.Load() != 0 {
			t.Fatal("FIFO dispatched")
		}
	})

	t.Run("hardlink", func(t *testing.T) {
		var dispatch atomic.Int32
		f := newFixture(t, Callbacks{Dispatch: countDispatch(&dispatch)}, ServerOptions{})
		envelope, encoded := signedCall(t, f.auth, f.grant.CredentialHostDir, Call{Kind: CallOperation, Route: access.RouteQuery, Verb: "snapshot"})
		path := filepath.Join(f.grant.RequestsHostDir, requestFileName(envelope.RequestID))
		if err := os.WriteFile(path, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(path, filepath.Join(f.grant.RequestsHostDir, "zz-hardlink")); err != nil {
			t.Fatal(err)
		}
		if _, err := f.server.ServeOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if dispatch.Load() != 0 {
			t.Fatal("multiply-linked request dispatched")
		}
	})

	t.Run("queue", func(t *testing.T) {
		f := newFixture(t, Callbacks{}, ServerOptions{})
		for index := 0; index < MaxQueuedRequests+1; index++ {
			path := filepath.Join(f.grant.RequestsHostDir, fmt.Sprintf("junk-%03d", index))
			if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		processed, err := f.server.ServeOnce(context.Background())
		if !errors.Is(err, ErrQueueFull) || processed != MaxQueuedRequests {
			t.Fatalf("processed=%d err=%v", processed, err)
		}
	})

	t.Run("over-capacity-valid-batch-never-dispatches", func(t *testing.T) {
		var dispatch atomic.Int32
		f := newFixture(t, Callbacks{Dispatch: countDispatch(&dispatch)}, ServerOptions{})
		for index := 0; index < MaxQueuedRequests+1; index++ {
			envelope, encoded := signedCall(t, f.auth, f.grant.CredentialHostDir, Call{
				Kind: CallOperation, Route: access.RouteAction, Verb: "mutate", ID: fmt.Sprint(index),
			})
			publishEnvelope(t, f.grant.RequestsHostDir, envelope, encoded)
		}
		for attempt := 0; attempt < 4; attempt++ {
			_, err := f.server.ServeOnce(context.Background())
			if err != nil && !errors.Is(err, ErrQueueFull) {
				t.Fatal(err)
			}
		}
		entries, err := os.ReadDir(f.grant.RequestsHostDir)
		if err != nil {
			t.Fatal(err)
		}
		if dispatch.Load() != 0 || len(entries) != 0 {
			t.Fatalf("over-capacity queue dispatched=%d remaining=%d", dispatch.Load(), len(entries))
		}
	})
}

func countDispatch(counter *atomic.Int32) func(context.Context, DispatchRequest) (DispatchResult, error) {
	return func(context.Context, DispatchRequest) (DispatchResult, error) {
		counter.Add(1)
		return DispatchResult{Status: StatusOK}, nil
	}
}

func signedCall(t *testing.T, authority *access.FileAuthority, credentialDir string, call Call) (access.SignedRequest, []byte) {
	t.Helper()
	body, err := json.Marshal(call)
	if err != nil {
		t.Fatal(err)
	}
	return signedBody(t, authority, credentialDir, body)
}

func signedBody(t *testing.T, authority *access.FileAuthority, credentialDir string, body []byte) (access.SignedRequest, []byte) {
	t.Helper()
	credential, err := access.LoadCredential(credentialDir)
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
	return envelope, encoded
}

func publishEnvelope(t *testing.T, requestsDir string, envelope access.SignedRequest, encoded []byte) {
	t.Helper()
	path := filepath.Join(requestsDir, requestFileName(envelope.RequestID))
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readTestResponse(t *testing.T, mailboxDir, requestID string) Response {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(mailboxDir, ResponsesDirName, responseFileName(requestID)))
	if err != nil {
		t.Fatal(err)
	}
	var response Response
	if err := json.Unmarshal(data, &response); err != nil {
		t.Fatal(err)
	}
	return response
}

type mutatingAuthority struct {
	base   *access.FileAuthority
	mutate func()
}

func (a mutatingAuthority) BootID() string { return a.base.BootID() }
func (a mutatingAuthority) Valid(ctx context.Context, principal access.Principal) error {
	return a.base.Valid(ctx, principal)
}
func (a mutatingAuthority) Verify(ctx context.Context, request access.SignedRequest) (access.Principal, error) {
	principal, err := a.base.Verify(ctx, request)
	if err == nil {
		a.mutate()
	}
	return principal, err
}

func TestClaimedRequestUsesOneImmutableCopyAfterRetainedFDMutation(t *testing.T) {
	var gotVerb string
	f := newFixture(t, Callbacks{}, ServerOptions{})
	_ = f.server.Close()
	f.server = nil
	original, encoded := signedCall(t, f.auth, f.grant.CredentialHostDir, Call{Kind: CallOperation, Route: access.RouteQuery, Verb: "original"})
	path := filepath.Join(f.grant.RequestsHostDir, requestFileName(original.RequestID))
	retained, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer retained.Close()
	if _, err := retained.Write(encoded); err != nil {
		t.Fatal(err)
	}
	if err := retained.Sync(); err != nil {
		t.Fatal(err)
	}
	mutated := bytes.Repeat([]byte{'x'}, len(encoded))
	wrapped := mutatingAuthority{base: f.auth, mutate: func() {
		if _, err := retained.WriteAt(mutated, 0); err != nil {
			t.Errorf("mutate retained request fd: %v", err)
		}
	}}
	server, err := OpenServerMailbox(f.grant.SubjectID, f.grant.MailboxHostDir, wrapped, f.auth, Callbacks{
		Authorize: func(context.Context, access.Principal, Call) error { return nil },
		Dispatch: func(_ context.Context, request DispatchRequest) (DispatchResult, error) {
			gotVerb = request.Call.Verb
			return DispatchResult{Status: StatusOK}, nil
		},
	}, ServerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	f.server = server
	if _, err := server.ServeOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotVerb != "original" {
		t.Fatalf("dispatch decoded mutable inode; verb=%q", gotVerb)
	}
}

func TestServerDescriptorsSurviveMailboxPathReplacement(t *testing.T) {
	var dispatch atomic.Int32
	f := newFixture(t, Callbacks{Dispatch: countDispatch(&dispatch)}, ServerOptions{})
	oldPath := f.grant.MailboxHostDir + ".old"
	if err := os.Rename(f.grant.MailboxHostDir, oldPath); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.grant.MailboxHostDir, RequestsDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(f.grant.MailboxHostDir, ResponsesDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	envelope, encoded := signedCall(t, f.auth, f.grant.CredentialHostDir, Call{Kind: CallOperation, Route: access.RouteQuery, Verb: "snapshot"})
	publishEnvelope(t, filepath.Join(oldPath, RequestsDirName), envelope, encoded)
	if _, err := f.server.ServeOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if dispatch.Load() != 1 {
		t.Fatalf("held mailbox descriptor lost; dispatched=%d", dispatch.Load())
	}
}

func TestRotationStableDirectoryAndRestartReconnect(t *testing.T) {
	f := newFixture(t, Callbacks{}, ServerOptions{})
	client := f.client(clientOptions{})
	first, err := pumpClient(t, f.server, func(ctx context.Context) (Result, error) {
		return client.Query(ctx, Query{Verb: "first"})
	})
	if err != nil || first.Status != StatusOK {
		t.Fatalf("first call: %+v %v", first, err)
	}
	before, err := access.LoadCredential(f.grant.CredentialHostDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.auth.Rotate(context.Background(), access.SubjectSession, f.grant.SubjectID); err != nil {
		t.Fatal(err)
	}
	after, err := access.LoadCredential(f.grant.CredentialHostDir)
	if err != nil {
		t.Fatal(err)
	}
	if after.Generation <= before.Generation {
		t.Fatalf("generation did not advance: %d -> %d", before.Generation, after.Generation)
	}
	if _, err := pumpClient(t, f.server, func(ctx context.Context) (Result, error) {
		return client.Query(ctx, Query{Verb: "after-rotation"})
	}); err != nil {
		t.Fatal(err)
	}
	oldBoot := f.auth.BootID()
	if err := f.server.Close(); err != nil {
		t.Fatal(err)
	}
	f.server = nil
	if err := f.auth.Close(); err != nil {
		t.Fatal(err)
	}
	f.auth = nil
	reopened, err := access.Open(f.authDir)
	if err != nil {
		t.Fatal(err)
	}
	f.auth = reopened
	if reopened.BootID() == oldBoot {
		t.Fatal("daemon restart reused boot ID")
	}
	server, err := OpenServerMailbox(f.grant.SubjectID, f.grant.MailboxHostDir, reopened, reopened, Callbacks{
		Authorize: func(context.Context, access.Principal, Call) error { return nil },
		Dispatch: func(context.Context, DispatchRequest) (DispatchResult, error) {
			return DispatchResult{Status: StatusOK}, nil
		},
	}, ServerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	f.server = server
	if err := server.PublishService(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := pumpClient(t, server, func(ctx context.Context) (Result, error) {
		return client.Query(ctx, Query{Verb: "after-restart"})
	}); err != nil {
		t.Fatal(err)
	}
}

func TestInFlightRestartIsIndeterminateAndNotRetried(t *testing.T) {
	var dispatch atomic.Int32
	f := newFixture(t, Callbacks{Dispatch: countDispatch(&dispatch)}, ServerOptions{})
	client := f.client(clientOptions{poll: time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := client.Action(ctx, Action{Verb: "mutate", ID: "subject-a"})
		done <- err
	}()
	requestsDir := filepath.Join(f.grant.MailboxHostDir, RequestsDirName)
	deadline := time.Now().Add(time.Second)
	for {
		entries, err := os.ReadDir(requestsDir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("client did not publish request")
		}
		time.Sleep(time.Millisecond)
	}
	if err := f.server.Close(); err != nil {
		t.Fatal(err)
	}
	f.server = nil
	if err := f.auth.Close(); err != nil {
		t.Fatal(err)
	}
	f.auth = nil
	reopened, err := access.Open(f.authDir)
	if err != nil {
		t.Fatal(err)
	}
	f.auth = reopened
	server, err := OpenServerMailbox(f.grant.SubjectID, f.grant.MailboxHostDir, reopened, reopened, Callbacks{
		Authorize: func(context.Context, access.Principal, Call) error { return nil }, Dispatch: countDispatch(&dispatch),
	}, ServerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	f.server = server
	if err := server.PublishService(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrRestarted) || !errors.Is(err, ErrIndeterminate) {
			t.Fatalf("restart error=%v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if dispatch.Load() != 0 {
		t.Fatalf("in-flight mutation was retried: %d dispatches", dispatch.Load())
	}
}

func TestReceiptOrderingAndArchivedPolicyNarrowness(t *testing.T) {
	var mu sync.Mutex
	var events []string
	archived := false
	f := newFixture(t, Callbacks{
		Authorize: func(_ context.Context, _ access.Principal, call Call) error {
			mu.Lock()
			defer mu.Unlock()
			if archived && call.Kind != CallReceipt {
				return errors.New("archived")
			}
			return nil
		},
		Dispatch: func(_ context.Context, request DispatchRequest) (DispatchResult, error) {
			mu.Lock()
			archived = true
			events = append(events, "commit")
			mu.Unlock()
			return DispatchResult{Status: StatusOK, Receipt: &ReceiptHooks{
				Grace: time.Second,
				ResponsePersisted: func(PersistedResponse) {
					mu.Lock()
					events = append(events, "persisted")
					mu.Unlock()
				},
				Settled: func(settlement ReceiptSettlement) {
					mu.Lock()
					events = append(events, fmt.Sprintf("settled:%t", settlement.Received))
					mu.Unlock()
				},
			}}, nil
		},
	}, ServerOptions{})
	client := f.client(clientOptions{})
	result, err := pumpClient(t, f.server, func(ctx context.Context) (Result, error) {
		return client.Action(ctx, Action{Verb: "done", ID: "subject-a"})
	})
	if err != nil || !result.ReceiptSent {
		t.Fatalf("done result=%+v err=%v", result, err)
	}
	// While archived, an unrelated operation is denied at current policy.
	other, encoded := signedCall(t, f.auth, f.grant.CredentialHostDir, Call{Kind: CallOperation, Route: access.RouteQuery, Verb: "snapshot"})
	publishEnvelope(t, f.grant.RequestsHostDir, other, encoded)
	if _, err := f.server.ServeOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The client's separately authenticated receipt remains narrowly allowed.
	if _, err := f.server.ServeOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got := fmt.Sprint(events); got != "[commit persisted settled:true]" {
		t.Fatalf("callback order=%s", got)
	}
}

func TestReceiptGraceSettlesWithoutBypass(t *testing.T) {
	clock := newTestClock(time.Now())
	settled := make(chan ReceiptSettlement, 1)
	f := newFixture(t, Callbacks{
		Dispatch: func(context.Context, DispatchRequest) (DispatchResult, error) {
			return DispatchResult{Status: StatusOK, Receipt: &ReceiptHooks{
				Grace: time.Second, Settled: func(value ReceiptSettlement) { settled <- value },
			}}, nil
		},
	}, ServerOptions{Clock: clock.Now})
	envelope, encoded := signedCall(t, f.auth, f.grant.CredentialHostDir, Call{Kind: CallOperation, Route: access.RouteAction, Verb: "done", ID: "subject-a"})
	publishEnvelope(t, f.grant.RequestsHostDir, envelope, encoded)
	if _, err := f.server.ServeOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Second)
	if _, err := f.server.ServeOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case value := <-settled:
		if value.Received || value.RequestID != envelope.RequestID {
			t.Fatalf("settlement=%+v", value)
		}
	default:
		t.Fatal("grace expiry did not settle")
	}
}

func TestDuplicateEnvelopeNeverDispatchesAgain(t *testing.T) {
	var dispatch atomic.Int32
	f := newFixture(t, Callbacks{Dispatch: countDispatch(&dispatch)}, ServerOptions{})
	envelope, encoded := signedCall(t, f.auth, f.grant.CredentialHostDir, Call{Kind: CallOperation, Route: access.RouteAction, Verb: "mutate", ID: "subject-a"})
	publishEnvelope(t, f.grant.RequestsHostDir, envelope, encoded)
	if _, err := f.server.ServeOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	publishEnvelope(t, f.grant.RequestsHostDir, envelope, encoded)
	if _, err := f.server.ServeOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if dispatch.Load() != 1 {
		t.Fatalf("duplicate dispatched %d times", dispatch.Load())
	}
}

func TestClaimRecoveryAndAcceptedCrashBecomeExplicit(t *testing.T) {
	t.Run("claim-before-verify-recovers", func(t *testing.T) {
		var dispatch atomic.Int32
		f := newFixture(t, Callbacks{Dispatch: countDispatch(&dispatch)}, ServerOptions{})
		envelope, encoded := signedCall(t, f.auth, f.grant.CredentialHostDir, Call{Kind: CallOperation, Route: access.RouteQuery, Verb: "recover"})
		processingDir := filepath.Join(f.grant.MailboxHostDir, processingDirName)
		publishEnvelope(t, processingDir, envelope, encoded)
		if _, err := f.server.ServeOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if dispatch.Load() != 1 {
			t.Fatalf("claimed unaccepted request dispatches=%d", dispatch.Load())
		}
	})

	t.Run("accepted-before-dispatch-is-not-retried", func(t *testing.T) {
		var dispatch atomic.Int32
		f := newFixture(t, Callbacks{Dispatch: countDispatch(&dispatch)}, ServerOptions{})
		envelope, encoded := signedCall(t, f.auth, f.grant.CredentialHostDir, Call{Kind: CallOperation, Route: access.RouteAction, Verb: "mutate", ID: "subject-a"})
		if _, err := f.auth.Verify(context.Background(), envelope); err != nil {
			t.Fatal(err)
		}
		publishEnvelope(t, filepath.Join(f.grant.MailboxHostDir, processingDirName), envelope, encoded)
		if _, err := f.server.ServeOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		if dispatch.Load() != 0 {
			t.Fatalf("accepted request retried %d times", dispatch.Load())
		}
		data, err := os.ReadFile(filepath.Join(f.grant.MailboxHostDir, ResponsesDirName, responseFileName(envelope.RequestID)))
		if err != nil {
			t.Fatal(err)
		}
		var response Response
		if err := json.Unmarshal(data, &response); err != nil {
			t.Fatal(err)
		}
		if response.Status != StatusReplay || response.Code != "replay_rejected" {
			t.Fatalf("response=%+v", response)
		}
	})
}

func TestExpiredOversizedAndUnsafeModeNeverDispatch(t *testing.T) {
	var dispatch atomic.Int32
	f := newFixture(t, Callbacks{Dispatch: countDispatch(&dispatch)}, ServerOptions{})
	credential, err := access.LoadCredential(f.grant.CredentialHostDir)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(Call{Kind: CallOperation, Route: access.RouteQuery, Verb: "expired"})
	if err != nil {
		t.Fatal(err)
	}
	expired, err := access.SignRequest(credential, f.auth.BootID(), body, time.Now().Add(-time.Minute), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	expiredBytes, _ := json.Marshal(expired)
	publishEnvelope(t, f.grant.RequestsHostDir, expired, expiredBytes)
	if _, err := f.server.ServeOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	oversizedID := string(bytes.Repeat([]byte{'d'}, 32))
	if err := os.WriteFile(filepath.Join(f.grant.RequestsHostDir, requestFileName(oversizedID)), bytes.Repeat([]byte{'x'}, maxEnvelopeFileBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.server.ServeOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	unsafeEnvelope, unsafeBytes := signedCall(t, f.auth, f.grant.CredentialHostDir, Call{Kind: CallOperation, Route: access.RouteQuery, Verb: "unsafe-mode"})
	unsafePath := filepath.Join(f.grant.RequestsHostDir, requestFileName(unsafeEnvelope.RequestID))
	if err := os.WriteFile(unsafePath, unsafeBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unsafePath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.server.ServeOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if dispatch.Load() != 0 {
		t.Fatalf("invalid requests dispatched=%d", dispatch.Load())
	}
}

func TestOversizedDispatchResultBecomesSignedFailure(t *testing.T) {
	f := newFixture(t, Callbacks{Dispatch: func(context.Context, DispatchRequest) (DispatchResult, error) {
		return DispatchResult{Status: StatusOK, Body: bytes.Repeat([]byte{'x'}, MaxResponseBody+1)}, nil
	}}, ServerOptions{})
	client := f.client(clientOptions{})
	result, err := pumpClient(t, f.server, func(ctx context.Context) (Result, error) {
		return client.Query(ctx, Query{Verb: "oversized-result"})
	})
	var remote *RemoteError
	if !errors.As(err, &remote) || result.Status != StatusFailed || result.Code != "invalid_dispatch_result" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestSignedServiceReplacementAndTamperDetection(t *testing.T) {
	clock := newTestClock(time.Now())
	f := newFixture(t, Callbacks{}, ServerOptions{Clock: clock.Now})
	first, err := os.Stat(filepath.Join(f.grant.MailboxHostDir, ServiceFileName))
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Second)
	if err := f.server.PublishService(context.Background()); err != nil {
		t.Fatal(err)
	}
	second, err := os.Stat(filepath.Join(f.grant.MailboxHostDir, ServiceFileName))
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(first, second) {
		t.Fatal("service publication reused inode instead of atomic replacement")
	}
	credential, err := access.LoadCredential(f.grant.CredentialHostDir)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(f.grant.MailboxHostDir, ServiceFileName))
	if err != nil {
		t.Fatal(err)
	}
	var service Service
	if err := json.Unmarshal(data, &service); err != nil {
		t.Fatal(err)
	}
	service.BootID = string(bytes.Repeat([]byte{'e'}, 64))
	if err := verifyService(credential, service); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("tampered service err=%v", err)
	}
}

func TestClientRetriesServiceReadAcrossAtomicReplacement(t *testing.T) {
	f := newFixture(t, Callbacks{}, ServerOptions{})
	var (
		once       sync.Once
		replaceErr error
	)
	client := f.client(clientOptions{
		poll: time.Millisecond,
		afterServiceStat: func() {
			once.Do(func() { replaceErr = f.server.PublishService(context.Background()) })
		},
	})
	result, err := pumpClient(t, f.server, func(ctx context.Context) (Result, error) {
		return client.Query(ctx, Query{Verb: "snapshot", Fields: map[string]string{"value": "republished"}})
	})
	if replaceErr != nil {
		t.Fatal(replaceErr)
	}
	if err != nil || string(result.Body) != "ok:republished" {
		t.Fatalf("call across service replacement result=%+v err=%v", result, err)
	}
}

func TestOrdinaryShellCanPublishOnlyARegularFileProbe(t *testing.T) {
	f := newFixture(t, Callbacks{}, ServerOptions{})
	name := requestFileName(string(bytes.Repeat([]byte{'a'}, 32)))
	command := exec.Command("/bin/sh", "-c", "umask 077; printf '%s' '{}' > \"$1\"; test -f \"$1\"", "sh", filepath.Join(f.grant.RequestsHostDir, name))
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("ordinary shell file probe: %v: %s", err, output)
	}
	if _, err := f.server.ServeOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.grant.RequestsHostDir, name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsigned shell probe was not consumed/rejected: %v", err)
	}
}

func TestResponseSignatureBindsStatusBodyAndRequest(t *testing.T) {
	f := newFixture(t, Callbacks{}, ServerOptions{})
	credential, err := access.LoadCredential(f.grant.CredentialHostDir)
	if err != nil {
		t.Fatal(err)
	}
	requestID := string(bytes.Repeat([]byte{'b'}, 32))
	requestDigestBytes := sha256.Sum256([]byte("request"))
	response := Response{
		Protocol: ProtocolVersion, BootID: f.auth.BootID(), IssuerKeyID: f.auth.IssuerKeyID(),
		SubjectID: f.grant.SubjectID, KeyID: credential.KeyID, Generation: credential.Generation,
		RequestID: requestID, RequestBodyDigest: hex.EncodeToString(requestDigestBytes[:]),
		Status: StatusOK, Body: []byte("body"), CompletedAt: time.Now().UnixMilli(),
	}
	if err := signResponse(f.auth, &response); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Response){
		"status":  func(value *Response) { value.Status = StatusDenied },
		"body":    func(value *Response) { value.Body[0] ^= 1 },
		"request": func(value *Response) { value.RequestID = string(bytes.Repeat([]byte{'c'}, 32)) },
	} {
		t.Run(name, func(t *testing.T) {
			tampered := response
			tampered.Body = append([]byte(nil), response.Body...)
			mutate(&tampered)
			if err := verifyResponse(credential, tampered); !errors.Is(err, ErrInvalidSignature) {
				t.Fatalf("tampered response err=%v", err)
			}
		})
	}
}

var _ io.Reader = zeroReader{}
