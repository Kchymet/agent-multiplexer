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
	// killWaitDelay bounds how long killProc's cmd.Wait blocks after the process is
	// killed, waiting for os/exec's stderr-copier goroutine to drain. Normally the
	// copier hits EOF the instant the child exits; but if the child spawned a
	// descendant that inherited and still holds the stderr pipe's write end (and
	// escaped the process-group kill via its own session), that EOF never comes.
	// WaitDelay makes Wait force the pipe closed after this delay and return, so a
	// launch/dial failure can never hang on a lingering grandchild (AGE-198 lifecycle
	// audit). The child's own stderr is already drained before this matters.
	killWaitDelay = 2 * time.Second
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

	mu       sync.Mutex
	proc     *exec.Cmd
	closed   bool
	threadID string

	// curTurn is the OBSERVED active turn on our pinned thread, from ANY origin (a
	// native TUI turn raises turn/started too). It is the target for Cancel/Interject,
	// and is cleared when that turn completes. It is deliberately NOT the local Prompt's
	// ownership: a peer turn sets curTurn, but must never satisfy our pending request.
	curTurn string

	// The local-request ownership state below is what a Prompt's own turn/start
	// establishes, scoped to a generation so an abandoned/cancelled Prompt can neither
	// rebind a newer request nor have late cleanup erase a newer waiter.
	turnDone  chan *turnResult       // the pending Prompt's waiter (nil when none)
	turnGen   uint64                 // bumped per Prompt; scopes ownership + cleanup
	ownTurn   string                 // turn id THIS Prompt owns, bound from its turn/start response ("" until bound)
	earlyTerm map[string]*turnResult // terminal events observed on our thread before ownTurn was bound, by turn id

	runCancel context.CancelFunc

	logMu  sync.Mutex
	logW   io.WriteCloser // EventLogPath sink, opened lazily on first emit
	logErr bool           // a prior log write failed; stop retrying (never fatal)

	resumable bool // a rollout exists; native attach and later resume are safe
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
// The handshake persists fresh threads before exposing this command, including
// the empty rollout required by the native TUI's paginated resume in Codex 0.153.4.
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
	// Resumable is true after successful resume. Fresh threads are named and
	// resumed during initialization, so they also have a rollout before any turn.
	// Older identities may still describe a never-persisted empty thread; the
	// manager's gating and handshake fallback retain compatibility with those.
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
	// Capture the child's stderr into a bounded ring so a startup failure inside the
	// sandbox wrapper (an execvp ENOENT, a bwrap mount error) is explained in the
	// error below instead of only surfacing as a generic dial timeout — os/exec would
	// otherwise send stderr to /dev/null. The ring keeps only the last few KiB, so the
	// long-running server's stderr never grows memory; control/events flow over the WS
	// protocol, not stderr, so this is purely diagnostic.
	stderr := newStderrRing(maxStderrCapture)
	cmd.Stderr = stderr
	// Bound the post-exit wait for the stderr-copier goroutine so a killed child whose
	// descendant still holds the stderr pipe can't wedge killProc's cmd.Wait (see
	// killWaitDelay). Zero (the default) would wait forever for that pipe to close.
	cmd.WaitDelay = killWaitDelay
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
		s.killProc() // waits for the child + stderr copier, so the tail below is complete
		return withStderrTail(err, stderr)
	}
	if err := s.attach(ctx, conn); err != nil {
		s.killProc()
		return withStderrTail(err, stderr)
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

// withStderrTail appends the captured child-stderr tail to a launch/dial/handshake
// error, so pane.exit and the async start journal explain the ACTUAL cause (e.g. a
// bwrap execvp ENOENT) rather than only the surface symptom. When nothing was
// captured the original error is returned unchanged.
func withStderrTail(err error, r *stderrRing) error {
	tail := r.tail()
	if tail == "" {
		return err
	}
	return fmt.Errorf("%w\ncodexapp: app-server stderr (last %dB):\n%s", err, len(tail), tail)
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
	if id == "" {
		return errors.New("codexapp: no thread id from handshake")
	}
	if !resumed {
		// Codex 0.153.4 does not persist an empty thread/start. Naming it makes
		// the rollout persistable; a normal resume flushes it before the native
		// TUI's excludeTurns=true resume loads paginated history. Neither RPC
		// starts a model turn. Naming alone still fails "missing source rollout".
		if _, err := s.rpc.call(ctx, "thread/name/set", map[string]any{
			"threadId": id, "name": "amux " + s.cfg.SessionID,
		}); err != nil {
			return fmt.Errorf("codexapp name fresh thread: %w", err)
		}
		ready, err := s.rpc.call(ctx, "thread/resume", map[string]any{"threadId": id})
		if err != nil {
			return fmt.Errorf("codexapp persist fresh thread: %w", err)
		}
		if got := threadIDFromResult(ready); got != id {
			return fmt.Errorf("codexapp fresh thread changed during persistence: got %q, want %q", got, id)
		}
	}
	s.mu.Lock()
	s.threadID = id
	// Both paths now completed a successful resume, even before a first turn.
	// Manager.Ensure can persist this identity for native attach and restarts.
	s.resumable = true
	s.mu.Unlock()
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
	// Signal the whole process group so any children the server spawned go too: SIGTERM
	// for a clean exit, then SIGKILL as the forceful backstop for anything in the group
	// that ignores it. The child is its own group leader (Setpgid), so -pid targets its
	// group alone, never the daemon's. A descendant that started its own session escapes
	// the group; the WaitDelay below (not the group signal) is what bounds that case.
	_ = syscall.Kill(-proc.Process.Pid, syscall.SIGTERM)
	_ = syscall.Kill(-proc.Process.Pid, syscall.SIGKILL)
	_ = proc.Process.Kill()
	// cmd.Wait (not Process.Wait) reaps the child AND waits for os/exec's stderr-copier
	// goroutine to drain, then closes the pipe fds — so the captured stderr tail is
	// complete and the diagnostic pipe never leaks. cmd.WaitDelay (set in Start) bounds
	// that wait, so a descendant still holding the stderr pipe open cannot wedge this.
	// Called exactly once per proc (this function nils s.proc under the lock).
	_ = proc.Wait()
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
	// Open a fresh ownership generation for this Prompt. ownTurn stays unbound until
	// our own turn/start response returns; earlyTerm retains any terminal observed for
	// our thread in the meantime so an early completion isn't lost (and a peer's isn't
	// mistaken for ours).
	s.turnGen++
	gen := s.turnGen
	s.turnDone = done
	s.ownTurn = ""
	s.earlyTerm = map[string]*turnResult{}
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
		// end so a consumer isn't left waiting. Carry the pinned thread id for
		// correlation; there is no turn id (the turn never started). Abandon only OUR
		// local waiter (scoped to this generation) — a peer turn's active-control state
		// (curTurn) must survive our failed start.
		s.abandonLocalTurn(gen)
		s.emit(turnEndEvent(threadID, "", "error"))
		return err
	}
	// Bind ownership to the ACTUAL turn/start response id, scoped to this generation.
	// If a superseding Prompt (or cancellation) already replaced our waiter, do not
	// rebind or touch the newer state. If our turn already completed before the
	// response returned (the completion-before-response race), a matching terminal is
	// waiting in earlyTerm — deliver it now; otherwise discard the retained peer
	// terminals (they were never ours) and wait for the live completion.
	myTurn := turnIDFromResult(res)
	s.mu.Lock()
	if s.turnGen == gen && s.turnDone == done {
		s.ownTurn = myTurn
		if myTurn != "" && s.earlyTerm != nil {
			if early, ok := s.earlyTerm[myTurn]; ok {
				s.turnDone = nil
				s.ownTurn = ""
				select {
				case done <- early:
				default:
				}
			}
		}
		s.earlyTerm = nil
	}
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

// errNoActiveTurn is returned by Interject when there is no in-flight turn to steer,
// so a caller (and the daemon's error surface) can tell a precondition failure from a
// transport error — and no malformed turn/steer ever reaches the App Server.
var errNoActiveTurn = errors.New("codexapp: no active turn to interject")

// Interject steers the in-flight turn (turn/steer): it appends input to the
// running turn rather than starting a new one. It requires an active turn — see
// errNoActiveTurn — and never starts one.
func (s *Supervisor) Interject(ctx context.Context, text string) error {
	s.mu.Lock()
	threadID := s.threadID
	turnID := s.curTurn
	s.mu.Unlock()
	// Interject steers an ACTIVE turn, and the App Server requires expectedTurnId to
	// correlate the steer to it. If no exact active turn is known — no pinned thread, or
	// no tracked turn (idle, or a completion raced this call and cleared curTurn) — fail
	// fast BEFORE any RPC: a turn/steer without expectedTurnId is malformed, and we must
	// never create/start a turn or infer/fall back to another target. A process that is
	// running is not the same as a model turn in flight.
	if threadID == "" || turnID == "" {
		return errNoActiveTurn
	}
	_, err := s.rpc.call(ctx, "turn/steer", map[string]any{
		"threadId":       threadID,
		"input":          inputBlocks(text),
		"expectedTurnId": turnID,
	})
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
	// Record the local answer (rejecting stale/duplicate) but do NOT mark it
	// resolved or emit permission_resolved — the server's authoritative
	// `serverRequest/resolved` notification does that, so amux, the native TUI, and
	// the web peer all converge on the same truth (no speculative resolution).
	p, err := s.approvals.answerLocally(requestID)
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
	// The server authoritatively resolves an approval — answered by ANY client —
	// via serverRequest/resolved, which arrives while the turn is still active. Clear
	// it and emit permission_resolved immediately so every client converges before
	// turn end (AGE-198). Handled here (not the pure mapper) because it mutates the
	// approval tracker; returns so it isn't also passed through as a raw event.
	if method == "serverRequest/resolved" {
		s.handleServerResolved(params)
		return
	}
	// A turn lifecycle notification for a DIFFERENT thread belongs to another
	// session/thread: it must neither mutate our state nor enter our event stream
	// (ROOT audit: foreign notifications must not contaminate our stream). A missing
	// threadId is left to the downstream correlation checks, which never treat it as
	// ours.
	if (method == "turn/started" || method == "turn/completed") && s.foreignThread(params) {
		return
	}
	// Track the active turn from ANY origin (ROOT audit): a turn started in the
	// native TUI raises turn/started too, and web steer/interrupt must target it —
	// not only turns this supervisor's Prompt began. This is OBSERVED active-turn
	// tracking (curTurn), deliberately separate from local-request ownership.
	if method == "turn/started" {
		s.trackTurn(params)
	}
	events, res := mapNotification(method, params, s.state)
	for _, ev := range events {
		s.emit(ev)
	}
	if res != nil {
		s.handleTurnCompleted(res)
	}
}

// handleTurnCompleted applies a turn/completed to two INDEPENDENT pieces of state:
//
//   - Observed active turn (curTurn): if this ended the active turn (any origin),
//     clear the Cancel/Interject target and drain the approvals that turn raised. A
//     peer/native-TUI turn owns and clears its own approvals this way.
//   - Local-request ownership (turnDone/ownTurn): the pending Prompt's waiter is
//     satisfied ONLY by the completion of the exact turn its own turn/start response
//     bound (ownTurn). A peer turn — even the observed active one — must never satisfy
//     an unrelated local Prompt. If ownership is not yet bound (the turn/start response
//     is still in flight), the terminal is RETAINED (earlyTerm) so it can be matched
//     once the response binds ownTurn; it is never allowed to clear an unbound waiter.
//
// A completion whose thread is not ours, or which carries no turn id, correlates to
// nothing and is ignored.
func (s *Supervisor) handleTurnCompleted(res *turnResult) {
	turnID := res.TurnID
	s.mu.Lock()
	if s.threadID == "" || res.ThreadID != s.threadID || turnID == "" {
		s.mu.Unlock()
		return
	}

	// (1) Observed active-turn control state.
	if turnID == s.curTurn {
		s.curTurn = ""
	}

	// (2) Local-request ownership.
	var deliverTo chan *turnResult
	switch {
	case s.turnDone == nil:
		// No local Prompt is waiting.
	case s.ownTurn != "":
		// Ownership bound: satisfy the waiter only on an exact turn match.
		if turnID == s.ownTurn {
			deliverTo = s.turnDone
			s.turnDone = nil
			s.ownTurn = ""
			s.earlyTerm = nil
		}
	default:
		// Ownership not yet bound (turn/start response still in flight): retain this
		// terminal so the response can match it. A peer completion retained here is
		// discarded when ownTurn binds to a different id (Prompt) — it never satisfies
		// the unbound waiter.
		if s.earlyTerm == nil {
			s.earlyTerm = map[string]*turnResult{}
		}
		s.earlyTerm[turnID] = res
	}
	s.mu.Unlock()

	if deliverTo != nil {
		select {
		case deliverTo <- res:
		default:
		}
	}
	// Every completed turn on our thread owns the approvals it raised: drain by turn id
	// so a peer turn clears only its own outstanding requests, neutrally.
	s.clearApprovalsForTurn(turnID)
}

// foreignThread reports whether params names a thread other than our pinned one. A
// missing threadId is NOT classified foreign here (downstream correlation handles it);
// only a present, mismatched thread is foreign.
func (s *Supervisor) foreignThread(params json.RawMessage) bool {
	var p struct {
		ThreadID string `json:"threadId"`
	}
	_ = json.Unmarshal(params, &p)
	if p.ThreadID == "" {
		return false
	}
	s.mu.Lock()
	pinned := s.threadID
	s.mu.Unlock()
	return pinned != "" && p.ThreadID != pinned
}

// trackTurn records the OBSERVED active turn from a turn/started notification on our
// pinned thread, so Interject/Cancel target it regardless of which client initiated
// the turn. It only touches curTurn (and the resumable flag) — never local-request
// ownership. A missing or foreign thread does not mutate state.
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
	// Require an exact, present thread match: a missing thread must not mutate our
	// state (ROOT audit), and a foreign one was already dropped by onNotify.
	if p.ThreadID != "" && p.ThreadID == s.threadID {
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

	// Ownership guard (ROOT foreign-approval audit / AGE-179 harness parity): an
	// approval names the thread it belongs to, and only our pinned thread's approvals
	// are answerable through this supervisor. A foreign thread's request belongs to
	// another client/session, and a missing thread is not implicitly ours — neither may
	// become a local OpenApprovals entry a `permission` verb could answer, or reach the
	// event stream. Validate ownership BEFORE registering or emitting. A same-thread
	// request still registers even if it is passive/early (before we track a turn): the
	// guard is on thread, not turn.
	s.mu.Lock()
	pinned := s.threadID
	s.mu.Unlock()
	if p.ThreadID == "" || pinned == "" || p.ThreadID != pinned {
		return
	}

	key := idKey(id)

	tool, action := "command_execution", p.Command
	if strings.Contains(method, "fileChange") {
		tool, action = "file_change", "apply patch"
	}

	// Register BEFORE emitting so a consumer that Resolves the instant it sees the
	// event always finds the outstanding request (no emit/register race). Emit ONLY on
	// the first registration: a re-sent or replayed request whose id is still pending
	// (or already resolved) must not raise a second permission_request — that would
	// reopen an unanswerable prompt or double the decision flow (ROOT approval-replay
	// audit). register is atomic, so exactly one concurrent duplicate emits.
	if !s.approvals.register(&pendingApproval{
		rawID:    append(json.RawMessage(nil), id...),
		key:      key,
		method:   method,
		threadID: p.ThreadID,
		turnID:   p.TurnID,
		itemID:   p.ItemID,
	}) {
		return
	}

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

// abandonLocalTurn drops THIS Prompt's local ownership state (waiter, bound turn,
// retained terminals) when its turn/start never started a turn — but only if the
// generation still matches, so an abandoned start can neither erase a newer waiter
// nor rebind a newer request. It deliberately leaves curTurn (a peer's observed
// active turn) untouched.
func (s *Supervisor) abandonLocalTurn(gen uint64) {
	s.mu.Lock()
	if s.turnGen == gen {
		s.turnDone = nil
		s.ownTurn = ""
		s.earlyTerm = nil
	}
	s.mu.Unlock()
}

// failTurn unblocks a pending Prompt if the transport closes mid-turn, and drops the
// local ownership state so a late completion can't deliver again.
func (s *Supervisor) failTurn() {
	s.mu.Lock()
	ch := s.turnDone
	s.turnDone = nil
	s.ownTurn = ""
	s.earlyTerm = nil
	s.curTurn = ""
	s.mu.Unlock()
	if ch != nil {
		select {
		case ch <- &turnResult{StopReason: "disconnected", IsError: true}:
		default:
		}
	}
}

// handleServerResolved consumes a serverRequest/resolved notification: the server
// (on any client's answer) reports requestId resolved. It matches the pinned
// thread, clears the request, and emits permission_resolved exactly once — with the
// decision this supervisor sent if it answered locally, else "cleared" (another
// client answered). A mismatched thread, or an unknown/already-resolved id, is
// ignored. requestId may be a string or a number.
func (s *Supervisor) handleServerResolved(params json.RawMessage) {
	var p struct {
		ThreadID  string          `json:"threadId"`
		RequestID json.RawMessage `json:"requestId"`
		Decision  string          `json:"decision"` // usually absent; the notice names no winner
	}
	if err := json.Unmarshal(params, &p); err != nil || len(p.RequestID) == 0 {
		return
	}
	// Exact thread correlation is required: a missing threadId is NOT implicitly
	// ours, and a mismatched one belongs to another thread. Either way, ignore.
	if p.ThreadID == "" {
		return
	}
	s.mu.Lock()
	pinned := s.threadID
	s.mu.Unlock()
	if pinned == "" || p.ThreadID != pinned {
		return
	}
	if s.approvals.serverResolved(idKey(p.RequestID)) {
		// Clear NEUTRALLY: the notification names no winning client or decision, and
		// our own reply was only an attempt (a peer may have answered the other way).
		// Surface a real decision only if the server itself supplied one.
		s.emit(permissionResolved(idKey(p.RequestID), winningDecision(p.Decision)))
	}
}

// winningDecision maps a server-supplied winning decision to the contract
// vocabulary, defaulting to DecisionCleared when the server named none (the current
// serverRequest/resolved notice carries no decision). It never invents allow/deny
// from our local attempt.
func winningDecision(serverDecision string) string {
	switch strings.ToLower(strings.TrimSpace(serverDecision)) {
	case "accept", harnessproto.DecisionAllow:
		return harnessproto.DecisionAllow
	case "decline", "reject", harnessproto.DecisionDeny:
		return harnessproto.DecisionDeny
	default:
		return harnessproto.DecisionCleared
	}
}

// clearApprovalsForTurn emits a neutral permission_resolved for every approval the
// completed turn raised but the server never confirmed — so a consumer never waits
// forever. It drains only that turn's requests (by turn id), so a peer/native turn
// ending clears its own approvals without disturbing another turn's outstanding ones.
// The clear is neutral: an unconfirmed local answer is only an attempt, so an
// abandoned turn must not surface it as the outcome.
func (s *Supervisor) clearApprovalsForTurn(turnID string) {
	if turnID == "" {
		return
	}
	for _, key := range s.approvals.drainForTurn(turnID) {
		s.emit(permissionResolved(key, harnessproto.DecisionCleared))
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

// writeLog appends one marshaled event as an NDJSON line to EventLogPath. It opens
// the file in APPEND mode (never truncating): the daemon may have already appended
// cold-start notices for this session (AppendNotice), and the structured event log is
// the session's SINGLE canonical runtime-event source — one append-only file whose
// line order is the event order, so an event's ordinal is its line number and stays
// identical whether a subscriber followed the live stream or reconnected (a merged
// journal+transcript with no cross-file order could not, see runtimeevents.sourcesFor).
// Best-effort: a failure disables further logging rather than ever blocking the read
// loop or a verb — the durable log is a transport, and losing it must never be worse
// than the turn it was carrying.
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
		f, err := os.OpenFile(s.cfg.EventLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
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

// AppendNotice appends one normalized notice event to a structured session's event
// log (EventLogPathFor) as a single NDJSON line, so amux's daemon can record
// cold-start progress and failure notices ONTO the same single, append-only log the
// supervisor writes. Keeping them in one canonical source — rather than a separate
// journal file merged at read time — is what makes replay ordinals stable: one file
// means an event's ordinal is its line number, identical live or on reconnect, even
// for a failure notice that lands after the first turn's output (ROOT interleaved
// replay audit). The write is one O_APPEND of a short line, so it interleaves whole
// lines with the supervisor's own appends rather than tearing one. Best-effort by
// contract: a caller treats a failure like a lost journal line.
func AppendNotice(sessionID, level, text string) error {
	if sessionID == "" || text == "" {
		return nil
	}
	path := EventLogPathFor(sessionID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(notice(level, text))
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

func (s *Supervisor) closeLog() {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	if s.logW != nil {
		_ = s.logW.Close()
		s.logW = nil
	}
}
