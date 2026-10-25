package codexapp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type heldLogWriter struct {
	entered     chan struct{}
	release     chan struct{}
	enteredOnce sync.Once
	releaseOnce sync.Once
}

func newHeldLogWriter() *heldLogWriter {
	return &heldLogWriter{entered: make(chan struct{}), release: make(chan struct{})}
}

func (w *heldLogWriter) Write(p []byte) (int, error) {
	w.enteredOnce.Do(func() { close(w.entered) })
	<-w.release
	return len(p), nil
}

func (w *heldLogWriter) Close() error {
	w.releaseOnce.Do(func() { close(w.release) })
	return nil
}

func TestManagerEnsureQueuedCreationHonorsAdmissionContext(t *testing.T) {
	m := NewManager(context.Background(), "")
	gate := m.startGate("queued")
	<-gate
	t.Cleanup(func() { gate <- struct{}{} })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := m.Ensure(ctx, "queued", "", nil, nil, "unix:///unused", "", "", "")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued Ensure error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("queued Ensure ignored admission deadline: %v", elapsed)
	}
}

func TestManagerEnsureRejectsCancelledLifetimeBeforeStart(t *testing.T) {
	lifetime, cancel := context.WithCancel(context.Background())
	cancel()
	m := NewManager(lifetime, "")
	_, err := m.Ensure(context.Background(), "stopped", "", nil, nil, "unix:///unused", "", "", "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Ensure after manager cancellation = %v", err)
	}
}

func TestManagerInterruptsInProgressTransportAndRejectsLaterEnsure(t *testing.T) {
	lifetime, cancelLifetime := context.WithCancel(context.Background())
	defer cancelLifetime()

	upgraded := make(chan struct{})
	releaseServer := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		close(upgraded)
		// Do not read or answer initialize. Ensure is now blocked in a real RPC
		// handshake over this manager-owned transport.
		<-releaseServer
	}))
	t.Cleanup(func() {
		close(releaseServer)
		server.Close()
	})

	m := NewManager(lifetime, "")
	defer m.Shutdown()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http")
	ensureDone := make(chan error, 1)
	go func() {
		_, err := m.Ensure(context.Background(), "starting", "", nil, []string{"sleep", "60"}, endpoint, "", "", "")
		ensureDone <- err
	}()

	select {
	case <-upgraded:
	case err := <-ensureDone:
		t.Fatalf("Ensure returned before interrupt: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("Ensure never attached its transport")
	}

	m.InterruptTransports()
	select {
	case err := <-ensureDone:
		if err == nil {
			t.Fatal("interrupted Ensure succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("transport interrupt did not release in-progress Ensure")
	}

	if _, err := m.Ensure(context.Background(), "after-drain", "", nil, nil, "unix:///unused", "", "", ""); !errors.Is(err, errManagerStopping) {
		t.Fatalf("Ensure after transport drain = %v, want manager stopping", err)
	}
}

// Concurrent starts (creation and native attach) submit once; a daemon restart
// resumes the persisted identity without replaying the creation prompt.
func TestManagerInitialPromptOnce(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	servers := make(chan *fakeServer, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		fs := &fakeServer{t: t, conn: &wsConn{c: conn}, respByID: map[string]chan incoming{}}
		defer fs.close()
		servers <- fs
		fs.loop()
	}))
	defer server.Close()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http")
	m := NewManager(ctx, "")
	defer m.Shutdown()
	// The in-process fake owns the socket; this child exercises process lifetime
	// without invoking a real model. Shutdown kills it immediately.
	ensure := func(m *Manager) (*Supervisor, error) {
		return m.Ensure(context.Background(), "initial", "", nil, []string{"sleep", "60"}, endpoint, "gpt-5.6-sol", "fix it", "")
	}
	errs := make(chan error, 8)
	for i := 0; i < cap(errs); i++ {
		go func() { _, err := ensure(m); errs <- err }()
	}
	for i := 0; i < cap(errs); i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	fs := <-servers
	fs.mu.Lock()
	turns := 0
	for _, call := range fs.calls {
		if call.Method == "turn/start" {
			turns++
		}
	}
	fs.mu.Unlock()
	if turns != 1 {
		t.Fatalf("concurrent starts submitted %d prompts, want 1", turns)
	}
	id, ok := LoadIdentity("initial")
	if !ok || !id.Resumable {
		t.Fatalf("missing resumable identity: %+v", id)
	}
	m.Shutdown()
	restarted := NewManager(ctx, "")
	defer restarted.Shutdown()
	sup, err := ensure(restarted)
	if err != nil {
		t.Fatal(err)
	}
	if sup.ThreadID() != id.ThreadID {
		t.Fatal("restart changed the thread")
	}
	resumed := <-servers
	if _, ok := resumed.sawCall("turn/start"); ok {
		t.Fatal("restart replayed the initial prompt")
	}
	if _, ok := resumed.sawCall("thread/start"); ok {
		t.Fatal("restart created a fresh thread")
	}
}

func TestManagerReconnectsTransportWithoutReplacingLiveProcess(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	servers := make(chan *fakeServer, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		fs := &fakeServer{t: t, conn: &wsConn{c: conn}, respByID: map[string]chan incoming{}}
		defer fs.close()
		servers <- fs
		fs.loop()
	}))
	defer server.Close()

	m := NewManager(context.Background(), "")
	defer m.Shutdown()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http")
	ensure := func() (*Supervisor, error) {
		return m.Ensure(context.Background(), "reconnect", "", nil, []string{"sleep", "60"}, endpoint, "gpt-main", "", "")
	}
	sup, err := ensure()
	if err != nil {
		t.Fatal(err)
	}
	first := <-servers
	sup.mu.Lock()
	pid := sup.proc.Process.Pid
	sup.mu.Unlock()

	first.close() // daemon transport only; the synthetic App Server process lives
	deadline := time.Now().Add(time.Second)
	for sup.live() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if sup.live() {
		t.Fatal("first transport did not close")
	}
	streamAcrossReconnect := sup.Subscribe(context.Background(), sup.hub.lastSeq())
	select {
	case _, ok := <-streamAcrossReconnect:
		if !ok {
			t.Fatal("transport loss closed the event hub while App Server remained live")
		}
	default:
	}

	reconnected, err := ensure()
	if err != nil {
		t.Fatalf("Ensure after transport loss: %v", err)
	}
	if reconnected != sup {
		t.Fatal("transport recovery replaced the supervisor")
	}
	sup.mu.Lock()
	gotPID := sup.proc.Process.Pid
	gotModel := sup.cfg.Model
	sup.mu.Unlock()
	if gotPID != pid {
		t.Fatalf("transport recovery replaced App Server pid %d with %d", pid, gotPID)
	}
	if gotModel != "" {
		t.Fatalf("transport recovery retained stale launch model %q", gotModel)
	}
	second := <-servers
	resume, ok := second.sawCall("thread/resume")
	if !ok {
		t.Fatal("reconnect did not resume the pinned thread")
	}
	var params map[string]any
	if err := json.Unmarshal(resume.Params, &params); err != nil {
		t.Fatal(err)
	}
	if got := params["threadId"]; got != "thr_1" {
		t.Fatalf("reconnect thread = %v, want thr_1", got)
	}
	if _, ok := params["model"]; ok {
		t.Fatal("reconnect reapplied the launch-time model over the native thread")
	}

	// The opposite boundary remains strict: actual process exit (not transport
	// loss) closes the serving stream without waiting for another manager poll.
	stream := sup.Subscribe(context.Background(), sup.hub.lastSeq())
	if err := sup.proc.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case _, ok := <-stream:
		if ok {
			t.Fatal("received an event after App Server process exit")
		}
	case <-time.After(time.Second):
		t.Fatal("App Server process exit did not close the serving stream")
	}
}

func TestManagerProcessExitAfterRetiredTransportClosesServingLifetime(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	connected := make(chan *fakeServer, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		fs := &fakeServer{t: t, conn: &wsConn{c: conn}, respByID: map[string]chan incoming{}}
		defer fs.close()
		connected <- fs
		fs.loop()
	}))
	defer server.Close()

	m := NewManager(context.Background(), "")
	defer m.Shutdown()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http")
	sup, err := m.Ensure(context.Background(), "exit-after-loss", "", nil, []string{"sleep", "60"}, endpoint, "gpt-main", "", "")
	if err != nil {
		t.Fatal(err)
	}
	fs := <-connected

	// Open both serving resources before loss. The control reader must retire
	// while the child is still alive, leaving them available for a reconnect.
	sup.emit(notice("info", "open durable log"))
	sup.logMu.Lock()
	logOpen := sup.logW != nil
	sup.logMu.Unlock()
	if !logOpen {
		t.Fatal("event log did not open")
	}
	sup.mu.Lock()
	readerDone := sup.rpcDone
	proc := sup.proc.Process
	sup.mu.Unlock()
	fs.close()
	select {
	case <-readerDone:
	case <-time.After(time.Second):
		t.Fatal("transport reader did not retire before process exit")
	}
	if !sup.processRunning() {
		t.Fatal("synthetic App Server exited with its control transport")
	}
	stream := sup.Subscribe(context.Background(), sup.hub.lastSeq())
	select {
	case _, ok := <-stream:
		if !ok {
			t.Fatal("transport loss closed serving lifetime before process exit")
		}
	default:
	}

	// No Ensure/reconnect follows. Real process exit must independently retire
	// the hub and log even though closing rpc is now an idempotent no-op and no
	// read loop remains to perform teardown.
	if err := proc.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case _, ok := <-stream:
		if ok {
			t.Fatal("received an event after App Server process exit")
		}
	case <-time.After(time.Second):
		t.Fatal("process exit after retired transport left subscriber open")
	}
	deadline := time.Now().Add(time.Second)
	for {
		sup.logMu.Lock()
		logClosed := sup.logW == nil
		sup.logMu.Unlock()
		if logClosed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("process exit after retired transport left event log open")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestManagerReconnectJoinsOldReaderBeforePublishingReplacement(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	connected := make(chan *fakeServer, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		fs := &fakeServer{t: t, conn: &wsConn{c: conn}, respByID: map[string]chan incoming{}}
		defer fs.close()
		connected <- fs
		fs.loop()
	}))
	defer server.Close()

	m := NewManager(context.Background(), "")
	defer m.Shutdown()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http")
	ensure := func(ctx context.Context) (*Supervisor, error) {
		return m.Ensure(ctx, "reader-join", "", nil, []string{"sleep", "60"}, endpoint, "gpt-main", "", "")
	}
	sup, err := ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	oldServer := <-connected

	// Hold an old connection callback inside emit. Its default request response is
	// written only after the hold is released, so publishing a replacement early
	// would both overlap read-loop-owned state and route this old id through s.rpc
	// onto the replacement socket.
	held := newHeldLogWriter()
	sup.logMu.Lock()
	sup.logW = held
	sup.logMu.Unlock()
	oldServer.write(map[string]any{
		"method": "test/unsupported",
		"id":     "old-request",
		"params": map[string]any{},
	})
	select {
	case <-held.entered:
	case <-time.After(time.Second):
		t.Fatal("old request handler did not reach the hold")
	}
	sup.mu.Lock()
	oldRPC := sup.rpc
	oldReaderDone := sup.rpcDone
	sup.mu.Unlock()
	if err := oldRPC.close(); err != nil {
		t.Fatal(err)
	}
	shortCtx, cancelShort := context.WithTimeout(context.Background(), 20*time.Millisecond)
	_, err = ensure(shortCtx)
	cancelShort()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reconnect reader join ignored admission deadline: %v", err)
	}
	select {
	case <-connected:
		t.Fatal("timed-out reconnect published a replacement before old reader retired")
	default:
	}

	reconnectCtx, cancelReconnect := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelReconnect()
	reconnected := make(chan error, 1)
	go func() {
		_, err := ensure(reconnectCtx)
		reconnected <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		sup.mu.Lock()
		joining := sup.reconnecting
		sup.mu.Unlock()
		if joining {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("reconnect did not begin")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-oldReaderDone:
		t.Fatal("old reader retired while its handler was held")
	default:
	}
	select {
	case <-connected:
		t.Fatal("replacement connected before old reader handler retired")
	case <-time.After(50 * time.Millisecond):
	}

	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-oldReaderDone:
	case <-time.After(time.Second):
		t.Fatal("old reader did not retire after handler release")
	}
	var replacement *fakeServer
	select {
	case replacement = <-connected:
	case <-time.After(time.Second):
		t.Fatal("replacement did not connect after old reader retired")
	}
	select {
	case err := <-reconnected:
		if err != nil {
			t.Fatalf("reconnect: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("reconnect did not complete")
	}

	wantID, _ := json.Marshal("old-request")
	replacement.mu.Lock()
	defer replacement.mu.Unlock()
	for _, call := range replacement.calls {
		if call.Method == "" && string(call.ID) == string(wantID) {
			t.Fatal("old request response was written on replacement connection")
		}
	}
}

// TestManagerEmptyLookups checks the no-supervisor paths: Get misses, Live is
// empty, and Close is a harmless no-op. These are the states the daemon hits on
// every steer/poll for a session that is not (or not yet) structured.
func TestManagerEmptyLookups(t *testing.T) {
	m := NewManager(context.Background(), "")
	if _, ok := m.Get("nope"); ok {
		t.Fatal("Get returned a supervisor for an unknown id")
	}
	if len(m.Live()) != 0 {
		t.Fatal("Live non-empty on a fresh manager")
	}
	m.Close("nope") // must not panic
	m.Shutdown()    // must not panic
	m.Close("nope") // idempotent after shutdown
}

func TestManagerTransportLossDoesNotCloseSupervisorWithPendingTool(t *testing.T) {
	m := NewManager(context.Background(), "")
	sup, fs, client := newFakePair(t)
	attach(t, sup, client)
	m.mu.Lock()
	m.sup["disconnected"] = sup
	m.mu.Unlock()
	defer m.Shutdown()

	if _, ok := m.Get("disconnected"); !ok {
		t.Fatal("attached supervisor was not live")
	}
	col := subscribeCollector(context.Background(), sup)
	fs.pushNotify("item/started", map[string]any{
		"threadId": "thr_1", "turnId": "turn_1",
		"item": map[string]any{"id": "tool_1", "type": "commandExecution", "command": "synthetic"},
	})
	if ev := col.waitFor(t, "tool_call"); ev.ItemID != "tool_1" {
		t.Fatalf("pending tool event = %+v", ev)
	}
	fs.close() // no item/completed: the tool remains outstanding in history

	deadline := time.Now().Add(time.Second)
	for {
		if _, ok := m.Get("disconnected"); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("dead transport remained published as a live supervisor")
		}
		time.Sleep(time.Millisecond)
	}
	if m.Live()["disconnected"] {
		t.Fatal("dead transport with a pending tool remained in liveness snapshot")
	}
	sup.mu.Lock()
	closed := sup.closed
	sup.mu.Unlock()
	if closed {
		t.Fatal("transport loss closed the supervisor and would kill native App Server work")
	}
}

func TestManagerCloseSerializesWithReplacementStart(t *testing.T) {
	m := NewManager(context.Background(), "")
	gate := m.startGate("same-session")
	<-gate // model an in-progress replacement start owning the endpoint

	closed := make(chan struct{})
	go func() {
		m.Close("same-session")
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("Close bypassed the per-session start gate; socket cleanup can race replacement bind")
	case <-time.After(20 * time.Millisecond):
	}
	gate <- struct{}{}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not proceed after replacement gate released")
	}
}

// TestManagerForgetRemovesIdentity checks that Forget drops the persisted identity
// (session deleted for good), while Close alone leaves it (archive → resumable).
func TestManagerForgetRemovesIdentity(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	if err := SaveIdentity(Identity{SessionID: "s1", ThreadID: "t1", ControlMode: "structured"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	m := NewManager(context.Background(), "")

	// Close on an unmanaged id leaves the identity in place.
	m.Close("s1")
	if _, ok := LoadIdentity("s1"); !ok {
		t.Fatal("Close removed the persisted identity; it should survive for resume")
	}

	// Forget removes it.
	m.Forget("s1")
	if _, ok := LoadIdentity("s1"); ok {
		t.Fatal("Forget left the persisted identity behind")
	}
}

// TestResumeThreadForGating checks the AGE-198 fix: a session is resumed ONLY once
// its thread has run a turn (Resumable), so a pinned-but-never-run thread is never
// resumed (which would 'no rollout found' and poison its first turn) — it starts
// fresh instead.
func TestResumeThreadForGating(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// Current identity, not resumable (fresh thread, no turn) ⇒ do not resume.
	if err := SaveIdentity(Identity{SessionID: "g", ThreadID: "thr-1", Resumable: false, Version: identityVersion}); err != nil {
		t.Fatal(err)
	}
	if got := resumeThreadFor("g", "pty-thread"); got != "" {
		t.Fatalf("resumeThreadFor(current, non-resumable) = %q, want empty", got)
	}

	// Resumable (ran a turn, or was previously resumed) ⇒ resume that thread.
	if err := SaveIdentity(Identity{SessionID: "g", ThreadID: "thr-1", Resumable: true, Version: identityVersion}); err != nil {
		t.Fatal(err)
	}
	if got := resumeThreadFor("g", "pty-thread"); got != "thr-1" {
		t.Fatalf("resumeThreadFor(resumable) = %q, want thr-1", got)
	}

	// LEGACY identity (persisted before the Resumable field, Version 0) with a
	// thread id ⇒ still attempt resume — don't silently discard a real conversation
	// on upgrade (the handshake fallback covers a genuine miss).
	if err := SaveIdentity(Identity{SessionID: "legacy", ThreadID: "old-thr", Resumable: false, Version: 0}); err != nil {
		t.Fatal(err)
	}
	if got := resumeThreadFor("legacy", "pty-thread"); got != "old-thr" {
		t.Fatalf("resumeThreadFor(legacy) = %q, want old-thr (must not discard a legacy conversation)", got)
	}

	// No structured identity ⇒ adopt the PTY path's persisted conversation.
	if got := resumeThreadFor("unknown", "pty-thread"); got != "pty-thread" {
		t.Fatalf("resumeThreadFor(unknown, PTY pin) = %q, want pty-thread", got)
	}
}

// TestManagerAdoptsPTYConversation covers a control-mode migration: Codex first
// ran in a native PTY, so the store has its real rollout id but no structured
// identity sidecar. The first App Server launch must resume that rollout and must
// not replay the session's creation prompt.
func TestManagerAdoptsPTYConversation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	connected := make(chan *fakeServer, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		fs := &fakeServer{t: t, conn: &wsConn{c: conn}, respByID: map[string]chan incoming{}}
		defer fs.close()
		connected <- fs
		fs.loop()
	}))
	defer server.Close()

	m := NewManager(ctx, "")
	defer m.Shutdown()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http")
	sup, err := m.Ensure(context.Background(), "migrating", "", nil, []string{"sleep", "60"}, endpoint, "gpt-5.6-sol", "original prompt", "pty-thread")
	if err != nil {
		t.Fatal(err)
	}
	if sup.ThreadID() != "pty-thread" {
		t.Fatalf("structured launch changed PTY thread: got %q", sup.ThreadID())
	}
	fs := <-connected
	resume, ok := fs.sawCall("thread/resume")
	if !ok || !strings.Contains(string(resume.Params), `"threadId":"pty-thread"`) {
		t.Fatalf("structured launch did not resume PTY thread: %+v", resume)
	}
	if _, ok := fs.sawCall("thread/start"); ok {
		t.Fatal("structured launch started a replacement thread")
	}
	if _, ok := fs.sawCall("turn/start"); ok {
		t.Fatal("structured launch replayed the creation prompt")
	}
	id, ok := LoadIdentity("migrating")
	if !ok || id.ThreadID != "pty-thread" || !id.Resumable {
		t.Fatalf("PTY adoption was not persisted: %+v", id)
	}
}
