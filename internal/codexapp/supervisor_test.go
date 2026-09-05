package codexapp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// collector drains a subscription into a slice, so a test can wait for and assert
// specific event types without racing the read loop.
type collector struct {
	ch <-chan harnessproto.RuntimeEventBatch
}

func subscribeCollector(ctx context.Context, s *Supervisor) *collector {
	return &collector{ch: s.Subscribe(ctx, 0)}
}

// waitFor reads batches until an event of type typ arrives (returns it) or the
// deadline passes (fails).
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

func attach(t *testing.T, sup *Supervisor, client interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
	Close() error
}) {
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
	if _, ok := fs.sawCall("thread/resume"); ok {
		t.Fatal("thread/resume sent for a fresh start")
	}
	id := sup.Identity()
	if id.ControlMode != harnessproto.ControlModeStructured || id.ThreadID != "thr_1" {
		t.Fatalf("identity = %+v", id)
	}
}

func TestHandshakeResume(t *testing.T) {
	client, server := newRawPair(t)
	fs := &fakeServer{t: t, conn: server, respByID: map[string]chan incoming{}}
	go fs.loop()
	defer fs.close()
	sup := New(Config{SessionID: "s-test", ResumeThreadID: "thr_resumed"})
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

	col.waitFor(t, harnessproto.TypeTurnStart)

	// Server streams a text item, then completes the turn.
	fs.pushNotify("item/agentMessage/delta", map[string]any{"itemId": "m1", "text": "hi"})
	// Wait until turn/start has been seen so the turn id is set, then complete.
	waitCall(t, fs, "turn/start")
	fs.completeTurn("completed")

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

func TestCancelInterrupts(t *testing.T) {
	sup, fs, client := newFakePair(t)
	defer fs.close()
	defer sup.Close()
	attach(t, sup, client)

	if err := sup.Cancel(context.Background()); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if _, ok := fs.sawCall("turn/interrupt"); !ok {
		t.Fatal("no turn/interrupt call")
	}
}

func TestApprovalRoundTrip(t *testing.T) {
	sup, fs, client := newFakePair(t)
	defer fs.close()
	defer sup.Close()
	attach(t, sup, client)

	ctx := context.Background()
	col := subscribeCollector(ctx, sup)

	// Server raises an approval request; the supervisor answers via Resolve.
	go fs.pushRequest("item/commandExecution/requestApproval", "ap1", map[string]any{
		"itemId": "it1", "threadId": "thr_1", "turnId": "turn_1", "command": "rm -rf x",
	})

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
	col.waitFor(t, harnessproto.TypePermissionResolved)
	if len(sup.OpenApprovals()) != 0 {
		t.Fatal("approval still open after Resolve")
	}

	// Duplicate reply is rejected as duplicate; unknown id as stale.
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

	// The server dies mid-turn; the pending Prompt must not hang.
	fs.close()

	select {
	case <-done:
		// Unblocked — the property under test (turn_end stop_reason may race the hub
		// teardown, so we assert only that Prompt returns).
	case <-time.After(3 * time.Second):
		t.Fatal("Prompt hung after mid-turn disconnect")
	}
}

func TestUnknownServerRequestAnswered(t *testing.T) {
	sup, fs, client := newFakePair(t)
	defer fs.close()
	defer sup.Close()
	attach(t, sup, client)

	// An unknown server request must be answered (method-not-found) so the server
	// does not hang, and recorded as raw so nothing is dropped.
	resp := fs.pushRequest("weird/serverRequest", "x1", map[string]any{})
	if resp.Error == nil {
		t.Fatalf("unknown server request not answered with an error: %+v", resp)
	}
}

// waitCall polls until the fake has seen a client call for method (the server's
// handleCall is async from the client's call() returning).
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
