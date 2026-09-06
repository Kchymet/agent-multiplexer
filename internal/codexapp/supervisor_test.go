package codexapp

import (
	"context"
	"encoding/json"
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
	if got := resumeThreadFor("s-test"); got != sup.ThreadID() {
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
	if got := resumeThreadFor("cont"); got != "existing-history" {
		t.Fatalf("after resume+persist, resumeThreadFor = %q, want existing-history (conversation dropped!)", got)
	}
	// Restart #2 (still no turn): the persisted identity is stable, so it keeps
	// resuming — the two-restart case ROOT flagged.
	saved, _ := LoadIdentity("cont")
	if !saved.Resumable {
		t.Fatalf("persisted identity lost Resumable: %+v", saved)
	}
	if got := resumeThreadFor("cont"); got != "existing-history" {
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

	ctx := context.Background()
	col := subscribeCollector(ctx, sup)

	done := make(chan error, 1)
	go func() { done <- sup.Prompt(ctx, "hello") }()

	waitCall(t, fs, "turn/start")
	// The observed turn lifecycle brackets the turn (any origin).
	fs.pushTurnStarted()
	fs.pushNotify("item/agentMessage/delta", map[string]any{"itemId": "m1", "text": "hi"})
	fs.completeTurn("completed")

	col.waitFor(t, harnessproto.TypeTurnStart)
	txt := col.waitFor(t, harnessproto.TypeText)
	if txt.ItemID != "m1" {
		t.Fatalf("text item id = %q", txt.ItemID)
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

	if err := sup.Interject(context.Background(), "also do X"); err != nil {
		t.Fatalf("Interject: %v", err)
	}
	c, ok := fs.sawCall("turn/steer")
	if !ok {
		t.Fatal("no turn/steer call")
	}
	var p struct {
		ThreadID string           `json:"threadId"`
		Input    []map[string]any `json:"input"`
	}
	_ = json.Unmarshal(c.Params, &p)
	if p.ThreadID != "thr_1" || len(p.Input) == 0 {
		t.Fatalf("steer params = %+v", p)
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
