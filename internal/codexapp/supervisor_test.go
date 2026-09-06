package codexapp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

type collector struct {
	ch <-chan harnessproto.RuntimeEventBatch
}

func subscribeCollector(ctx context.Context, s *Supervisor) *collector {
	return &collector{ch: s.Subscribe(ctx, 0)}
}

// tryGet returns the next event if one arrives within a short window, else ok=false.
// It is for negative assertions ("nothing reached the stream").
func (c *collector) tryGet() (harnessproto.RuntimeEvent, bool) {
	for {
		select {
		case b, ok := <-c.ch:
			if !ok {
				return harnessproto.RuntimeEvent{}, false
			}
			if len(b.Events) > 0 {
				return b.Events[0], true
			}
		case <-time.After(100 * time.Millisecond):
			return harnessproto.RuntimeEvent{}, false
		}
	}
}

func (c *collector) waitFor(t *testing.T, typ string) harnessproto.RuntimeEvent {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case b, ok := <-c.ch:
			if !ok {
				t.Fatalf("event stream closed before %q", typ)
			}
			for _, ev := range b.Events {
				if ev.Type == typ {
					return ev
				}
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %q", typ)
		}
	}
}

func attach(t *testing.T, sup *Supervisor, client msgConn) {
	t.Helper()
	if err := sup.attach(context.Background(), client); err != nil {
		t.Fatalf("attach/handshake: %v", err)
	}
}

func TestHandshakeStart(t *testing.T) {
	sup, fs, client := newFakePair(t)
	defer fs.close()
	defer sup.Close()
	attach(t, sup, client)

	if got := sup.ThreadID(); got != "thr_1" {
		t.Fatalf("threadID = %q, want thr_1", got)
	}
	if _, ok := fs.sawCall("initialize"); !ok {
		t.Fatal("no initialize call")
	}
	if _, ok := fs.sawCall("thread/start"); !ok {
		t.Fatal("no thread/start call")
	}
	id := sup.Identity()
	if id.ControlMode != harnessproto.ControlModeStructured || id.ThreadID != "thr_1" {
		t.Fatalf("identity = %+v", id)
	}
}

func TestHandshakeInitialPrompt(t *testing.T) {
	const prompt = "Fix the 'Codex' launch\nPreserve $HOME, `quotes`, and Unicode: café."
	for _, tc := range []struct {
		name, prompt, resume, resumeErr string
		wantTurn                        bool
	}{
		{name: "fresh", prompt: " \n" + prompt + "\n ", wantTurn: true},
		{name: "empty"},
		{name: "whitespace", prompt: " \n\t"},
		{name: "resume", prompt: prompt, resume: "thr_saved"},
		{name: "missing rollout", prompt: prompt, resume: "thr_gone", resumeErr: "no rollout found", wantTurn: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sup, fs, client := newFakePair(t)
			defer fs.close()
			defer sup.Close()
			sup.cfg.InitialPrompt = tc.prompt
			sup.cfg.ResumeThreadID = tc.resume
			fs.resumeErr = tc.resumeErr
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			// No turn/completed is sent: launching must wait only for acceptance.
			if err := sup.attach(ctx, client); err != nil {
				t.Fatal(err)
			}
			call, ok := fs.sawCall("turn/start")
			if ok != tc.wantTurn {
				t.Fatalf("turn/start present = %v, want %v", ok, tc.wantTurn)
			}
			if !ok {
				return
			}
			var params struct {
				ThreadID string                        `json:"threadId"`
				Input    []struct{ Type, Text string } `json:"input"`
			}
			if err := json.Unmarshal(call.Params, &params); err != nil {
				t.Fatal(err)
			}
			if params.ThreadID != sup.ThreadID() || len(params.Input) != 1 || params.Input[0].Type != "text" || params.Input[0].Text != prompt {
				t.Fatalf("initial prompt lost or misrouted: %s", call.Params)
			}
		})
	}
}

func TestHandshakeInitialPromptFailure(t *testing.T) {
	sup, fs, client := newFakePair(t)
	defer fs.close()
	defer sup.Close()
	sup.cfg.InitialPrompt = "fix it"
	fs.failMethod = "turn/start"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := sup.attach(ctx, client); err == nil || !strings.Contains(err.Error(), "initial prompt") {
		t.Fatalf("initial prompt failure must fail launch: %v", err)
	}
}

// A structured session has no Codex CLI invocation to consume amux's selected
// model. Carry it on fresh/resumed threads and on the initial App Server turn.
func TestHandshakeSelectedModel(t *testing.T) {
	for _, tc := range []struct {
		name, resume, prompt string
	}{
		{name: "fresh with initial prompt", prompt: "fix the launch path"},
		{name: "resume", resume: "thr_existing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, server := newMemPair()
			fs := &fakeServer{t: t, conn: server, respByID: map[string]chan incoming{}}
			go fs.loop()
			defer fs.close()

			sup := New(Config{
				SessionID: "selected", Endpoint: "unix:///tmp/x.sock",
				ResumeThreadID: tc.resume, InitialPrompt: tc.prompt, Model: "gpt-5.6-sol",
			})
			defer sup.Close()
			attach(t, sup, client)

			method := "thread/start"
			if tc.resume != "" {
				method = "thread/resume"
			}
			thread, ok := fs.sawCall(method)
			if !ok {
				t.Fatalf("no %s call", method)
			}
			var threadParams struct {
				Model string `json:"model"`
			}
			if err := json.Unmarshal(thread.Params, &threadParams); err != nil {
				t.Fatal(err)
			}
			if threadParams.Model != "gpt-5.6-sol" {
				t.Fatalf("%s model = %q, want gpt-5.6-sol", method, threadParams.Model)
			}

			turn, hasTurn := fs.sawCall("turn/start")
			if tc.prompt == "" {
				if hasTurn {
					t.Fatal("resume unexpectedly started an initial turn")
				}
				return
			}
			if !hasTurn {
				t.Fatal("initial prompt did not start a turn")
			}
			var turnParams struct {
				Model string `json:"model"`
			}
			if err := json.Unmarshal(turn.Params, &turnParams); err != nil {
				t.Fatal(err)
			}
			if turnParams.Model != "gpt-5.6-sol" {
				t.Fatalf("turn/start model = %q, want gpt-5.6-sol", turnParams.Model)
			}
		})
	}
}

func TestHandshakeResumeUsesConfiguredPolicy(t *testing.T) {
	for _, sandbox := range []string{"workspace-write", "read-only"} {
		t.Run(sandbox, func(t *testing.T) {
			sup, fs, client := newFakePair(t)
			defer fs.close()
			defer sup.Close()
			sup.cfg.ResumeThreadID = "thr_saved"
			sup.cfg.Sandbox = sandbox
			sup.cfg.ApprovalPolicy = "never"
			attach(t, sup, client)
			call, ok := fs.sawCall("thread/resume")
			if !ok {
				t.Fatal("no thread/resume call")
			}
			var params struct {
				Sandbox        string `json:"sandbox"`
				ApprovalPolicy string `json:"approvalPolicy"`
			}
			if err := json.Unmarshal(call.Params, &params); err != nil {
				t.Fatal(err)
			}
			if params.Sandbox != sandbox || params.ApprovalPolicy != "never" {
				t.Fatalf("resume lost configured permissions: %s", call.Params)
			}
		})
	}
}

func TestHandshakeResume(t *testing.T) {
	client, server := newMemPair()
	fs := &fakeServer{t: t, conn: server, respByID: map[string]chan incoming{}}
	go fs.loop()
	defer fs.close()
	sup := New(Config{SessionID: "s-test", Endpoint: "unix:///tmp/x.sock", ResumeThreadID: "thr_resumed"})
	defer sup.Close()
	attach(t, sup, client)

	if got := sup.ThreadID(); got != "thr_resumed" {
		t.Fatalf("threadID = %q, want thr_resumed", got)
	}
	if _, ok := fs.sawCall("thread/resume"); !ok {
		t.Fatal("no thread/resume call")
	}
	if _, ok := fs.sawCall("thread/start"); ok {
		t.Fatal("thread/start sent on a resume")
	}
	if _, ok := fs.sawCall("thread/name/set"); ok {
		t.Fatal("existing thread renamed during resume")
	}
}

// TestHandshakeResumeMissFallsBack checks the belt-and-suspenders backstop: if a
// resume is attempted and the server returns "no rollout found", the handshake
// falls back to thread/start and pins the new thread rather than failing (or
// leaving the thread in a state whose first turn hangs).
func TestHandshakeResumeMissFallsBack(t *testing.T) {
	client, server := newMemPair()
	fs := &fakeServer{t: t, conn: server, respByID: map[string]chan incoming{}, resumeErr: "no rollout found"}
	go fs.loop()
	defer fs.close()
	sup := New(Config{SessionID: "s-test", Endpoint: "unix:///tmp/x.sock", ResumeThreadID: "thr_gone"})
	defer sup.Close()
	attach(t, sup, client)

	if _, ok := fs.sawCall("thread/resume"); !ok {
		t.Fatal("expected a thread/resume attempt")
	}
	if _, ok := fs.sawCall("thread/start"); !ok {
		t.Fatal("resume miss did not fall back to thread/start")
	}
	if got := sup.ThreadID(); got != "thr_1" {
		t.Fatalf("threadID = %q, want thr_1 (the fresh thread from the fallback)", got)
	}
}

// Fresh initialization must persist an empty thread without spending a model
// turn, and preserve that same identity when the daemon restarts.
func TestFreshThreadReadyBeforeFirstTurn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	sup, fs, client := newFakePair(t)
	defer fs.close()
	defer sup.Close()
	attach(t, sup, client)
	if !sup.Identity().Resumable {
		t.Fatal("fresh thread not ready to resume")
	}
	for _, method := range []string{"thread/name/set", "thread/resume"} {
		call, ok := fs.sawCall(method)
		if !ok {
			t.Fatalf("missing %s", method)
		}
		var p struct {
			ThreadID     string `json:"threadId"`
			ExcludeTurns bool   `json:"excludeTurns"`
		}
		if err := json.Unmarshal(call.Params, &p); err != nil {
			t.Fatal(err)
		}
		if p.ThreadID != sup.ThreadID() || p.ExcludeTurns {
			t.Fatalf("%s does not prepare canonical full history: %s", method, call.Params)
		}
	}
	if _, ok := fs.sawCall("turn/start"); ok {
		t.Fatal("initialization spent a hidden model turn")
	}
	if err := SaveIdentity(sup.Identity()); err != nil {
		t.Fatal(err)
	}
	if got := resumeThreadFor("s-test", ""); got != sup.ThreadID() {
		t.Fatalf("restart lost empty thread: %q", got)
	}
}

func TestFreshThreadPreparationFailure(t *testing.T) {
	for _, method := range []string{"thread/name/set", "thread/resume"} {
		t.Run(method, func(t *testing.T) {
			client, server := newMemPair()
			fs := &fakeServer{t: t, conn: server, respByID: map[string]chan incoming{}, failMethod: method}
			go fs.loop()
			defer fs.close()
			sup := New(Config{SessionID: "failure"})
			defer sup.Close()
			if err := sup.attach(context.Background(), client); err == nil {
				t.Fatal("startup succeeded without persisted empty thread")
			}
			if sup.Identity().Resumable {
				t.Fatal("failed preparation advertised resumability")
			}
			if _, ok := fs.sawCall("turn/start"); ok {
				t.Fatal("failed preparation spent a model turn")
			}
		})
	}
}

// TestResumedIdentitySurvivesAnotherRestart is the ROOT #98 regression: a
// SUCCESSFUL resume must leave the session Resumable=true, so Manager.Ensure's
// post-Start SaveIdentity persists true (not false) and a second restart with no
// intervening turn still resumes the same thread rather than silently starting a
// new one (losing the conversation).
func TestResumedIdentitySurvivesAnotherRestart(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // SaveIdentity writes under HOME

	client, server := newMemPair()
	fs := &fakeServer{t: t, conn: server, respByID: map[string]chan incoming{}} // thread/resume succeeds
	go fs.loop()
	defer fs.close()
	sup := New(Config{SessionID: "cont", Endpoint: "unix:///tmp/x.sock", ResumeThreadID: "existing-history"})
	defer sup.Close()
	attach(t, sup, client)

	// A successful resume ⇒ resumable immediately (the thread has history), even
	// though no new turn ran this session.
	id := sup.Identity()
	if !id.Resumable {
		t.Fatalf("successful resume did not set Resumable: %+v", id)
	}
	if id.ThreadID != "existing-history" {
		t.Fatalf("resumed thread id = %q, want existing-history", id.ThreadID)
	}

	// Manager.Ensure persists identity right after Start — this must NOT clobber the
	// true back to false.
	if err := SaveIdentity(sup.Identity()); err != nil {
		t.Fatal(err)
	}

	// Restart #1 (no intervening turn): still resumes the same thread.
	if got := resumeThreadFor("cont", ""); got != "existing-history" {
		t.Fatalf("after resume+persist, resumeThreadFor = %q, want existing-history (conversation dropped!)", got)
	}
	// Restart #2 (still no turn): the persisted identity is stable, so it keeps
	// resuming — the two-restart case ROOT flagged.
	saved, _ := LoadIdentity("cont")
	if !saved.Resumable {
		t.Fatalf("persisted identity lost Resumable: %+v", saved)
	}
	if got := resumeThreadFor("cont", ""); got != "existing-history" {
		t.Fatalf("second restart resumeThreadFor = %q, want existing-history", got)
	}
}

// TestTurnEventsCarryCorrelation checks the producer-side shared-session invariant:
// the normalized turn_start and turn_end both carry the wire thread_id and turn_id,
// so the provider→browser path can pair them and know which thread/turn they name.
func TestTurnEventsCarryCorrelation(t *testing.T) {
	sup, fs, client := newFakePair(t)
	defer fs.close()
	defer sup.Close()
	attach(t, sup, client)
	ctx := context.Background()
	col := subscribeCollector(ctx, sup)

	fs.pushNotify("turn/started", map[string]any{"threadId": "thr_1", "turn": map[string]any{"id": "turn_9"}})
	start := col.waitFor(t, harnessproto.TypeTurnStart)
	fs.pushNotify("turn/completed", map[string]any{"threadId": "thr_1", "turn": map[string]any{"id": "turn_9", "status": "completed"}})
	end := col.waitFor(t, harnessproto.TypeTurnEnd)

	for _, ev := range []harnessproto.RuntimeEvent{start, end} {
		var p struct {
			ThreadID string `json:"thread_id"`
			TurnID   string `json:"turn_id"`
		}
		_ = json.Unmarshal(ev.Payload, &p)
		if p.ThreadID != "thr_1" || p.TurnID != "turn_9" {
			t.Fatalf("%s missing correlation ids: %s", ev.Type, ev.Payload)
		}
	}
}

// TestForeignThreadTurnNotTracked checks the cancel-target invariant: a turn/started
// for a DIFFERENT thread must not become this supervisor's tracked turn, so a Cancel
// never interrupts a foreign turn.
func TestForeignThreadTurnNotTracked(t *testing.T) {
	sup, fs, client := newFakePair(t) // pinned thread is thr_1
	defer fs.close()
	defer sup.Close()
	attach(t, sup, client)
	ctx := context.Background()

	fs.pushNotify("turn/started", map[string]any{"threadId": "other-thread", "turn": map[string]any{"id": "foreign"}})
	// Give the read loop a moment to process the notification.
	waitCall(t, fs, "initialize") // ensures the loop is running; initialize already happened
	time.Sleep(50 * time.Millisecond)

	if err := sup.Cancel(ctx); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	c, ok := fs.sawCall("turn/interrupt")
	if !ok {
		t.Fatal("no turn/interrupt")
	}
	var p struct {
		TurnID string `json:"turnId"`
	}
	_ = json.Unmarshal(c.Params, &p)
	if p.TurnID == "foreign" {
		t.Fatal("Cancel targeted a foreign thread's turn — cross-thread cancel leak")
	}
}

func TestPromptBracketsTurn(t *testing.T) {
	sup, fs, client := newFakePair(t)
	defer fs.close()
	defer sup.Close()
	attach(t, sup, client)
	sup.SetModel("gpt-5.6-sol") // runtime observation after the thread is live

	ctx := context.Background()
	col := subscribeCollector(ctx, sup)

	done := make(chan error, 1)
	go func() { done <- sup.Prompt(ctx, "hello") }()

	waitCall(t, fs, "turn/start")
	call, _ := fs.sawCall("turn/start")
	var turnParams struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(call.Params, &turnParams); err != nil {
		t.Fatal(err)
	}
	if turnParams.Model != "gpt-5.6-sol" {
		t.Fatalf("turn/start model = %q, want gpt-5.6-sol", turnParams.Model)
	}
	// The observed turn lifecycle brackets the turn (any origin).
	fs.pushTurnStarted()
	// Streamed text field is `delta` (pinned 0.153.4 AgentMessageDeltaNotification), not `text`.
	fs.pushNotify("item/agentMessage/delta", map[string]any{"itemId": "m1", "delta": "hi", "threadId": "thr_1", "turnId": "turn_1"})
	fs.completeTurn("completed")

	col.waitFor(t, harnessproto.TypeTurnStart)
	txt := col.waitFor(t, harnessproto.TypeText)
	if txt.ItemID != "m1" {
		t.Fatalf("text item id = %q", txt.ItemID)
	}
	// Non-empty streamed content must survive the supervisor event stream / coalescing — this
	// is what the provider bridge relays to the user (the dropped-text regression, AGE-179).
	var tp struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(txt.Payload, &tp)
	if tp.Text != "hi" {
		t.Fatalf("streamed assistant text lost through the supervisor stream: got %q, want %q", tp.Text, "hi")
	}
	end := col.waitFor(t, harnessproto.TypeTurnEnd)
	var ep struct {
		StopReason string `json:"stop_reason"`
	}
	_ = json.Unmarshal(end.Payload, &ep)
	if ep.StopReason != "completed" {
		t.Fatalf("turn_end stop_reason = %q, want completed", ep.StopReason)
	}
	if err := <-done; err != nil {
		t.Fatalf("Prompt returned %v", err)
	}
}

// TestObservedTurnFromAnyOrigin checks that a turn started by *another* client
// (only turn/started + turn/completed observed, no local Prompt) is still tracked
// and bracketed — so web steer/interrupt can target a TUI-initiated turn.
func TestObservedTurnFromAnyOrigin(t *testing.T) {
	sup, fs, client := newFakePair(t)
	defer fs.close()
	defer sup.Close()
	attach(t, sup, client)
	ctx := context.Background()
	col := subscribeCollector(ctx, sup)

	fs.mu.Lock()
	fs.turnID = "turn_tui"
	fs.mu.Unlock()
	fs.pushTurnStarted()

	col.waitFor(t, harnessproto.TypeTurnStart)
	// The supervisor tracked the TUI-origin turn, so Cancel targets it.
	if err := sup.Cancel(ctx); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	c, ok := fs.sawCall("turn/interrupt")
	if !ok {
		t.Fatal("no turn/interrupt")
	}
	var p struct {
		TurnID string `json:"turnId"`
	}
	_ = json.Unmarshal(c.Params, &p)
	if p.TurnID != "turn_tui" {
		t.Fatalf("interrupt targeted turn %q, want turn_tui (the observed TUI turn)", p.TurnID)
	}
}

func TestInterjectSteers(t *testing.T) {
	sup, fs, client := newFakePair(t)
	defer fs.close()
	defer sup.Close()
	attach(t, sup, client)

	// Establish an active turn first — Interject steers an in-flight turn and requires
	// its expectedTurnId.
	col := subscribeCollector(context.Background(), sup)
	fs.mu.Lock()
	fs.turnID = "turn_live"
	fs.mu.Unlock()
	fs.pushTurnStarted()
	col.waitFor(t, harnessproto.TypeTurnStart)

	if err := sup.Interject(context.Background(), "also do X"); err != nil {
		t.Fatalf("Interject: %v", err)
	}
	c, ok := fs.sawCall("turn/steer")
	if !ok {
		t.Fatal("no turn/steer call")
	}
	var p struct {
		ThreadID       string           `json:"threadId"`
		ExpectedTurnID string           `json:"expectedTurnId"`
		Input          []map[string]any `json:"input"`
	}
	_ = json.Unmarshal(c.Params, &p)
	if p.ThreadID != "thr_1" || len(p.Input) == 0 {
		t.Fatalf("steer params = %+v", p)
	}
	// The active turn's id must be carried so the server can correlate the steer.
	if p.ExpectedTurnID != "turn_live" {
		t.Fatalf("steer omitted/mismatched expectedTurnId: %+v", p)
	}
}

// Interject with no in-flight turn must fail fast with errNoActiveTurn and send NO
// turn/steer — a running process is not a running model turn, and a steer without
// expectedTurnId is malformed. It must never start or infer a turn.
func TestInterjectIdleFailsFastNoRPC(t *testing.T) {
	sup, fs, client := newFakePair(t)
	defer fs.close()
	defer sup.Close()
	attach(t, sup, client) // pinned thread, but no turn started

	if err := sup.Interject(context.Background(), "steer nothing"); err != errNoActiveTurn {
		t.Fatalf("idle Interject err = %v, want errNoActiveTurn", err)
	}
	if _, ok := fs.sawCall("turn/steer"); ok {
		t.Fatal("idle Interject sent a turn/steer despite no active turn")
	}
	if _, ok := fs.sawCall("turn/start"); ok {
		t.Fatal("idle Interject started a turn — it must never create one")
	}
}

// A completion that races Interject clears the tracked turn, so a steer arriving after
// the turn ended is rejected (stale) rather than steering a dead turn.
func TestInterjectRejectedAfterTurnCompletes(t *testing.T) {
	sup, fs, client := newFakePair(t)
	defer fs.close()
	defer sup.Close()
	attach(t, sup, client)

	col := subscribeCollector(context.Background(), sup)
	fs.mu.Lock()
	fs.turnID = "turn_live"
	fs.mu.Unlock()
	fs.pushTurnStarted()
	col.waitFor(t, harnessproto.TypeTurnStart)
	fs.completeTurn("completed")
	col.waitFor(t, harnessproto.TypeTurnEnd) // curTurn cleared by the completion

	if err := sup.Interject(context.Background(), "too late"); err != errNoActiveTurn {
		t.Fatalf("post-completion Interject err = %v, want errNoActiveTurn", err)
	}
}

func TestApprovalRoundTrip(t *testing.T) {
	sup, fs, client := newFakePair(t)
	defer fs.close()
	defer sup.Close()
	attach(t, sup, client)

	ctx := context.Background()
	col := subscribeCollector(ctx, sup)

	go func() {
		resp := fs.pushRequest("item/commandExecution/requestApproval", "ap1", map[string]any{
			"itemId": "it1", "threadId": "thr_1", "turnId": "turn_1", "command": "rm -rf x",
		})
		// The client answers with an OBJECT {decision:"accept"}, not a bare enum.
		var r struct {
			Decision string `json:"decision"`
		}
		_ = json.Unmarshal(resp.Result, &r)
		if r.Decision != "accept" {
			t.Errorf("approval response result = %s, want {decision:accept}", resp.Result)
		}
	}()

	req := col.waitFor(t, harnessproto.TypePermissionRequest)
	var rp struct {
		RequestID string `json:"request_id"`
		Tool      string `json:"tool"`
	}
	_ = json.Unmarshal(req.Payload, &rp)
	if rp.RequestID != "ap1" || rp.Tool != "command_execution" {
		t.Fatalf("permission_request payload = %s", req.Payload)
	}
	if open := sup.OpenApprovals(); len(open) != 1 || open[0] != "ap1" {
		t.Fatalf("open approvals = %v", open)
	}

	if err := sup.Resolve(ctx, "ap1", harnessproto.DecisionAllow); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(sup.OpenApprovals()) != 0 {
		t.Fatal("approval still open after Resolve")
	}
	// No speculative permission_resolved is emitted on a successful write; the
	// resolution is signalled by the server (turn end / item completion).
	if err := sup.Resolve(ctx, "ap1", harnessproto.DecisionAllow); err != errDuplicateApproval {
		t.Fatalf("duplicate Resolve err = %v, want duplicate", err)
	}
	if err := sup.Resolve(ctx, "nope", harnessproto.DecisionDeny); err != errStaleApproval {
		t.Fatalf("stale Resolve err = %v, want stale", err)
	}
}

func TestDisconnectMidTurnUnblocksPrompt(t *testing.T) {
	sup, fs, client := newFakePair(t)
	defer sup.Close()
	attach(t, sup, client)

	ctx := context.Background()
	done := make(chan error, 1)
	go func() { done <- sup.Prompt(ctx, "long task") }()
	waitCall(t, fs, "turn/start")
	fs.close()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Prompt hung after mid-turn disconnect")
	}
}

func TestUnknownServerRequestAnswered(t *testing.T) {
	sup, fs, client := newFakePair(t)
	defer fs.close()
	defer sup.Close()
	attach(t, sup, client)

	resp := fs.pushRequest("weird/serverRequest", "x1", map[string]any{})
	if resp.Error == nil {
		t.Fatalf("unknown server request not answered with an error: %+v", resp)
	}
}

func TestUserInputNotAutoAnswered(t *testing.T) {
	sup, fs, client := newFakePair(t)
	defer fs.close()
	defer sup.Close()
	attach(t, sup, client)
	ctx := context.Background()
	col := subscribeCollector(ctx, sup)

	// A user-input request is surfaced but NOT auto-answered (another client may).
	rawID, _ := json.Marshal("ui1")
	ch := make(chan incoming, 1)
	fs.mu.Lock()
	fs.respByID[string(rawID)] = ch
	fs.mu.Unlock()
	fs.write(map[string]any{"method": "tool/requestUserInput", "id": "ui1",
		"params": map[string]any{"questions": []map[string]any{{"question": "which?"}}}})

	col.waitFor(t, harnessproto.TypeNotice)
	select {
	case r := <-ch:
		t.Fatalf("user-input was auto-answered: %+v", r)
	case <-time.After(300 * time.Millisecond):
		// good: left open for another client
	}
}

func waitCall(t *testing.T, fs *fakeServer, method string) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		if _, ok := fs.sawCall(method); ok {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("client never sent %q", method)
		case <-time.After(5 * time.Millisecond):
		}
	}
}
