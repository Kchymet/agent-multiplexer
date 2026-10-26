package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"amux/internal/access"
	"amux/internal/core"
	"amux/internal/engine"
	"amux/internal/panespec"
	"amux/internal/sessionreport"
	"amux/internal/sessionrpc"
	"amux/internal/source"
	"amux/internal/store"

	"github.com/gorilla/websocket"
)

type lifecycleBlockingSource struct {
	entered chan struct{}
	exited  chan struct{}
	once    sync.Once
}

func (s *lifecycleBlockingSource) Name() string { return "lifecycle-block" }

func (s *lifecycleBlockingSource) Poll(ctx context.Context) ([]core.Session, error) {
	s.once.Do(func() { close(s.entered) })
	<-ctx.Done()
	close(s.exited)
	return nil, ctx.Err()
}

type lifecycleTestEngine struct {
	shutdownOnce sync.Once
	shutdown     chan error
	check        func() error
}

type lifecycleListener struct {
	closed chan struct{}
	once   sync.Once
}

func newLifecycleListener() *lifecycleListener {
	return &lifecycleListener{closed: make(chan struct{})}
}

func (l *lifecycleListener) Accept() (net.Conn, error) {
	<-l.closed
	return nil, net.ErrClosed
}

func (l *lifecycleListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (*lifecycleListener) Addr() net.Addr { return &net.UnixAddr{Name: "lifecycle-test", Net: "unix"} }

func newLifecycleTestEngine() *lifecycleTestEngine {
	return &lifecycleTestEngine{shutdown: make(chan error, 1)}
}

func (*lifecycleTestEngine) Name() string { return "lifecycle-test" }
func (*lifecycleTestEngine) Ensure(context.Context, engine.Spec) (engine.Instance, error) {
	return nil, errors.New("lifecycle test engine does not start instances")
}
func (*lifecycleTestEngine) Lookup(engine.Key) (engine.Instance, bool) { return nil, false }
func (*lifecycleTestEngine) Live() []engine.Key                        { return nil }
func (*lifecycleTestEngine) Kill(engine.Key)                           {}
func (e *lifecycleTestEngine) Shutdown() {
	e.shutdownOnce.Do(func() {
		var err error
		if e.check != nil {
			err = e.check()
		}
		e.shutdown <- err
	})
}

func TestRunDrainsOwnedWorkBeforeEngineAndAuthority(t *testing.T) {
	isolateControl(t)
	authorityRoot := filepath.Join(t.TempDir(), "authority")
	authority, err := access.Open(authorityRoot)
	if err != nil {
		t.Fatal(err)
	}
	credentialDir, err := authority.EnsureHost(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	credential, err := access.LoadCredential(credentialDir)
	if err != nil {
		t.Fatal(err)
	}
	principal := credentialPrincipal(credential)

	pollSource := &lifecycleBlockingSource{entered: make(chan struct{}), exited: make(chan struct{})}
	eng := newLifecycleTestEngine()
	d := New("", []source.Source{pollSource}, time.Hour)
	d.authority = authority
	d.engine = eng
	listener := newLifecycleListener()
	d.listen = func(string, string) (net.Listener, func(), error) {
		return listener, func() {}, nil
	}
	d.launchSpec = func(context.Context, string) (panespec.LaunchSpec, error) {
		return panespec.LaunchSpec{}, errors.New("disabled in lifecycle test")
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- d.Run(ctx) }()
	select {
	case <-pollSource.entered:
	case err := <-runDone:
		t.Fatalf("Run exited before polling: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not begin polling")
	}

	deferredEntered := make(chan struct{})
	deferredExited := make(chan struct{})
	if !d.startDeferredWork(func() {
		close(deferredEntered)
		<-ctx.Done()
		close(deferredExited)
	}) {
		t.Fatal("deferred work rejected before shutdown")
	}
	<-deferredEntered

	completion := d.sessionRPC.completions
	completion.begin(principal, strings.Repeat("a", 32), "pending-completion")
	completion.mu.Lock()
	entry := completion.entries["pending-completion"]
	completion.mu.Unlock()
	if entry == nil {
		t.Fatal("completion owner was not registered")
	}

	eng.check = func() error {
		select {
		case <-pollSource.exited:
		default:
			return errors.New("poll loop still owned source work at engine shutdown")
		}
		select {
		case <-deferredExited:
		default:
			return errors.New("deferred action still running at engine shutdown")
		}
		select {
		case <-entry.done:
		default:
			return errors.New("completion owner still running at engine shutdown")
		}
		completion.mu.Lock()
		closed := completion.closed
		completion.mu.Unlock()
		if !closed {
			return errors.New("completion registry not closed at engine shutdown")
		}
		if err := d.authority.Valid(context.Background(), principal); err != nil {
			return fmt.Errorf("authority closed before engine shutdown: %w", err)
		}
		return nil
	}

	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run shutdown: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run shutdown deadlocked")
	}
	if err := <-eng.shutdown; err != nil {
		t.Fatal(err)
	}
	reopened, err := access.Open(authorityRoot)
	if err != nil {
		t.Fatalf("authority ownership leaked after Run: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRunInterruptsStalledStructuredWriteBeforeDeferredJoin(t *testing.T) {
	isolateControl(t)
	authority, err := access.Open(filepath.Join(t.TempDir(), "authority"))
	if err != nil {
		t.Fatal(err)
	}

	protocolReady := make(chan struct{})
	frameEntered := make(chan struct{})
	releaseServer := make(chan struct{})
	serverErr := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			serverErr <- fmt.Errorf("upgrade: %w", err)
			return
		}
		defer conn.Close()
		if tcp, ok := conn.UnderlyingConn().(*net.TCPConn); ok {
			_ = tcp.SetReadBuffer(1024)
		}

		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				serverErr <- fmt.Errorf("handshake read: %w", err)
				return
			}
			var call struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if err := json.Unmarshal(raw, &call); err != nil {
				serverErr <- fmt.Errorf("decode handshake call: %w", err)
				return
			}
			if len(call.ID) == 0 || string(call.ID) == "null" {
				continue
			}
			var result any = map[string]any{}
			switch call.Method {
			case "initialize":
				result = map[string]any{"capabilities": map[string]any{}}
			case "thread/start":
				result = map[string]any{"thread": map[string]any{"id": "run-thread"}}
			case "thread/resume":
				result = map[string]any{"thread": map[string]any{"id": "run-thread"}}
			case "thread/goal/get":
				result = map[string]any{"goal": nil}
			}
			if err := conn.WriteJSON(map[string]any{"id": call.ID, "result": result}); err != nil {
				serverErr <- fmt.Errorf("handshake response: %w", err)
				return
			}
			if call.Method == "thread/goal/get" {
				close(protocolReady)
				break
			}
		}

		// The manager's next request is the large turn/start below. Consume only a
		// fragment of its actual WebSocket frame, then stop reading so the client
		// remains inside the kernel write until Run interrupts the transport.
		oneFrameFragment := make([]byte, 1024)
		if _, err := conn.UnderlyingConn().Read(oneFrameFragment); err != nil {
			serverErr <- fmt.Errorf("stalled frame read: %w", err)
			return
		}
		close(frameEntered)
		<-releaseServer
	}))
	t.Cleanup(func() {
		close(releaseServer)
		server.Close()
	})

	eng := newLifecycleTestEngine()
	d := New("", nil, time.Hour)
	d.authority = authority
	d.engine = eng
	listener := newLifecycleListener()
	d.listen = func(string, string) (net.Listener, func(), error) {
		return listener, func() {}, nil
	}
	d.launchSpec = func(context.Context, string) (panespec.LaunchSpec, error) {
		return panespec.LaunchSpec{}, errors.New("disabled in lifecycle test")
	}

	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	runDone := make(chan error, 1)
	go func() { runDone <- d.Run(runCtx) }()
	select {
	case <-d.firstPoll:
	case err := <-runDone:
		t.Fatalf("Run exited before manager initialization: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not initialize its manager")
	}

	endpoint := "ws" + strings.TrimPrefix(server.URL, "http")
	// This test covers transport interruption, not auth: the fake App Server
	// has no session record for central token delivery.
	d.codex.SetChatGPTAuth(nil)
	sup, err := d.codex.Ensure(context.Background(), "stalled", "", nil, []string{"sleep", "60"}, endpoint, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-protocolReady:
	case err := <-serverErr:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("structured handshake did not finish")
	}

	admissionDone := make(chan error, 1)
	if !d.startDeferredWork(func() {
		admissionDone <- d.admitDeferredEffect(context.Background(), "stalled", func(admitCtx context.Context) error {
			_, err := sup.BeginPrompt(admitCtx, strings.Repeat("x", 32<<20))
			return err
		})
	}) {
		t.Fatal("structured admission rejected before shutdown")
	}
	select {
	case <-frameEntered:
	case err := <-admissionDone:
		t.Fatalf("turn/start returned before transport interrupt: %v", err)
	case err := <-serverErr:
		t.Fatal(err)
	case <-time.After(3 * time.Second):
		t.Fatal("turn/start never entered stalled WebSocket write")
	}

	cancelRun()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run waited on a structured write instead of interrupting its transport")
	}
	select {
	case err := <-admissionDone:
		if err == nil {
			t.Fatal("interrupted turn/start succeeded")
		}
	default:
		t.Fatal("Run returned before its admitted structured caller")
	}
	if err := <-eng.shutdown; err != nil {
		t.Fatal(err)
	}
}

func TestRunStartupListenerFailureDrainsStartedOwners(t *testing.T) {
	isolateControl(t)
	authorityRoot := filepath.Join(t.TempDir(), "authority")
	authority, err := access.Open(authorityRoot)
	if err != nil {
		t.Fatal(err)
	}
	credentialDir, err := authority.EnsureHost(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	credential, err := access.LoadCredential(credentialDir)
	if err != nil {
		t.Fatal(err)
	}
	principal := credentialPrincipal(credential)

	eng := newLifecycleTestEngine()
	d := New("", nil, time.Hour)
	d.authority = authority
	d.engine = eng
	d.listen = func(string, string) (net.Listener, func(), error) {
		return nil, nil, errors.New("injected listener publication failure")
	}
	d.launchSpec = func(context.Context, string) (panespec.LaunchSpec, error) {
		return panespec.LaunchSpec{}, errors.New("disabled in lifecycle test")
	}
	eng.check = func() error {
		if d.sessionRPC == nil {
			return errors.New("session RPC was not constructed before listener failure")
		}
		d.sessionRPC.completions.mu.Lock()
		closed := d.sessionRPC.completions.closed
		d.sessionRPC.completions.mu.Unlock()
		if !closed {
			return errors.New("session RPC completion owner survived startup failure")
		}
		if err := d.authority.Valid(context.Background(), principal); err != nil {
			return fmt.Errorf("authority closed before engine startup cleanup: %w", err)
		}
		return nil
	}

	err = d.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "injected listener publication failure") {
		t.Fatalf("startup listener failure = %v", err)
	}
	if err := <-eng.shutdown; err != nil {
		t.Fatal(err)
	}
	reopened, err := access.Open(authorityRoot)
	if err != nil {
		t.Fatalf("authority ownership leaked after startup failure: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDeferredDrainRejectsNewAdmissionAndJoinsAcceptedWork(t *testing.T) {
	d := New("", nil, time.Hour)
	entered := make(chan struct{})
	release := make(chan struct{})
	if !d.startDeferredWork(func() {
		close(entered)
		<-release
	}) {
		t.Fatal("initial deferred work rejected")
	}
	<-entered
	d.stopDeferredAdmission()
	if d.startDeferredWork(func() {}) {
		t.Fatal("deferred work admitted after shutdown began")
	}
	drained := make(chan struct{})
	go func() {
		d.deferredWG.Wait()
		close(drained)
	}()
	select {
	case <-drained:
		t.Fatal("drain returned before accepted work finished")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("deferred drain deadlocked")
	}
}

// idleTestEngine is a lifecycle engine whose WaitIdle polls the daemon's real
// activity probe for one instance, as the local engine's wait-for-idle does.
type idleTestEngine struct {
	*lifecycleTestEngine
	key      engine.Key
	activity func(engine.Key) engine.Activity
	budget   time.Duration
	// during runs as the wait begins, standing in for the agent's hooks.
	during func()
	waited chan time.Duration
}

func (e *idleTestEngine) WaitIdle() {
	start := time.Now()
	go e.during()
	for e.activity(e.key) == engine.ActivityBusy && time.Since(start) < e.budget {
		time.Sleep(10 * time.Millisecond)
	}
	e.waited <- time.Since(start)
}

// A Claude agent's turn end reaches the daemon only as a hook report through
// its session mailbox. Shutdown must wait for mid-turn agents while reports
// still land, so a turn that ends during the wait ends the wait, instead of
// closing the mailboxes first and waiting out the whole grace.
func TestShutdownIdleWaitSeesTurnEndReports(t *testing.T) {
	isolateControl(t)
	const runtimeID = "77777777-7777-4777-8777-777777777777"
	session := store.Session{ID: "a1", RootID: "root", Agent: "claude", Dir: t.TempDir(), ClaudeID: runtimeID}
	d, _, principals := sessionRuntimeFixture(t, session)
	generation := publishTestRuntime(t, d, session.ID)
	if err := core.WriteSessionHookState(session.ID, runtimeID, core.StateRunning, session.Dir); err != nil {
		t.Fatal(err)
	}
	key := engine.Key{AgentID: session.ID, Tab: panespec.TabAgent}
	if got := d.instanceActivity(key); got != engine.ActivityBusy {
		t.Fatalf("activity before shutdown = %v, want busy", got)
	}

	const budget = 10 * time.Second
	eng := &idleTestEngine{
		lifecycleTestEngine: newLifecycleTestEngine(),
		key:                 key, activity: d.instanceActivity, budget: budget,
		waited: make(chan time.Duration, 1),
	}
	type outcome struct {
		report, other sessionrpc.DispatchResult
		closed        bool
	}
	during := make(chan outcome, 1)
	eng.during = func() {
		var o outcome
		o.report, _ = d.sessionRPC.dispatch(context.Background(), reportRequest(principals[session.ID], sessionreport.Activity, map[string]string{
			sessionreport.FieldRuntimeGeneration: generation,
			sessionreport.FieldState:             core.StateReady,
		}))
		o.other, _ = d.sessionRPC.dispatch(context.Background(), sessionrpc.DispatchRequest{
			Principal: principals[session.ID], RequestID: strings.Repeat("b", 32),
			Call: sessionrpc.Call{Kind: sessionrpc.CallOperation, Route: access.RouteAction, Verb: core.ActionRename, ID: session.ID, Fields: map[string]string{"name": "late"}},
		})
		d.sessionRPC.completions.mu.Lock()
		o.closed = d.sessionRPC.completions.closed
		d.sessionRPC.completions.mu.Unlock()
		during <- o
	}
	var waited time.Duration
	eng.check = func() error {
		select {
		case waited = <-eng.waited:
			return nil
		default:
			return errors.New("engine shutdown began before the wait-for-idle")
		}
	}
	pollSource := &lifecycleBlockingSource{entered: make(chan struct{}), exited: make(chan struct{})}
	d.sources = []source.Source{pollSource}
	d.engine = eng
	listener := newLifecycleListener()
	d.listen = func(string, string) (net.Listener, func(), error) {
		return listener, func() {}, nil
	}
	d.launchSpec = func(context.Context, string) (panespec.LaunchSpec, error) {
		return panespec.LaunchSpec{}, errors.New("disabled in lifecycle test")
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- d.Run(ctx) }()
	select {
	case <-pollSource.entered:
	case err := <-runDone:
		t.Fatalf("Run exited before polling: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not begin polling")
	}
	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run shutdown: %v", err)
		}
	case <-time.After(budget + 5*time.Second):
		t.Fatal("Run shutdown deadlocked")
	}
	if err := <-eng.shutdown; err != nil {
		t.Fatal(err)
	}
	o := <-during
	if o.report.Status != sessionrpc.StatusOK {
		t.Fatalf("turn-end report during the wait = %+v", o.report)
	}
	if o.other.Status != sessionrpc.StatusFailed || o.other.Code != "daemon_stopping" {
		t.Fatalf("non-report call during the wait = %+v, want daemon_stopping", o.other)
	}
	if o.closed {
		t.Fatal("session runtime closed before the wait-for-idle")
	}
	if waited > budget/4 {
		t.Fatalf("wait-for-idle took %v after the turn ended; budget %v", waited, budget)
	}
}
