package codexapp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"amux/internal/launchenv"
)

// TestSmokeGoalAutomatic is the end-to-end proof of the behaviour the goal
// coordinator is built on, against the REAL Codex App Server and the REAL goal
// API — no fake server, no mocked goal calls, and deliberately no native TUI
// attached to the thread at any point.
//
// The claim under test has three parts, and only a live runtime can settle them:
//
//  1. A user's task becomes the session's native goal AUTOMATICALLY. amux's own
//     observer (Config.Goals ⇒ observeUserTask/establishGoal) does it from the
//     userMessage the App Server broadcasts for an ordinary prompt. Nothing here
//     calls thread/goal/set to establish it, and the model is given no
//     create_goal/update_goal opportunity in that phase, so an established goal
//     carrying the user's exact words can only have come from amux.
//  2. Codex then CONTINUES that goal headlessly: further model turns happen with
//     no second prompt, no TUI attach, and no further host RPC of any kind.
//  3. The user — and only the user — stops it. An explicit pause through the
//     same control the `goal` verb uses quiets the loop; a completed goal does
//     not bleed its accounting into the next task, which starts a fresh goal.
//
// The model is a local HTTP fixture: no credentials and no network. The endpoint
// is loopback rather than a unix socket so the path length of the sandbox's
// TMPDIR cannot push it past Darwin's 104-byte sun_path limit.
func TestSmokeGoalAutomatic(t *testing.T) {
	bin := requireSmokeCodex(t)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	model := newGoalFixtureModel(t)
	defer model.Close()

	dir := t.TempDir()
	home := filepath.Join(dir, "codex")
	t.Setenv("HOME", dir)
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf("model_provider = 'fixture'\nmodel = 'gpt-5.6'\n[features]\ngoals = true\n"+
		"[model_providers.fixture]\nname = 'fixture'\nbase_url = %q\nwire_api = 'responses'\nenv_key = 'OPENAI_API_KEY'\n"+
		"[projects.%q]\ntrust_level = 'trusted'\n", model.URL, dir)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	endpoint, err := LoopbackEndpoint()
	if err != nil {
		t.Fatalf("loopback endpoint: %v", err)
	}
	access, _ := launchenv.ForModelEnvironment(launchenv.CodexModelAccount, []string{"OPENAI_API_KEY=synthetic"})

	// Goals: true is the whole difference between a coordinator and an ordinary
	// Codex agent. Everything else is an ordinary supervised session.
	s := New(Config{
		SessionID: "goal-headless", Bin: bin, Dir: dir, Env: []string{"CODEX_HOME=" + home},
		ModelAccess: access, Endpoint: endpoint, Goals: true,
		EventLogPath: filepath.Join(dir, "events.log"),
	})
	if err := s.Start(ctx, nil); err != nil {
		t.Fatalf("start goal session against real codex: %v", err)
	}
	defer s.Close()
	col := newSmokeCollector(s.Subscribe(ctx, 0))
	defer col.stop()

	if goal, err := s.readGoal(ctx); err != nil {
		t.Fatalf("read goal on a fresh goal session: %v", err)
	} else if goal != nil {
		t.Fatalf("fresh thread already carries a goal: %+v", goal)
	}

	// ── 1. the user's task becomes the goal, with no host goal call ──────────
	const task = "Reconcile the ledger exports and report the first mismatching row"
	if err := s.Prompt(ctx, task); err != nil {
		t.Fatalf("prompt the goal session: %v", err)
	}
	goal := awaitLiveGoal(t, ctx, s, 20*time.Second, func(g *threadGoal) bool {
		return g != nil && g.Status == GoalActive
	}, "the user's task to become an active goal")
	if goal.Objective != task {
		t.Fatalf("goal objective = %q, want the user's task %q", goal.Objective, task)
	}
	// The fixture answers every request with one assistant message and nothing
	// else — it never emits a create_goal or update_goal call — so a goal
	// carrying the user's exact words can only be amux's doing.
	t.Logf("goal established automatically from the prompt: objective=%q status=%s", goal.Objective, goal.Status)

	// ── 2. Codex continues it headlessly ────────────────────────────────────
	// From here to the pause the test issues NOTHING: no prompt, no TUI attach,
	// no RPC. Every further model request and turn is the runtime pursuing the
	// goal on its own.
	startsBefore, _, _ := col.counts()
	requestsBefore := model.requests()
	// Enough continuation turns that the goal has clearly accumulated usage of
	// its own, so the fresh-accounting comparison at the end has real separation
	// rather than a one-turn margin.
	const continuationTurns = 6
	if !waitUntil(40*time.Second, func() bool {
		starts, _, _ := col.counts()
		return starts >= startsBefore+continuationTurns && model.requests() >= requestsBefore+continuationTurns
	}) {
		starts, ends, _ := col.counts()
		t.Fatalf("no headless continuation: turns %d→%d (ends %d), model requests %d→%d — the goal did not drive work without a second prompt",
			startsBefore, starts, ends, requestsBefore, model.requests())
	}
	starts, _, _ := col.counts()
	t.Logf("headless continuation: %d further turn(s) and %d further model request(s) with no prompt, no attach, no RPC",
		starts-startsBefore, model.requests()-requestsBefore)
	if live, err := s.readGoal(ctx); err != nil || live == nil || live.Status != GoalActive {
		t.Fatalf("goal during continuation = %+v (err=%v), want it still active", live, err)
	}

	// ── 3a. the user pauses, and the loop quiets ────────────────────────────
	if err := s.SetGoal(ctx, GoalUpdate{Status: GoalPaused}); err != nil {
		t.Fatalf("pause the goal (the user's control): %v", err)
	}
	awaitLiveGoal(t, ctx, s, 15*time.Second, func(g *threadGoal) bool {
		return g != nil && g.Status == GoalPaused
	}, "the goal to read back as paused")
	// Let any turn already in flight finish, then require a quiet window: a
	// paused goal must not start another one.
	settled := quiesce(20*time.Second, func() (int, int) {
		starts, _, _ := col.counts()
		return starts, model.requests()
	})
	if !settled {
		t.Fatal("work kept starting after the user paused the goal")
	}
	quietStarts, _, _ := col.counts()
	quietRequests := model.requests()
	time.Sleep(6 * time.Second)
	if starts, _, _ := col.counts(); starts != quietStarts || model.requests() != quietRequests {
		t.Fatalf("paused goal resumed itself: turns %d→%d, requests %d→%d",
			quietStarts, starts, quietRequests, model.requests())
	}
	paused, err := s.readGoal(ctx)
	if err != nil || paused == nil {
		t.Fatalf("read paused goal: %+v (err=%v)", paused, err)
	}
	t.Logf("user pause quiets the loop: no new turn or model request over a %s window after %d turn(s), tokensUsed=%d",
		6*time.Second, quietStarts, paused.TokensUsed)

	// ── 3b. a completed goal does not lend its accounting to the next task ──
	if err := s.SetGoal(ctx, GoalUpdate{Status: GoalComplete}); err != nil {
		t.Fatalf("complete the goal (the user's control): %v", err)
	}
	awaitLiveGoal(t, ctx, s, 15*time.Second, func(g *threadGoal) bool {
		return g != nil && g.Status == GoalComplete
	}, "the goal to read back as complete")

	const next = "Draft the migration note for the reconciled ledger"
	if err := s.Prompt(ctx, next); err != nil {
		t.Fatalf("prompt after completion: %v", err)
	}
	fresh := awaitLiveGoal(t, ctx, s, 20*time.Second, func(g *threadGoal) bool {
		return g != nil && g.Status == GoalActive && g.Objective == next
	}, "the next task to become a fresh active goal")
	if fresh.CreatedAt == paused.CreatedAt {
		t.Fatalf("the next task reused the finished goal (createdAt %d)", fresh.CreatedAt)
	}
	// fresh is the FIRST reading in which the new goal exists, and the fixture
	// paces its answers, so at most a turn or two of the new goal is accounted by
	// now while the finished goal ran continuationTurns+ of them. Require that
	// separation explicitly rather than trusting the timing.
	if paused.TokensUsed < continuationTurns*fixtureTokensPerTurn {
		t.Fatalf("finished goal only accounted %d tokens over %d turns; the comparison below would prove nothing",
			paused.TokensUsed, continuationTurns)
	}
	if fresh.TokensUsed >= paused.TokensUsed {
		t.Fatalf("fresh goal inherited the finished goal's accounting: tokensUsed %d, finished goal had %d",
			fresh.TokensUsed, paused.TokensUsed)
	}
	t.Logf("next task started a fresh goal: objective=%q tokensUsed=%d (finished goal had %d)",
		fresh.Objective, fresh.TokensUsed, paused.TokensUsed)

	// Leave the session quiet rather than tearing down mid-continuation.
	if err := s.SetGoal(ctx, GoalUpdate{Status: GoalPaused}); err != nil {
		t.Logf("final pause: %v", err)
	}
}

// awaitLiveGoal polls the goal through readGoal — a real thread/goal/get against
// the running server — until cond holds. readGoal keeps a concurrent
// notification's fresher state rather than overwriting it with the reply it
// raced, so what comes back is the runtime's own view either way; the
// supervisor's observed copy is checked to agree with it, since a status surface
// that disagreed would show a live coordinator the wrong thing.
func awaitLiveGoal(t *testing.T, ctx context.Context, s *Supervisor, d time.Duration, cond func(*threadGoal) bool, what string) *threadGoal {
	t.Helper()
	deadline := time.Now().Add(d)
	var last *threadGoal
	for time.Now().Before(deadline) {
		goal, err := s.readGoal(ctx)
		if err == nil && cond(goal) {
			observed, ok := s.Goal()
			if !ok || observed.Objective != goal.Objective || observed.Status != goal.Status {
				t.Fatalf("supervisor observed %+v (ok=%v) but the runtime holds %+v", observed, ok, goal)
			}
			return goal
		}
		last = goal
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s; last goal = %+v", d, what, last)
	return nil
}

// quiesce waits for two consecutive identical readings of (turns, requests),
// i.e. for whatever was in flight to finish.
func quiesce(d time.Duration, counts func() (int, int)) bool {
	deadline := time.Now().Add(d)
	lastA, lastB := counts()
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		a, b := counts()
		if a == lastA && b == lastB {
			return true
		}
		lastA, lastB = a, b
	}
	return false
}

func waitUntil(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return cond()
}

// fixtureTokensPerTurn is the usage the fixture reports for every turn, which is
// what the goal's accounting accumulates.
const fixtureTokensPerTurn = 1000

// fixturePace is how long the fixture takes to answer. A model that answered
// instantly would let the runtime burn dozens of continuation turns between two
// reads, which makes any statement about a goal's accumulated usage a race. A
// deliberate pace keeps each phase's turn count close to what the test asked
// for, and costs the smoke a second or two overall.
const fixturePace = 200 * time.Millisecond

// goalFixtureModel is the local model the smoke runs against: every request is
// answered with one short assistant message and nothing else — never a
// create_goal or update_goal call — so the only thing that can create or change
// a goal in this test is amux or the user.
type goalFixtureModel struct {
	*httptest.Server
	mu    sync.Mutex
	count int
}

func newGoalFixtureModel(t *testing.T) *goalFixtureModel {
	t.Helper()
	m := &goalFixtureModel{}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 8<<20))
		m.mu.Lock()
		m.count++
		n := m.count
		m.mu.Unlock()
		select {
		case <-time.After(fixturePace):
		case <-r.Context().Done():
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		id := fmt.Sprintf("resp_%d", n)
		item := map[string]any{
			"type": "message", "id": fmt.Sprintf("msg_%d", n), "role": "assistant", "status": "completed",
			"content": []map[string]any{{"type": "output_text", "text": fmt.Sprintf("Checked batch %d.", n)}},
		}
		for _, ev := range []map[string]any{
			{"type": "response.created", "response": map[string]any{"id": id}},
			{"type": "response.output_item.done", "item": item},
			{"type": "response.completed", "response": map[string]any{
				"id": id,
				"usage": map[string]any{
					"input_tokens": 700, "output_tokens": 300, "total_tokens": fixtureTokensPerTurn,
					"input_tokens_details":  map[string]any{"cached_tokens": 0},
					"output_tokens_details": map[string]any{"reasoning_tokens": 0},
				},
			}},
		} {
			b, _ := json.Marshal(ev)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev["type"], b)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	return m
}

func (m *goalFixtureModel) requests() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.count
}
