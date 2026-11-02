package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"amux/internal/access"
	"amux/internal/core"
	"amux/internal/panespec"
	"amux/internal/sessionrpc"
	"amux/internal/source"
	"amux/internal/store"

	"github.com/gorilla/websocket"
)

// newStuckTurnRPCServer is a fake Codex App Server that accepts turn/start and
// announces turn/started but never sends turn/completed: the agent stays
// mid-turn for as long as the test runs.
func newStuckTurnRPCServer(t *testing.T) (string, <-chan struct{}) {
	t.Helper()
	turnStarted := make(chan struct{}, 4)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		go func() { <-release; _ = conn.Close() }()
		const threadID = "stuck-thread"
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var call struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if json.Unmarshal(raw, &call) != nil || len(call.ID) == 0 || string(call.ID) == "null" {
				continue
			}
			var result any = map[string]any{}
			switch call.Method {
			case "initialize":
				result = map[string]any{"capabilities": map[string]any{}}
			case "thread/start", "thread/resume":
				result = map[string]any{"thread": map[string]any{"id": threadID}}
			case "thread/goal/get":
				result = map[string]any{"goal": nil}
			case "turn/start":
				result = map[string]any{"turn": map[string]any{"id": "stuck-turn"}}
			}
			if err := conn.WriteJSON(map[string]any{"id": call.ID, "result": result}); err != nil {
				return
			}
			if call.Method == "turn/start" {
				_ = conn.WriteJSON(map[string]any{"method": "turn/started", "params": map[string]any{
					"threadId": threadID, "turn": map[string]any{"id": "stuck-turn"},
				}})
				turnStarted <- struct{}{}
			}
		}
	}))
	t.Cleanup(func() {
		close(release)
		server.Close()
	})
	return "ws" + strings.TrimPrefix(server.URL, "http"), turnStarted
}

// TestRunShutdownMidTurnRPCPromptReleasesLock is the regression for the
// 2026-11-01 restart wedge: a Codex structured prompt steered through the
// session RPC mailbox is ACKed and continues as deferred work bound to the
// session RPC serving lifetime. Shutdown joined that deferred work before it
// cancelled the serving lifetime, and it had already interrupted the transport
// that could deliver turn/completed, so Run never returned and the singleton
// lock stayed held.
func TestRunShutdownMidTurnRPCPromptReleasesLock(t *testing.T) {
	isolateControl(t)
	root := store.Session{ID: "wedge-root", Scope: store.ScopeWork, Agent: "codex", Dir: t.TempDir()}
	member := store.Session{ID: "wedge-member", RootID: root.ID, Agent: "codex", Dir: t.TempDir()}
	d, _, _ := sessionRuntimeFixture(t, root, member)
	grants := map[string]access.SessionAccess{}
	for _, s := range []store.Session{root, member} {
		grant, err := d.authority.EnsureSession(t.Context(), s.ID, s.Dir)
		if err != nil {
			t.Fatal(err)
		}
		grants[s.ID] = grant
	}
	d.launchSpec = func(_ context.Context, id string) (panespec.LaunchSpec, error) {
		for _, s := range []store.Session{root, member} {
			if s.ID == id {
				return panespec.LaunchSpec{Session: s, Access: grants[id]}, nil
			}
		}
		return panespec.LaunchSpec{}, errors.New("no mailbox in shutdown test")
	}
	eng := newLifecycleTestEngine()
	d.engine = eng
	pollSource := &lifecycleBlockingSource{entered: make(chan struct{}), exited: make(chan struct{})}
	d.sources = []source.Source{pollSource}
	listener := newLifecycleListener()
	d.listen = func(string, string) (net.Listener, func(), error) {
		return listener, func() {}, nil
	}
	d.steerStarted = make(chan string, 1)
	// The per-join watchdog must not be what ends this shutdown.
	d.shutdownJoinTimeout = time.Minute

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- d.Run(ctx) }()
	select {
	case <-pollSource.entered:
	case err := <-runDone:
		t.Fatalf("Run exited before polling: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not begin polling")
	}

	endpoint, turnStarted := newStuckTurnRPCServer(t)
	d.codex.SetChatGPTAuth(nil)
	if _, err := d.codex.Ensure(t.Context(), member.ID, member.Dir, nil,
		[]string{"sleep", "60"}, endpoint, "", "", ""); err != nil {
		t.Fatal(err)
	}

	// Two prompts through the mailbox: the second supersedes the first while it
	// is mid-turn, so the first waiter is no longer the supervisor's turnDone and
	// neither turn/completed nor transport loss can release it — only its
	// deferred context can.
	for i, text := range []string{"work until shutdown", "and then this"} {
		call := sessionrpc.Call{
			Kind: sessionrpc.CallOperation, Route: access.RouteAction,
			Verb: core.ActionSteer, ID: member.ID,
			Fields: map[string]string{core.SteerVerb: core.SteerPrompt, core.SteerText: text},
		}
		requestID := publishSignedMailboxCall(t, d.authority, grants[root.ID], call)
		select {
		case <-turnStarted:
		case err := <-runDone:
			t.Fatalf("Run exited before turn %d started: %v", i, err)
		case <-time.After(10 * time.Second):
			t.Fatalf("session RPC prompt %d never reached turn/start", i)
		}
		if response := readMailboxResponse(t, grants[root.ID], requestID); response.Status != sessionrpc.StatusOK {
			t.Fatalf("prompt %d response = %+v", i, response)
		}
	}

	// The agent is mid-turn. Stop the daemon.
	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run shutdown deadlocked on a mid-turn session RPC prompt")
	}
	lock, err := acquireSingletonLock()
	if err != nil {
		t.Fatalf("singleton lock still held after Run returned: %v", err)
	}
	releaseSingletonLock(lock)
}

// startLifecycleRun starts Run with a lifecycle engine and listener and waits
// for its first poll, returning the context cancel and the Run result.
func startLifecycleRun(t *testing.T, d *Daemon) (context.CancelFunc, <-chan error) {
	t.Helper()
	d.engine = newLifecycleTestEngine()
	pollSource := &lifecycleBlockingSource{entered: make(chan struct{}), exited: make(chan struct{})}
	d.sources = []source.Source{pollSource}
	listener := newLifecycleListener()
	d.listen = func(string, string) (net.Listener, func(), error) {
		return listener, func() {}, nil
	}
	d.launchSpec = func(context.Context, string) (panespec.LaunchSpec, error) {
		return panespec.LaunchSpec{}, errors.New("disabled in lifecycle test")
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	runDone := make(chan error, 1)
	go func() { runDone <- d.Run(ctx) }()
	select {
	case <-pollSource.entered:
	case err := <-runDone:
		t.Fatalf("Run exited before polling: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not begin polling")
	}
	return cancel, runDone
}

func awaitRunReleasesLock(t *testing.T, runDone <-chan error, bound time.Duration) {
	t.Helper()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run shutdown: %v", err)
		}
	case <-time.After(bound):
		t.Fatalf("Run did not return within %v", bound)
	}
	lock, err := acquireSingletonLock()
	if err != nil {
		t.Fatalf("singleton lock still held after Run returned: %v", err)
	}
	releaseSingletonLock(lock)
}

// TestRunCancelsDeferredWorkBoundToServingLifetime isolates the daemon side of
// the wedge from any particular runtime: deferred work ACKed by a session RPC
// callback is bound to the serving lifetime, which shutdown ends only after it
// joins deferred work. Shutdown must cancel that work itself before the join.
func TestRunCancelsDeferredWorkBoundToServingLifetime(t *testing.T) {
	isolateControl(t)
	authority, err := access.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d := New("", nil, time.Hour)
	d.authority = authority
	d.shutdownJoinTimeout = time.Minute
	cancel, runDone := startLifecycleRun(t, d)

	// Stands in for rpcCtx: outlives the whole deferred join.
	serving, stopServing := context.WithCancel(context.Background())
	defer stopServing()
	callback, finish := context.WithTimeout(serving, time.Second)
	callback = withDeferredLifetime(callback, serving)
	entered := make(chan struct{})
	if !d.startDeferredContext(callback, func(workCtx context.Context) {
		close(entered)
		<-workCtx.Done() // a turn waiter nothing else will ever release
	}) {
		t.Fatal("deferred work rejected before shutdown")
	}
	finish() // the callback returned its ACK
	<-entered

	cancel()
	awaitRunReleasesLock(t, runDone, 10*time.Second)
}

// syncBuffer is a log sink safe for concurrent writers.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// TestRunShutdownJoinTimeoutReleasesLock: a join that never finishes (here,
// deferred work that ignores its context) is logged by name with a goroutine
// dump, and teardown still releases the singleton lock.
func TestRunShutdownJoinTimeoutReleasesLock(t *testing.T) {
	isolateControl(t)
	var logs syncBuffer
	prev := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prev) })

	authority, err := access.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d := New("", nil, time.Hour)
	d.authority = authority
	d.shutdownJoinTimeout = 200 * time.Millisecond
	cancel, runDone := startLifecycleRun(t, d)

	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	if !d.startDeferredWork(func() { <-stuck }) {
		t.Fatal("deferred work rejected before shutdown")
	}
	cancel()
	awaitRunReleasesLock(t, runDone, 10*time.Second)
	out := logs.String()
	if !strings.Contains(out, "deferred actions did not finish within") {
		t.Fatalf("expired join not named in the log:\n%s", out)
	}
	if !strings.Contains(out, "goroutine dump (shutdown join deferred actions expired)") {
		t.Fatalf("expired join logged no goroutine dump:\n%s", out)
	}
}

func TestSingletonHolderNamesPidAndAge(t *testing.T) {
	isolateControl(t)
	if err := os.MkdirAll(core.StateDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireSingletonLock()
	if err != nil {
		t.Fatal(err)
	}
	defer releaseSingletonLock(lock)
	_, err = acquireSingletonLock()
	if err == nil {
		t.Fatal("second acquire succeeded")
	}
	want := fmt.Sprintf("held by pid %d, started ", os.Getpid())
	if !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), " ago)") {
		t.Fatalf("contention error %q does not name holder (%q)", err, want)
	}
	now := time.Date(2026, 11, 1, 18, 44, 0, 0, time.UTC)
	if got := describeSingletonHolder("pid=42 started=2026-11-01T18:36:00Z\n", now); got != "held by pid 42, started 2026-11-01T18:36:00Z (8m0s ago)" {
		t.Fatalf("describe = %q", got)
	}
	if got := describeSingletonHolder("", now); !strings.HasPrefix(got, "holder unknown") {
		t.Fatalf("describe empty = %q", got)
	}
}
