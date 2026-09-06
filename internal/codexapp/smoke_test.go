package codexapp

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// TestSmokeRealAppServer drives the supervisor against an ACTUAL `codex app-server`
// over a Unix WebSocket in a throwaway sandbox. It is OPT-IN and self-skipping:
// AMUX_CODEX_APP_SERVER_SMOKE=1 plus a `codex` on PATH; absent, it SKIPS (a skip is
// not verification).
//
// It separates TRANSPORT/LIFECYCLE success from MODEL success:
//   - transport (asserted): the WebSocket handshake + initialize → thread/start
//     returns a thread id; a turn is bracketed (turn_start … turn_end) on the
//     normalized stream; a SECOND client completes its own initialize on the same
//     listener (the multi-client property native `--remote` attach relies on).
//   - model (reported, not asserted): whether the turn produced assistant text and
//     ended cleanly. Without credentials the turn ends in error — that is a model
//     outcome, not a transport failure, so it is logged; the test does not "pass a
//     model turn" it never had. If the turn errors we say so explicitly.
//
// It does NOT claim the native CLI `codex --remote <endpoint> resume <id>` attaches
// to this running thread — that is covered separately (TestSmokeEmptyThreadResume
// proves the empty-thread fallback identity; the native TUI attach is a host item).
func TestSmokeRealAppServer(t *testing.T) {
	bin := requireSmokeCodex(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	sandbox := t.TempDir()
	endpoint := "unix://" + filepath.Join(sandbox, "app.sock")
	t.Logf("app-server argv: %v", AppServerArgv(bin, endpoint))

	sup := New(Config{SessionID: "smoke", Bin: bin, Dir: sandbox, Endpoint: endpoint})
	if err := sup.Start(ctx, nil); err != nil {
		t.Fatalf("TRANSPORT: Start (spawn app-server + WebSocket handshake): %v", err)
	}
	defer sup.Close()

	threadID := sup.ThreadID()
	if threadID == "" {
		t.Fatal("TRANSPORT: real binary returned an empty thread id")
	}
	t.Logf("TRANSPORT ok: handshake, thread id = %s", threadID)

	// A second client must complete its OWN initialize handshake on the same
	// listener — not merely open a socket. This is the real multi-client check.
	if err := secondClientInitializes(ctx, endpoint); err != nil {
		t.Fatalf("TRANSPORT: second client failed to initialize on the same listener: %v", err)
	}
	t.Log("TRANSPORT ok: second client initialized on the same listener")

	// Collect events race-free; signal turnEnd via a channel instead of sleeping.
	col := newSmokeCollector(sup.Subscribe(ctx, 0))
	defer col.stop()

	pctx, pcancel := context.WithTimeout(ctx, 90*time.Second)
	defer pcancel()
	promptErr := sup.Prompt(pctx, "Reply with the single word: pong")

	end, ok := col.awaitTurnEnd(10 * time.Second)
	starts, ends, text := col.counts()
	if starts == 0 || !ok {
		t.Fatalf("TRANSPORT: turn not bracketed against the real binary (turn_start=%d turn_end=%d)", starts, ends)
	}
	t.Logf("TRANSPORT ok: turn bracketed (turn_start=%d turn_end=%d)", starts, ends)

	// MODEL outcome — reported, never a transport pass/fail. Only claim model
	// success on a clean end with assistant text; otherwise say it failed and why.
	var ep struct {
		StopReason string `json:"stop_reason"`
	}
	_ = json.Unmarshal(end.Payload, &ep)
	switch {
	case promptErr != nil:
		t.Logf("MODEL: turn did not complete (%v) — expected without credentials; transport is still proven", promptErr)
	case ep.StopReason != "completed":
		t.Logf("MODEL: turn ended stop_reason=%q with %d text event(s) — not a clean model success (likely no credentials)", ep.StopReason, text)
	case text == 0:
		t.Logf("MODEL: turn completed but produced no assistant text — not asserting model success")
	default:
		t.Logf("MODEL ok: turn completed with %d text event(s)", text)
	}
}

// TestSmokeEmptyThreadResume proves the empty-thread resume fallback against the
// real binary and, crucially, that it does not split identities or discard
// conversation: a pinned thread that never ran a turn has no rollout, so
// `thread/resume` fails ("no rollout found") and the supervisor adopts a NEW thread
// id — which becomes the single source of truth (ThreadID/Identity), so every
// client (web via amux, native via AttachCommand) uses the same id. No conversation
// is lost because the resumed thread was empty.
func TestSmokeEmptyThreadResume(t *testing.T) {
	bin := requireSmokeCodex(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// 1) Fresh server → thread T1, never run a turn (so no rollout is written).
	sandbox := t.TempDir()
	endpoint := "unix://" + filepath.Join(sandbox, "a.sock")
	s1 := New(Config{SessionID: "empty", Bin: bin, Dir: sandbox, Endpoint: endpoint})
	if err := s1.Start(ctx, nil); err != nil {
		t.Fatalf("start s1: %v", err)
	}
	t1 := s1.ThreadID()
	_ = s1.Close()
	if t1 == "" {
		t.Fatal("empty thread id from s1")
	}

	// 2) A fresh supervisor pinned to resume T1. The real binary returns "no rollout
	// found"; the supervisor must fall back to a new thread rather than fail.
	endpoint2 := "unix://" + filepath.Join(sandbox, "b.sock")
	s2 := New(Config{SessionID: "empty", Bin: bin, Dir: sandbox, Endpoint: endpoint2, ResumeThreadID: t1})
	if err := s2.Start(ctx, nil); err != nil {
		t.Fatalf("start s2 (resume of empty thread must fall back, not fail): %v", err)
	}
	defer s2.Close()
	t2 := s2.ThreadID()

	if t2 == "" {
		t.Fatal("no thread id after empty-thread resume fallback")
	}
	if t2 == t1 {
		t.Logf("NOTE: resume of the empty thread %s succeeded (binary tolerated it); id preserved", t1)
	} else {
		t.Logf("empty-thread resume fell back: %s → %s (new id adopted)", t1, t2)
	}
	// Single source of truth: Identity and ThreadID agree, so a native attach
	// (AttachCommand uses ThreadID) and the web (via the persisted Identity) target
	// the SAME thread — no split.
	if id := s2.Identity(); id.ThreadID != t2 {
		t.Fatalf("identity split: Identity.ThreadID=%q but ThreadID()=%q", id.ThreadID, t2)
	}
}

// TestSmokeTurnAfterResumeMiss is the AGE-198 assertion: a FIRST turn completes
// after a pre-turn resume miss. It pins a never-run thread, lets `thread/resume`
// miss (→ fallback), then runs a turn against the real binary and requires it to
// bracket (turn_start … turn_end) — i.e. the failed resume did not poison the turn.
func TestSmokeTurnAfterResumeMiss(t *testing.T) {
	bin := requireSmokeCodex(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	sandbox := t.TempDir()

	// A never-run thread (no rollout).
	s1 := New(Config{SessionID: "rm", Bin: bin, Dir: sandbox, Endpoint: "unix://" + filepath.Join(sandbox, "a.sock")})
	if err := s1.Start(ctx, nil); err != nil {
		t.Fatalf("s1: %v", err)
	}
	t1 := s1.ThreadID()
	_ = s1.Close()

	// Resume it (miss → fallback), then run the first turn.
	s2 := New(Config{SessionID: "rm", Bin: bin, Dir: sandbox, Endpoint: "unix://" + filepath.Join(sandbox, "b.sock"), ResumeThreadID: t1})
	if err := s2.Start(ctx, nil); err != nil {
		t.Fatalf("s2 start after resume miss: %v", err)
	}
	defer s2.Close()

	col := newSmokeCollector(s2.Subscribe(ctx, 0))
	pctx, pc := context.WithTimeout(ctx, 60*time.Second)
	defer pc()
	_ = s2.Prompt(pctx, "reply: pong")
	_, ended := col.awaitTurnEnd(10 * time.Second)
	starts, ends, _ := col.counts()
	if starts == 0 || !ended {
		t.Fatalf("first turn after a resume miss did not complete (turn_start=%d turn_end=%d) — the failed resume poisoned the turn", starts, ends)
	}
	t.Logf("turn completed after a pre-turn resume miss (turn_start=%d turn_end=%d)", starts, ends)
}

// TestSmokeLoopbackTransport proves the Origin fix: with gorilla/websocket omitting
// Origin, the amux client now completes the handshake against codex's LOOPBACK ws
// listener (which 403s any Origin, and x/net always sent one).
func TestSmokeLoopbackTransport(t *testing.T) {
	bin := requireSmokeCodex(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	ep, err := LoopbackEndpoint()
	if err != nil {
		t.Fatalf("loopback endpoint: %v", err)
	}
	t.Logf("loopback endpoint: %s", ep)
	sup := New(Config{SessionID: "lb", Bin: bin, Dir: t.TempDir(), Endpoint: ep})
	if err := sup.Start(ctx, nil); err != nil {
		t.Fatalf("loopback ws handshake against real codex (Origin must be omitted): %v", err)
	}
	defer sup.Close()
	if sup.ThreadID() == "" {
		t.Fatal("no thread id over loopback ws")
	}
	t.Logf("loopback ws OK: handshake completed, thread id = %s", sup.ThreadID())
}

// TestSmokeResumeAfterTurnKeepsThread is the end-to-end continuity proof for the
// ROOT #98 regression, against the real binary: a thread that ran a turn writes a
// rollout and becomes Resumable; a later launch pinned to it **resumes the SAME
// thread** (not a fresh one) and stays Resumable — so a conversation survives
// restarts even with no new turn in between.
func TestSmokeResumeAfterTurnKeepsThread(t *testing.T) {
	bin := requireSmokeCodex(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	sandbox := t.TempDir()

	s1 := New(Config{SessionID: "cont", Bin: bin, Dir: sandbox, Endpoint: "unix://" + filepath.Join(sandbox, "a.sock")})
	if err := s1.Start(ctx, nil); err != nil {
		t.Fatalf("s1: %v", err)
	}
	t1 := s1.ThreadID()
	pctx, pc := context.WithTimeout(ctx, 30*time.Second)
	_ = s1.Prompt(pctx, "hello") // errors without creds, but writes a rollout
	pc()
	if !s1.Identity().Resumable {
		t.Fatal("thread not marked Resumable after a turn")
	}
	_ = s1.Close()

	// A fresh launch pinned to t1 must resume the SAME thread (rollout exists).
	s2 := New(Config{SessionID: "cont", Bin: bin, Dir: sandbox, Endpoint: "unix://" + filepath.Join(sandbox, "b.sock"), ResumeThreadID: t1})
	if err := s2.Start(ctx, nil); err != nil {
		t.Fatalf("s2 resume: %v", err)
	}
	defer s2.Close()
	if s2.ThreadID() != t1 {
		t.Fatalf("resume did not keep the thread: %s → %s (conversation would be lost)", t1, s2.ThreadID())
	}
	if !s2.Identity().Resumable {
		t.Fatal("resumed thread lost Resumable — a further restart would drop it")
	}
	t.Logf("continuity OK: resumed the same thread %s, still Resumable", t1)
}

// ── helpers ──────────────────────────────────────────────────────────────────

func requireSmokeCodex(t *testing.T) string {
	t.Helper()
	if os.Getenv("AMUX_CODEX_APP_SERVER_SMOKE") == "" {
		t.Skip("live smoke disabled; set AMUX_CODEX_APP_SERVER_SMOKE=1 (with a pinned codex on PATH) to run")
	}
	bin, err := exec.LookPath("codex")
	if err != nil {
		t.Skipf("codex not on PATH: %v", err)
	}
	verOut, verErr := exec.Command(bin, "--version").CombinedOutput()
	t.Logf("codex: %s — %s (err=%v)", bin, strings.TrimSpace(string(verOut)), verErr)
	return bin
}

// secondClientInitializes opens a second WebSocket to the same listener and
// completes an initialize round-trip, proving the listener multiplexes real
// clients (not just accepts a socket).
func secondClientInitializes(ctx context.Context, endpoint string) error {
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := dialWS(dctx, endpoint, "")
	if err != nil {
		return err
	}
	defer conn.Close()
	req, _ := json.Marshal(map[string]any{
		"method": "initialize", "id": 1,
		"params": map[string]any{
			"clientInfo":   map[string]any{"name": "amux-smoke-2", "title": "second", "version": "1"},
			"capabilities": map[string]any{"experimentalApi": true},
		},
	})
	if err := conn.WriteMessage(req); err != nil {
		return err
	}
	msg, err := conn.ReadMessage()
	if err != nil {
		return err
	}
	var resp struct {
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(msg, &resp); err != nil {
		return err
	}
	if len(resp.Error) > 0 {
		return &rpcError{Message: string(resp.Error)}
	}
	return nil
}

// smokeCollector drains a subscription with a mutex (race-free) and signals when a
// turn_end is seen, so the test waits on a condition rather than sleeping.
type smokeCollector struct {
	mu             sync.Mutex
	starts, ends   int
	text           int
	lastEnd        harnessproto.RuntimeEvent
	turnEnded      chan struct{}
	endedOnce      sync.Once
	cancelSubclose func()
	done           chan struct{}
}

func newSmokeCollector(ch <-chan harnessproto.RuntimeEventBatch) *smokeCollector {
	c := &smokeCollector{turnEnded: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(c.done)
		for b := range ch {
			for _, e := range b.Events {
				c.mu.Lock()
				switch e.Type {
				case harnessproto.TypeTurnStart:
					c.starts++
				case harnessproto.TypeTurnEnd:
					c.ends++
					c.lastEnd = e
				case harnessproto.TypeText:
					c.text++
				}
				c.mu.Unlock()
				if e.Type == harnessproto.TypeTurnEnd {
					c.endedOnce.Do(func() { close(c.turnEnded) })
				}
			}
		}
	}()
	return c
}

func (c *smokeCollector) awaitTurnEnd(d time.Duration) (harnessproto.RuntimeEvent, bool) {
	select {
	case <-c.turnEnded:
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.lastEnd, true
	case <-time.After(d):
		return harnessproto.RuntimeEvent{}, false
	}
}

func (c *smokeCollector) counts() (starts, ends, text int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.starts, c.ends, c.text
}

func (c *smokeCollector) stop() {}
