package codexapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// supervisor.go owns one background `codex app-server` and drives it as a
// JSON-RPC client. The App Server's lifetime is the supervisor's — bound to the
// context Start is given (the daemon's lifetime), NEVER a UI pane or a client
// connection — so closing the native Codex TUI or a remote client cannot stop the
// server or an in-flight turn (AGE-181 acceptance: "no process killed by client
// disconnect"). Only Close (or daemon shutdown) terminates it.
//
// Two entry points separate the process concern from the protocol concern so the
// protocol is unit-testable without a real binary:
//
//   - Start(ctx, wrappedArgv): production. Launches `codex app-server --listen
//     <endpoint>` (under the daemon's sandbox-wrapped argv), dials the endpoint's
//     WebSocket, and runs the handshake. The process is in its own group so a
//     signal aimed at the pane never reaches it, and ctx cancellation kills it.
//   - attach(ctx, msgConn): the protocol core over a message-framed transport —
//     exercised in tests against an in-memory fake App Server.

// Thread/start policy. The values are the App Server's HYPHENATED enums, verified
// against Codex 0.153.4 by the ROOT probe: `onRequest`/`workspaceWrite` are
// rejected -32600. on-request so the server actually raises answerable approvals,
// confined to workspace-write, never danger-full-access. The process-level sandbox
// is amux's own launcher (panespec); these are the thread-level request defaults.
const (
	defaultApprovalPolicy = "on-request"
	defaultSandbox        = "workspace-write"
	defaultDialTimeout    = 15 * time.Second
)

// Config parameterizes one supervised App Server. SessionID ties it to the amux
// session (identity persistence); Endpoint is the WebSocket listen/dial address
// (unix://<per-session socket> by default, inside the session's private scope).
type Config struct {
	SessionID string
	Bin       string   // codex binary; "" ⇒ "codex" (resolved by the caller / PATH)
	Dir       string   // working directory (the worktree)
	Env       []string // extra KEY=VALUE additions to the child environment
	// Endpoint is the App Server WebSocket endpoint: unix://<path> (default,
	// per-session, sandbox-scoped), ws://127.0.0.1:<port> (loopback), or
	// wss://host:port (cross-machine, authenticated). amux launches the server with
	// --listen <Endpoint> and dials the same value.
	Endpoint       string
	ResumeThreadID string        // non-empty ⇒ thread/resume instead of thread/start
	ApprovalPolicy string        // "" ⇒ defaultApprovalPolicy
	Sandbox        string        // "" ⇒ defaultSandbox
	DialTimeout    time.Duration // "" ⇒ defaultDialTimeout
	// Origin, when set, is sent as the WebSocket Origin header. Default empty ⇒ no
	// Origin (codex's loopback/wss listeners 403 any Origin). Set only for a server
	// deployment that allowlists a specific Origin.
	Origin string
	// EventLogPath, when set, is the per-session NDJSON record the supervisor
	// appends every emitted runtime event to (one marshaled harnessproto.RuntimeEvent
	// per line). It is the durable transport the out-of-process provider tails via
	// the existing runtime-events reader (docs/codex-app-server-supervision.md): the
	// provider cannot reach the daemon's in-memory hub, so the file is the bridge.
	// Empty ⇒ events go only to the in-memory hub (tests, in-daemon consumers).
	EventLogPath string
}

// Supervisor is a running (or resumable) App Server plus the amux-side client.
type Supervisor struct {
	cfg       Config
	rpc       *rpcConn
	hub       *eventHub
	approvals *approvalTracker
	state     *streamState // owned by the read loop only

	mu        sync.Mutex
	proc      *exec.Cmd
	closed    bool
	threadID  string
	curTurn   string
	turnDone  chan *turnResult
	runCancel context.CancelFunc

	logMu  sync.Mutex
	logW   io.WriteCloser // EventLogPath sink, opened lazily on first emit
	logErr bool           // a prior log write failed; stop retrying (never fatal)

	resumable bool // set once a turn has started (a rollout now exists → resume is safe)
}

// New builds a supervisor from cfg. It does not start anything — call Start (or
// attach in tests).
func New(cfg Config) *Supervisor {
	if cfg.ApprovalPolicy == "" {
		cfg.ApprovalPolicy = defaultApprovalPolicy
	}
	if cfg.Sandbox == "" {
		cfg.Sandbox = defaultSandbox
	}
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = defaultDialTimeout
	}
	if cfg.Bin == "" {
		cfg.Bin = "codex"
	}
	return &Supervisor{
		cfg:       cfg,
		hub:       newEventHub(),
		approvals: newApprovalTracker(),
		state:     &streamState{},
	}
}

// AppServerArgv is the inner argv amux launches to run the background App Server:
// `codex app-server --listen <endpoint>` (docs: `--listen ws://…`; the endpoint may
// also be `unix://…`). It is the command *before* the sandbox wrapper — the daemon
// wraps it with the amux launcher (panespec) so it runs in the session's scope.
// The endpoint is exactly what a native `--remote` peer points at.
func AppServerArgv(bin, endpoint string) []string {
	if bin == "" {
		bin = "codex"
	}
	return []string{bin, "app-server", "--listen", endpoint}
}

// AttachArgv is the argv a native Codex CLI (in an amux pane) uses to attach to
// the SAME supervised server/thread — the whole point of AGE-181: the terminal UI
// and the web bridge drive one server/thread, not separate processes.
//
// UNVALIDATED ON HOST: that `codex --remote <endpoint> resume <thread-id>` attaches
// to the running server/thread (rather than starting its own process) MUST be
// confirmed on a host with the pinned codex; and `thread/resume` before the first
// turn returns "no rollout found" (ROOT probe), so fresh-session attach needs the
// create-first-turn flow, not a bare resume. This builder encodes the documented
// syntax; the caller must not claim live attachment until a host run confirms it.
func AttachArgv(bin, endpoint, threadID string) []string {
	if bin == "" {
		bin = "codex"
	}
	argv := []string{bin, "--remote", endpoint}
	if threadID != "" {
		argv = append(argv, "resume", threadID)
	}
	return argv
}

// Identity is the durable server/thread identity amux persists for a structured
// session so the supervisor, a native CLI, and a reconnecting daemon all refer to
// one server/thread (AGE-181). It is amux-internal — never published on the wire
// (the socket lives in the private sandbox scope).
type Identity struct {
	SessionID   string `json:"sessionId"`
	Endpoint    string `json:"endpoint"`
	ThreadID    string `json:"threadId"`
	ControlMode string `json:"controlMode"`
	// Resumable is true once the thread has a rollout on disk — set when the thread
	// runs its first turn, or immediately when it is (re)attached by a successful
	// `thread/resume` (a resumed thread necessarily has history). Until then a resume
	// would return "no rollout found" (ROOT/AGE-198), so a reconnect must NOT attempt
	// it — it starts a fresh thread instead. This is the primary guard against a
	// pre-turn failed resume poisoning the first turn; the handshake keeps an error
	// fallback as a backstop.
	Resumable bool `json:"resumable,omitempty"`
	// Version is the identity schema version. A value of 0 means the identity was
	// persisted before Resumable existed — such a thread may hold a real conversation
	// even though Resumable reads false, so a reconnect still attempts to resume it
	// (the handshake fallback covers a genuine miss) rather than silently discarding
	// it. A current identity (Version >= 1) is trusted: Resumable=false means truly
	// not-yet-run, so start fresh.
	Version int `json:"v,omitempty"`
}

// identityVersion is the current Identity schema version (see Identity.Version).
const identityVersion = 1

// Identity returns the current durable identity of the supervised session.
func (s *Supervisor) Identity() Identity {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.identityLocked()
}

// ThreadID returns the pinned Codex thread id, or "" before the handshake.
func (s *Supervisor) ThreadID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.threadID
}

// ── lifecycle ────────────────────────────────────────────────────────────────

// Start launches the background App Server under the amux sandbox launcher, dials
// its WebSocket endpoint, and runs the handshake. ctx is the SUPERVISOR's lifetime
// (the daemon's context) — not any pane or client connection; when it is cancelled
// the supervisor closes and the server is killed. wrappedArgv is the fully
// sandbox-wrapped command the daemon resolved (bwrap … -- codex app-server --listen
// <endpoint>); passing it here is how the server inherits the session's mount/config/
// identity scope instead of a bare exec. A nil wrappedArgv falls back to the inner
// AppServerArgv (used only by the opt-in smoke test, which runs codex directly).
func (s *Supervisor) Start(ctx context.Context, wrappedArgv []string) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("codexapp: supervisor closed")
	}
	if s.proc != nil {
		s.mu.Unlock()
		return errors.New("codexapp: already started")
	}
	s.mu.Unlock()

	if s.cfg.Endpoint == "" {
		return errors.New("codexapp: no endpoint")
	}
	if p := unixEndpointPath(s.cfg.Endpoint); p != "" {
		_ = os.Remove(p) // a stale socket from a prior run blocks the listener bind
	}

	argv := wrappedArgv
	if len(argv) == 0 {
		argv = AppServerArgv(s.cfg.Bin, s.cfg.Endpoint)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = s.cfg.Dir
	cmd.Env = append(os.Environ(), s.cfg.Env...)
	// Own process group: signals aimed at the foreground pane never reach the
	// background server (independent lifetime).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("codexapp: start app-server: %w", err)
	}
	s.mu.Lock()
	s.proc = cmd
	s.mu.Unlock()

	conn, err := s.dialWithRetry(ctx)
	if err != nil {
		s.killProc()
		return err
	}
	if err := s.attach(ctx, conn); err != nil {
		s.killProc()
		return err
	}
	// Context cancellation (daemon shutdown, or the manager dropping this session)
	// must tear the supervisor down even if nothing calls Close — the audit found
	// ctx cancel alone did not stop the server.
	go func() {
		<-ctx.Done()
		_ = s.Close()
	}()
	return nil
}

// dialWithRetry waits for the App Server's WebSocket endpoint to accept a
// connection and complete the handshake, up to the configured timeout (the child
// needs a moment to bind its listener).
func (s *Supervisor) dialWithRetry(ctx context.Context) (msgConn, error) {
	deadline := time.Now().Add(s.cfg.DialTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		dialCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		conn, err := dialWS(dialCtx, s.cfg.Endpoint, s.cfg.Origin)
		cancel()
		if err == nil {
			return conn, nil
		}
		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = errors.New("timeout")
	}
	return nil, fmt.Errorf("codexapp: connect %s: %w", s.cfg.Endpoint, lastErr)
}

// attach runs the protocol core over an already-connected transport: it starts
// the read loop, performs the handshake, and pins the thread id. Exposed to tests
// via an in-memory msgConn; Start calls it with the dialed WebSocket.
func (s *Supervisor) attach(ctx context.Context, transport msgConn) error {
	rpc := newRPCConn(transport)
	rpc.onNotify = s.onNotify
	rpc.onRequest = s.onRequest
	s.mu.Lock()
	s.rpc = rpc
	s.mu.Unlock()

	runCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.runCancel = cancel
	s.mu.Unlock()
	go func() {
		_ = rpc.run(runCtx)
		// The transport is gone: unblock any pending turn and tear the hub down so
		// subscribers stop cleanly (mirrors the pane sender teardown).
		s.failTurn()
		s.hub.close()
	}()

	return s.handshake(ctx)
}

// handshake runs initialize → initialized → thread/start|resume and pins the
// resulting thread id.
func (s *Supervisor) handshake(ctx context.Context) error {
	if _, err := s.rpc.call(ctx, "initialize", map[string]any{
		"clientInfo": map[string]any{"name": "amux", "title": "amux supervisor", "version": "1"},
		"capabilities": map[string]any{
			"experimentalApi": true, // turn/steer, turn/interrupt gate on this (docs: -32601 otherwise)
		},
	}); err != nil {
		return fmt.Errorf("codexapp initialize: %w", err)
	}
	if err := s.rpc.notify("initialized", map[string]any{}); err != nil {
		return fmt.Errorf("codexapp initialized: %w", err)
	}

	start := func() (json.RawMessage, error) {
		return s.rpc.call(ctx, "thread/start", map[string]any{
			"approvalPolicy": s.cfg.ApprovalPolicy,
			"sandbox":        s.cfg.Sandbox,
		})
	}

	var res json.RawMessage
	var err error
	resumed := s.cfg.ResumeThreadID != ""
	if resumed {
		res, err = s.rpc.call(ctx, "thread/resume", map[string]any{"threadId": s.cfg.ResumeThreadID})
		if err != nil && isNoRollout(err) {
			// A pinned thread that never ran a turn has no rollout to resume (ROOT
			// probe: "no rollout found"). Start a fresh thread and adopt its id rather
			// than failing the launch — the empty thread carried no history to lose.
			resumed = false
			res, err = start()
		}
	} else {
		res, err = start()
	}
	if err != nil {
		return fmt.Errorf("codexapp thread handshake: %w", err)
	}

	id := threadIDFromResult(res)
	if id == "" && resumed {
		id = s.cfg.ResumeThreadID // resumed onto the pinned id even if the server echoed nothing
	}
	s.mu.Lock()
	if s.threadID == "" {
		s.threadID = id
	} else {
		id = s.threadID // a thread/started notification raced in first; it wins
	}
	// A SUCCESSFUL resume means the thread already had a rollout, so it stays
	// resumable across further restarts even if this run never adds a turn. Without
	// this, Manager.Ensure's post-Start SaveIdentity would overwrite a persisted
	// Resumable:true with false, and a second restart with no intervening turn would
	// silently drop the pinned thread and start a new one — losing the conversation
	// (ROOT regression on #98). A fresh/fallback thread/start stays not-resumable
	// until its first turn.
	if resumed {
		s.resumable = true
	}
	s.mu.Unlock()
	if id == "" {
		return errors.New("codexapp: no thread id from handshake")
	}
	return nil
}

// isNoRollout reports whether a thread/resume error is the "no rollout found"
// case — a pinned thread that never had a turn, so there is nothing to resume.
func isNoRollout(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "no rollout")
}

// unixEndpointPath returns the filesystem path of a unix:// endpoint, or "" for a
// ws/wss endpoint (nothing to unlink).
func unixEndpointPath(endpoint string) string {
	if strings.HasPrefix(endpoint, "unix://") {
		return strings.TrimPrefix(endpoint, "unix://")
	}
	return ""
}

func threadIDFromResult(res json.RawMessage) string {
	var p struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
		ThreadID  string `json:"threadId"`
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(res, &p)
	switch {
	case p.Thread.ID != "":
		return p.Thread.ID
	case p.ThreadID != "":
		return p.ThreadID
	default:
		return p.SessionID
	}
}

// Close terminates the supervised server and tears down the client. Idempotent.
// This is the ONLY thing (besides ctx cancellation from daemon shutdown) that
// stops the App Server — a client disconnect never reaches here.
func (s *Supervisor) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	cancel := s.runCancel
	rpc := s.rpc
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if rpc != nil {
		_ = rpc.close()
	}
	s.hub.close()
	s.closeLog()
	s.killProc()
	return nil
}

func (s *Supervisor) killProc() {
	s.mu.Lock()
	proc := s.proc
	s.proc = nil
	s.mu.Unlock()
	if proc == nil || proc.Process == nil {
		return
	}
	// Signal the whole process group so any children the server spawned go too.
	_ = syscall.Kill(-proc.Process.Pid, syscall.SIGTERM)
	_ = proc.Process.Kill()
	_, _ = proc.Process.Wait()
	if p := unixEndpointPath(s.cfg.Endpoint); p != "" {
		_ = os.Remove(p)
	}
}

// ── control verbs ────────────────────────────────────────────────────────────

// Prompt starts one turn (turn/start) and blocks until the App Server's
// turn/completed notification resolves it, bracketing the turn with
// turn_start/turn_end events on the runtime-event stream.
func (s *Supervisor) Prompt(ctx context.Context, text string) error {
	done := make(chan *turnResult, 1)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("codexapp: session closed")
	}
	s.turnDone = done
	threadID := s.threadID
	s.mu.Unlock()

	// turn_start / turn_end are emitted from the OBSERVED turn/started + turn/completed
	// notifications (onNotify), so every turn is bracketed once regardless of origin.
	// Prompt only starts the turn and waits for completion.
	res, err := s.rpc.call(ctx, "turn/start", map[string]any{
		"threadId": threadID,
		"input":    inputBlocks(text),
	})
	if err != nil {
		// No turn began, so no observed turn/completed will arrive — emit a synthetic
		// end so a consumer isn't left waiting.
		s.clearTurn()
		s.emit(turnEndEvent("error"))
		return err
	}
	s.mu.Lock()
	s.curTurn = turnIDFromResult(res)
	s.mu.Unlock()

	select {
	case <-ctx.Done():
		// The caller's context ended; the turn continues server-side and its observed
		// turn/completed will bracket and clear it. Don't emit a synthetic end here.
		return ctx.Err()
	case <-done:
		// turn/completed was observed: onNotify already emitted turn_end and cleared
		// the turn + open approvals. Nothing to do but return.
		return nil
	}
}

func turnIDFromResult(res json.RawMessage) string {
	var p struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
		TurnID string `json:"turnId"`
	}
	_ = json.Unmarshal(res, &p)
	if p.Turn.ID != "" {
		return p.Turn.ID
	}
	return p.TurnID
}

// Interject steers the in-flight turn (turn/steer): it appends input to the
// running turn rather than starting a new one.
func (s *Supervisor) Interject(ctx context.Context, text string) error {
	s.mu.Lock()
	threadID := s.threadID
	turnID := s.curTurn
	s.mu.Unlock()
	params := map[string]any{"threadId": threadID, "input": inputBlocks(text)}
	if turnID != "" {
		params["expectedTurnId"] = turnID
	}
	_, err := s.rpc.call(ctx, "turn/steer", params)
	return err
}

// Cancel interrupts the in-flight turn (turn/interrupt); the turn then completes
// with status "interrupted", which unblocks the pending Prompt.
func (s *Supervisor) Cancel(ctx context.Context) error {
	s.mu.Lock()
	threadID := s.threadID
	turnID := s.curTurn
	s.mu.Unlock()
	params := map[string]any{"threadId": threadID}
	if turnID != "" {
		params["turnId"] = turnID
	}
	_, err := s.rpc.call(ctx, "turn/interrupt", params)
	return err
}

// Resolve answers an App Server approval request. It correlates the decision to
// the exact server request by echoing its JSON-RPC id, and rejects a stale
// (unknown) or duplicate (already-answered) reply.
//
// It does NOT emit permission_resolved just because the write succeeded (ROOT
// audit: "don't declare resolved on a successful write"). The App Server's own
// notification that the item/turn moved on is the resolution signal, so all
// clients — this one and the native TUI — converge on the same truth; a
// turn/completed also clears any still-open approval.
func (s *Supervisor) Resolve(_ context.Context, requestID, decision string) error {
	p, err := s.approvals.take(requestID)
	if err != nil {
		return err // stale or duplicate
	}
	// The approval response is an OBJECT {decision:"accept"|"decline"}, verified
	// against Codex 0.153.4 (a bare enum string is rejected). The same shape serves
	// amux's client and a native `--remote` peer.
	return s.rpc.respond(p.rawID, map[string]any{"decision": decisionToResult(decision)})
}

// decisionToResult maps a contract decision onto the App Server approval
// vocabulary: allow→accept, anything else→decline (never guess affirmatively).
func decisionToResult(decision string) string {
	if strings.EqualFold(decision, harnessproto.DecisionAllow) {
		return "accept"
	}
	return "decline"
}

// OpenApprovals returns the request ids the runtime currently has open, so the
// daemon can refuse a permission verb whose id names no open prompt (the §3.1
// correlation guard), reading the live App Server state rather than a file.
func (s *Supervisor) OpenApprovals() []string { return s.approvals.open() }

// Subscribe joins the runtime-event stream at afterSeq (see eventHub.subscribe).
// This is the exact shape the provider's RuntimeEventStream hook consumes.
func (s *Supervisor) Subscribe(ctx context.Context, afterSeq int64) <-chan harnessproto.RuntimeEventBatch {
	return s.hub.subscribe(ctx, afterSeq)
}

// ── read-loop handlers (run on rpcConn.run's goroutine) ──────────────────────

func (s *Supervisor) onNotify(method string, params json.RawMessage) {
	if method == unparsableMethod {
		s.emit(rawEvent("$unparsable", params))
		return
	}
	if method == "thread/started" {
		if id := threadIDFromResult(params); id != "" {
			s.mu.Lock()
			s.threadID = id
			s.mu.Unlock()
		}
	}
	// Track the active turn from ANY origin (ROOT audit): a turn started in the
	// native TUI raises turn/started too, and web steer/interrupt must target it —
	// not only turns this supervisor's Prompt began. Filter to our pinned thread so
	// a stray id can't hijack the tracked turn.
	if method == "turn/started" {
		s.trackTurn(params)
	}
	events, res := mapNotification(method, params, s.state)
	for _, ev := range events {
		s.emit(ev)
	}
	if res != nil {
		// The turn ended: wake a Prompt waiting on it FIRST (deliverTurn reads
		// s.turnDone, which clearTurn nils), then clear the tracked turn and any
		// approval it left open.
		s.deliverTurn(res)
		s.clearTurn()
		s.clearOpenApprovals("turn ended")
	}
}

// trackTurn records the in-flight turn id from a turn/started notification when it
// belongs to our pinned thread, so Interject/Cancel target it regardless of which
// client initiated the turn.
func (s *Supervisor) trackTurn(params json.RawMessage) {
	var p struct {
		ThreadID string `json:"threadId"`
		Turn     struct {
			ID string `json:"id"`
		} `json:"turn"`
		TurnID string `json:"turnId"`
	}
	_ = json.Unmarshal(params, &p)
	turnID := p.Turn.ID
	if turnID == "" {
		turnID = p.TurnID
	}
	if turnID == "" {
		return
	}
	s.mu.Lock()
	newlyResumable := false
	if p.ThreadID == "" || p.ThreadID == s.threadID {
		s.curTurn = turnID
		if !s.resumable {
			// A turn has begun, so the thread now has a rollout — future launches may
			// safely `thread/resume` it. Persist that fact once so a reconnect resumes
			// rather than starting fresh, and never attempts a resume before a rollout.
			s.resumable = true
			newlyResumable = true
		}
	}
	id := s.identityLocked()
	s.mu.Unlock()
	if newlyResumable {
		_ = SaveIdentity(id)
	}
}

// identityLocked builds the Identity without taking s.mu (caller holds it).
func (s *Supervisor) identityLocked() Identity {
	return Identity{
		SessionID:   s.cfg.SessionID,
		Endpoint:    s.cfg.Endpoint,
		ThreadID:    s.threadID,
		ControlMode: harnessproto.ControlModeStructured,
		Resumable:   s.resumable,
		Version:     identityVersion,
	}
}

func (s *Supervisor) onRequest(id json.RawMessage, method string, params json.RawMessage) {
	switch {
	case strings.HasSuffix(method, "/requestApproval"):
		s.handleApproval(id, method, params)
	case method == "tool/requestUserInput":
		s.handleUserInput(id, params)
	default:
		s.emit(rawEvent(method, params))
		_ = s.rpc.respondErr(id, -32601, "unsupported server request")
	}
}

func (s *Supervisor) handleApproval(id json.RawMessage, method string, params json.RawMessage) {
	var p struct {
		ItemID   string `json:"itemId"`
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
		Command  string `json:"command"`
		Reason   string `json:"reason"`
	}
	_ = json.Unmarshal(params, &p)
	key := idKey(id)

	tool, action := "command_execution", p.Command
	if strings.Contains(method, "fileChange") {
		tool, action = "file_change", "apply patch"
	}

	// Register BEFORE emitting so a consumer that Resolves the instant it sees the
	// event always finds the outstanding request (no emit/register race).
	s.approvals.register(&pendingApproval{
		rawID:    append(json.RawMessage(nil), id...),
		key:      key,
		method:   method,
		threadID: p.ThreadID,
		turnID:   p.TurnID,
		itemID:   p.ItemID,
	})

	s.emit(harnessproto.RuntimeEvent{
		Type:      harnessproto.TypePermissionRequest,
		ItemID:    p.ItemID,
		Direction: harnessproto.DirOut,
		Payload: mustMarshal(map[string]any{
			"request_id": key,
			"tool":       tool,
			"action":     action,
			"reason":     p.Reason,
			"thread_id":  p.ThreadID,
			"turn_id":    p.TurnID,
			"item_id":    p.ItemID,
			"options": []map[string]any{
				{"id": "accept", "label": "Allow", "kind": "allow"},
				{"id": "decline", "label": "Deny", "kind": "deny"},
			},
		}),
	})
}

// handleUserInput represents a structured request-user-input. amux does NOT
// auto-answer it (ROOT audit): with the native TUI (and other clients) attached to
// the same server, answering empty here would preempt a client that can actually
// collect the input. So the questions are surfaced as a notice + raw (nothing
// lost) and the request is left open for another client to answer. The answer
// shape, when a client does answer, is a MAP keyed by question id — not an array
// (verified against Codex 0.153.4) — but amux is not that client here.
func (s *Supervisor) handleUserInput(_ json.RawMessage, params json.RawMessage) {
	var p struct {
		Questions []struct {
			Question string `json:"question"`
		} `json:"questions"`
	}
	_ = json.Unmarshal(params, &p)
	qs := make([]string, 0, len(p.Questions))
	for _, q := range p.Questions {
		qs = append(qs, q.Question)
	}
	s.emit(notice("info", "request-user-input awaiting a client: "+strings.Join(qs, " | ")))
	s.emit(rawEvent("tool/requestUserInput", params))
}

// ── turn signalling ──────────────────────────────────────────────────────────

func (s *Supervisor) deliverTurn(res *turnResult) {
	s.mu.Lock()
	ch := s.turnDone
	s.mu.Unlock()
	if ch != nil {
		select {
		case ch <- res:
		default:
		}
	}
}

// failTurn unblocks a pending Prompt if the transport closes mid-turn.
func (s *Supervisor) failTurn() {
	s.mu.Lock()
	ch := s.turnDone
	s.mu.Unlock()
	if ch != nil {
		select {
		case ch <- &turnResult{StopReason: "disconnected", IsError: true}:
		default:
		}
	}
}

func (s *Supervisor) clearTurn() {
	s.mu.Lock()
	s.turnDone = nil
	s.curTurn = ""
	s.mu.Unlock()
}

// clearOpenApprovals emits a permission_resolved(cleared) for every approval
// still open, so a consumer never waits forever on a prompt the turn abandoned.
func (s *Supervisor) clearOpenApprovals(string) {
	for _, id := range s.approvals.open() {
		if _, err := s.approvals.take(id); err == nil {
			s.emit(permissionResolved(id, harnessproto.DecisionCleared))
		}
	}
}

func permissionResolved(requestID, decision string) harnessproto.RuntimeEvent {
	return harnessproto.RuntimeEvent{
		Type:      harnessproto.TypePermissionResolved,
		Direction: harnessproto.DirOut,
		Payload:   mustMarshal(map[string]any{"request_id": requestID, "decision": decision}),
	}
}

// inputBlocks builds the App Server turn input array (a single text block; images
// are not carried on the amux structured path in this pilot).
func inputBlocks(text string) []map[string]any {
	return []map[string]any{{"type": "text", "text": text}}
}

// ── event persistence ────────────────────────────────────────────────────────

// emit records ev on the durable event log (when EventLogPath is set) and fans it
// out to the in-memory hub. The log is the transport the out-of-process provider
// tails via the existing runtime-events reader; the hub serves any in-daemon
// subscriber. Every event the supervisor produces goes through here.
func (s *Supervisor) emit(ev harnessproto.RuntimeEvent) {
	s.writeLog(ev)
	s.hub.emit(ev)
}

// writeLog appends one marshaled event as an NDJSON line to EventLogPath, opening
// (and truncating) the file on first use. Best-effort: a failure disables further
// logging rather than ever blocking the read loop or a verb — the durable log is a
// transport, and losing it must never be worse than the turn it was carrying.
func (s *Supervisor) writeLog(ev harnessproto.RuntimeEvent) {
	if s.cfg.EventLogPath == "" {
		return
	}
	s.logMu.Lock()
	defer s.logMu.Unlock()
	if s.logErr {
		return
	}
	if s.logW == nil {
		if err := os.MkdirAll(filepath.Dir(s.cfg.EventLogPath), 0o700); err != nil {
			s.logErr = true
			return
		}
		// Truncate: each supervisor lifetime is a fresh event seq space. The tailer
		// resyncs on the size shrink and a consumer dedups by ordinal, exactly as it
		// does for a rollout --resume rewrite.
		f, err := os.OpenFile(s.cfg.EventLogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			s.logErr = true
			return
		}
		s.logW = f
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	if _, err := s.logW.Write(append(b, '\n')); err != nil {
		s.logErr = true
	}
}

func (s *Supervisor) closeLog() {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	if s.logW != nil {
		_ = s.logW.Close()
		s.logW = nil
	}
}
