package codexapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
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
//   - Start(ctx): production. Spawns `codex app-server --listen unix://<socket>`,
//     waits for the socket, dials it, and runs the handshake. The process is put
//     in its own process group so it is not felled by signals aimed at the pane.
//   - attach(ctx, transport): the protocol core over any io.ReadWriteCloser —
//     exercised in tests against an in-memory fake App Server (net.Pipe).

// Pilot posture for thread/start (mirrors the AGE-179 harness pilot). onRequest so
// the server actually raises answerable approvals — confined to workspaceWrite,
// never dangerFullAccess. The process-level sandbox is governed by amux's own
// launcher; these are the thread-level request defaults.
const (
	defaultApprovalPolicy = "onRequest"
	defaultSandbox        = "workspaceWrite"
	defaultDialTimeout    = 15 * time.Second
)

// Config parameterizes one supervised App Server. SessionID ties it to the amux
// session (identity persistence); SocketPath is the per-session Unix socket
// inside the session's private scope.
type Config struct {
	SessionID      string
	Bin            string        // codex binary; "" ⇒ "codex" (resolved by the caller / PATH)
	Dir            string        // working directory (the worktree)
	Env            []string      // extra KEY=VALUE additions to the child environment
	SocketPath     string        // unix socket to listen on / dial
	ResumeThreadID string        // non-empty ⇒ thread/resume instead of thread/start
	ApprovalPolicy string        // "" ⇒ defaultApprovalPolicy
	Sandbox        string        // "" ⇒ defaultSandbox
	DialTimeout    time.Duration // "" ⇒ defaultDialTimeout
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

// AppServerArgv is the argv amux launches to run the background App Server on a
// Unix socket. The official docs show `codex app-server --listen ws://...`; the
// pinned 0.153.4 CLI additionally accepts a unix:// endpoint (see the AGE-177
// note). This is the exact command a native CLI's `--remote` peer must point at.
//
// UNVALIDATED ON HOST: there is no codex binary in the amux CI sandbox, so the
// exact --listen flag/scheme is validated only against docs and the pinned CLI
// surface here; the opt-in smoke test captures the real invocation on a host with
// codex installed. Do not treat this as live-verified.
func AppServerArgv(bin, socketPath string) []string {
	if bin == "" {
		bin = "codex"
	}
	return []string{bin, "app-server", "--listen", "unix://" + socketPath}
}

// AttachArgv is the argv a native Codex CLI (in an amux pane) uses to attach to
// the SAME supervised server/thread — the whole point of AGE-181: the terminal UI
// and the web adapter drive one server/thread, not separate processes.
//
// UNVALIDATED ON HOST: that `codex --remote unix://<socket> resume <thread-id>`
// attaches to the running server/thread (rather than starting its own process)
// MUST be confirmed on a host with the pinned codex. This builder encodes the
// documented syntax; the caller must not claim live attachment until the smoke
// test / host run confirms it.
func AttachArgv(bin, socketPath, threadID string) []string {
	if bin == "" {
		bin = "codex"
	}
	argv := []string{bin, "--remote", "unix://" + socketPath}
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
	SocketPath  string `json:"socketPath"`
	ThreadID    string `json:"threadId"`
	ControlMode string `json:"controlMode"`
}

// Identity returns the current durable identity of the supervised session.
func (s *Supervisor) Identity() Identity {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Identity{
		SessionID:   s.cfg.SessionID,
		SocketPath:  s.cfg.SocketPath,
		ThreadID:    s.threadID,
		ControlMode: harnessproto.ControlModeStructured,
	}
}

// ThreadID returns the pinned Codex thread id, or "" before the handshake.
func (s *Supervisor) ThreadID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.threadID
}

// ── lifecycle ────────────────────────────────────────────────────────────────

// Start launches the background App Server, dials its socket, and runs the
// handshake. ctx is the SUPERVISOR's lifetime (pass the daemon's context) — it is
// not tied to any pane or client connection. On success the server is live and
// the thread is pinned; on any failure the partial process is cleaned up.
func (s *Supervisor) Start(ctx context.Context) error {
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

	if s.cfg.SocketPath == "" {
		return errors.New("codexapp: no socket path")
	}
	// A stale socket from a prior run would make the listener fail to bind.
	_ = os.Remove(s.cfg.SocketPath)

	argv := AppServerArgv(s.cfg.Bin, s.cfg.SocketPath)
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

	conn, err := s.dialSocket(ctx)
	if err != nil {
		s.killProc()
		return err
	}
	if err := s.attach(ctx, conn); err != nil {
		s.killProc()
		return err
	}
	return nil
}

// dialSocket waits for the App Server's Unix socket to appear and become
// dialable, up to the configured timeout.
func (s *Supervisor) dialSocket(ctx context.Context) (net.Conn, error) {
	deadline := time.Now().Add(s.cfg.DialTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		conn, err := net.Dial("unix", s.cfg.SocketPath)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = errors.New("timeout")
	}
	return nil, fmt.Errorf("codexapp: dial %s: %w", s.cfg.SocketPath, lastErr)
}

// attach runs the protocol core over an already-connected transport: it starts
// the read loop, performs the handshake, and pins the thread id. Exposed to tests
// via a net.Pipe fake; Start calls it with the dialed socket.
func (s *Supervisor) attach(ctx context.Context, transport io.ReadWriteCloser) error {
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

	var res json.RawMessage
	var err error
	if s.cfg.ResumeThreadID != "" {
		res, err = s.rpc.call(ctx, "thread/resume", map[string]any{"threadId": s.cfg.ResumeThreadID})
	} else {
		res, err = s.rpc.call(ctx, "thread/start", map[string]any{
			"approvalPolicy": s.cfg.ApprovalPolicy,
			"sandbox":        s.cfg.Sandbox,
		})
	}
	if err != nil {
		return fmt.Errorf("codexapp thread handshake: %w", err)
	}

	id := threadIDFromResult(res)
	if id == "" {
		id = s.cfg.ResumeThreadID // resume onto the pinned id even if the server echoed nothing
	}
	s.mu.Lock()
	if s.threadID == "" {
		s.threadID = id
	} else {
		id = s.threadID // a thread/started notification raced in first; it wins
	}
	s.mu.Unlock()
	if id == "" {
		return errors.New("codexapp: no thread id from handshake")
	}
	return nil
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
	if s.cfg.SocketPath != "" {
		_ = os.Remove(s.cfg.SocketPath)
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

	s.emit(harnessproto.RuntimeEvent{Type: harnessproto.TypeTurnStart, Direction: harnessproto.DirMeta, Payload: json.RawMessage(`{}`)})

	res, err := s.rpc.call(ctx, "turn/start", map[string]any{
		"threadId": threadID,
		"input":    inputBlocks(text),
	})
	if err != nil {
		s.clearTurn()
		s.emit(harnessproto.RuntimeEvent{Type: harnessproto.TypeTurnEnd, Direction: harnessproto.DirMeta,
			Payload: mustMarshal(map[string]any{"stop_reason": "error", "error": err.Error()})})
		return err
	}
	s.mu.Lock()
	s.curTurn = turnIDFromResult(res)
	s.mu.Unlock()

	select {
	case <-ctx.Done():
		s.clearTurn()
		s.emit(harnessproto.RuntimeEvent{Type: harnessproto.TypeTurnEnd, Direction: harnessproto.DirMeta,
			Payload: mustMarshal(map[string]any{"stop_reason": "cancelled"})})
		return ctx.Err()
	case r := <-done:
		s.clearTurn()
		s.clearOpenApprovals("turn ended")
		s.emit(harnessproto.RuntimeEvent{Type: harnessproto.TypeTurnEnd, Direction: harnessproto.DirMeta,
			Payload: mustMarshal(map[string]any{"stop_reason": r.StopReason})})
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
// (unknown) or duplicate (already-answered) reply. On success it emits a
// permission_resolved event so the stream records the prompt as closed.
func (s *Supervisor) Resolve(_ context.Context, requestID, decision string) error {
	p, err := s.approvals.take(requestID)
	if err != nil {
		return err // stale or duplicate
	}
	// Answer with the bare App Server approval enum, matching the AGE-179 harness
	// pilot verbatim so both amux's client and a native `--remote` peer resolve
	// identically (and any host-validated correction lands in one shape).
	if err := s.rpc.respond(p.rawID, decisionToResult(decision)); err != nil {
		return err
	}
	s.emit(permissionResolved(requestID, decision))
	return nil
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
	events, res := mapNotification(method, params, s.state)
	for _, ev := range events {
		s.emit(ev)
	}
	if res != nil {
		s.deliverTurn(res)
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

// handleUserInput represents a structured request-user-input. There is no
// interactive-input event type in the shared contract and no path to collect an
// answer here, so it is EXPLICITLY LIMITED: the questions are surfaced as a notice
// + raw (nothing lost) and the server request is answered with empty answers so
// the turn does not hang. Same posture as the AGE-179 pilot.
func (s *Supervisor) handleUserInput(id json.RawMessage, params json.RawMessage) {
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
	s.emit(notice("info", "request-user-input (not interactively answerable): "+strings.Join(qs, " | ")))
	s.emit(rawEvent("tool/requestUserInput", params))
	_ = s.rpc.respond(id, map[string]any{"answers": []string{}})
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
