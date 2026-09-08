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

func TestServerScanPreservesInFlightAtomicPublication(t *testing.T) {
	f := newFixture(t, Callbacks{}, ServerOptions{})
	created := make(chan struct{})
	resume := make(chan struct{})
	client := f.client(clientOptions{
		poll: time.Millisecond,
		afterRequestCreate: func() {
			close(created)
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
	case <-created:
	case <-ctx.Done():
		t.Fatal("client did not create publication temporary")
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

func TestServerRemovesAbandonedAtomicPublication(t *testing.T) {
	now := time.Now()
	f := newFixture(t, Callbacks{}, ServerOptions{Clock: func() time.Time { return now }})
	name := ".tmp-" + strings.Repeat("a", 32)
	path := filepath.Join(f.grant.RequestsHostDir, name)
	if err := os.WriteFile(path, []byte("abandoned"), regularFileMode); err != nil {
		t.Fatal(err)
	}
	now = now.Add(requestTemporaryMaxAge + time.Second)
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
	credential, err := access.LoadCredential(credentialDir)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(call)
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
	now := time.Now()
	settled := make(chan ReceiptSettlement, 1)
	f := newFixture(t, Callbacks{
		Dispatch: func(context.Context, DispatchRequest) (DispatchResult, error) {
			return DispatchResult{Status: StatusOK, Receipt: &ReceiptHooks{
				Grace: time.Second, Settled: func(value ReceiptSettlement) { settled <- value },
			}}, nil
		},
	}, ServerOptions{Clock: func() time.Time { return now }})
	envelope, encoded := signedCall(t, f.auth, f.grant.CredentialHostDir, Call{Kind: CallOperation, Route: access.RouteAction, Verb: "done", ID: "subject-a"})
	publishEnvelope(t, f.grant.RequestsHostDir, envelope, encoded)
	if _, err := f.server.ServeOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
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
	now := time.Now()
	f := newFixture(t, Callbacks{}, ServerOptions{Clock: func() time.Time { return now }})
	first, err := os.Stat(filepath.Join(f.grant.MailboxHostDir, ServiceFileName))
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
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
