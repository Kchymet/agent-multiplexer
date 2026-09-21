package codexapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"amux/internal/agent"
)

// RestartWork is host-owned evidence of work running before shutdown. A goal's
// identity is retained without copying its objective into the restart journal.
type RestartWork struct {
	ThreadID string `json:"threadId"`
	GoalKey  string `json:"goalKey,omitempty"`
	Continue bool   `json:"continue,omitempty"`
}

// threadGoal mirrors the App Server's ThreadGoal. Codex owns every field: the
// objective, the status machine (active | paused | blocked | usageLimited |
// budgetLimited | complete), the optional token budget and the usage counters.
type threadGoal struct {
	ThreadID        string `json:"threadId"`
	Objective       string `json:"objective"`
	Status          string `json:"status"`
	TokenBudget     *int64 `json:"tokenBudget"`
	TokensUsed      int64  `json:"tokensUsed"`
	TimeUsedSeconds int64  `json:"timeUsedSeconds"`
	CreatedAt       int64  `json:"createdAt"`
	UpdatedAt       int64  `json:"updatedAt"`
}

func (g *threadGoal) key() string {
	if g == nil {
		return ""
	}
	data, _ := json.Marshal([]any{g.ThreadID, g.Objective, g.CreatedAt})
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

// Native goal statuses (App Server ThreadGoalStatus).
const (
	GoalActive        = "active"
	GoalPaused        = "paused"
	GoalBlocked       = "blocked"
	GoalUsageLimited  = "usageLimited"
	GoalBudgetLimited = "budgetLimited"
	GoalComplete      = "complete"
)

// GoalState is the observed native goal of a supervised thread, for status
// surfaces. Zero TokenBudget means the goal has no budget.
type GoalState struct {
	Objective   string `json:"objective"`
	Status      string `json:"status"`
	TokenBudget int64  `json:"tokenBudget,omitempty"`
	TokensUsed  int64  `json:"tokensUsed"`
	CreatedAt   int64  `json:"createdAt"`
}

// Goal returns the last goal state observed on the thread (from the resume
// handshake and every thread/goal/updated since), or ok=false when the thread
// has no goal or none was observed yet.
func (s *Supervisor) Goal() (GoalState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return goalState(s.goal)
}

func goalState(g *threadGoal) (GoalState, bool) {
	if g == nil {
		return GoalState{}, false
	}
	st := GoalState{Objective: g.Objective, Status: g.Status, TokensUsed: g.TokensUsed, CreatedAt: g.CreatedAt}
	if g.TokenBudget != nil {
		st.TokenBudget = *g.TokenBudget
	}
	return st, true
}

// RestartWork reads observed state even after the transport was retired for
// daemon shutdown. Explicit pauses/clears are observed while it is live.
func (s *Supervisor) RestartWork() RestartWork {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return RestartWork{}
	}
	if s.restartWork != nil {
		return *s.restartWork
	}
	return s.restartWorkLocked()
}

func (s *Supervisor) restartWorkLocked() RestartWork {
	w := RestartWork{ThreadID: s.threadID}
	if s.goal != nil {
		if s.goal.Status == GoalActive {
			w.GoalKey = s.goal.key()
		}
	} else if s.curTurn != "" && len(s.approvals.open()) == 0 {
		w.Continue = true
	}
	return w
}

func isRPCUnsupported(err error) bool {
	var rpc *rpcError
	return errors.As(err, &rpc) && (rpc.Code == -32601 || strings.Contains(strings.ToLower(rpc.Message), "goals feature is disabled"))
}

func (s *Supervisor) observeGoal(method string, params json.RawMessage) {
	if method != "thread/goal/updated" && method != "thread/goal/cleared" {
		return
	}
	var p struct {
		ThreadID string      `json:"threadId"`
		Goal     *threadGoal `json:"goal"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.ThreadID != s.threadID || s.closed || s.interrupted {
		return
	}
	s.goal = p.Goal
	s.goalRevision++
}

// readGoal fetches the authoritative goal. Older Codex builds and installations
// with goals disabled read as "no goal" and remain usable.
func (s *Supervisor) readGoal(ctx context.Context) (*threadGoal, error) {
	s.mu.Lock()
	revision := s.goalRevision
	s.mu.Unlock()
	raw, err := s.rpc.call(ctx, "thread/goal/get", map[string]any{"threadId": s.ThreadID()})
	if err != nil {
		if !isRPCUnsupported(err) {
			return nil, fmt.Errorf("codexapp read goal: %w", err)
		}
		if s.cfg.Goals {
			// A goal session must not come up as an ordinary session by accident.
			return nil, ErrGoalsUnsupported
		}
		raw = json.RawMessage(`{"goal":null}`)
	}
	var result struct {
		Goal *threadGoal `json:"goal"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("codexapp decode goal: %w", err)
	}
	s.mu.Lock()
	if s.goalRevision == revision {
		s.goal = result.Goal
	}
	goal := s.goal
	s.mu.Unlock()
	return goal, nil
}

func (s *Supervisor) restoreGoal(ctx context.Context, resumed bool) error {
	goal, err := s.readGoal(ctx)
	if err != nil {
		return err
	}
	w := s.cfg.RestartWork
	if !resumed || w == nil || w.ThreadID != s.ThreadID() {
		return nil
	}
	if goal != nil {
		if w.GoalKey != "" && w.GoalKey == goal.key() && goal.Status == GoalPaused {
			// Change status only: preserve the objective, budget and usage. Never
			// reactivate blocked, completed or limited goals, or a different goal.
			if _, err := s.rpc.call(ctx, "thread/goal/set", map[string]any{"threadId": w.ThreadID, "status": GoalActive}); err != nil {
				return fmt.Errorf("codexapp resume interrupted goal: %w", err)
			}
		}
		// Active goals are continued by Codex itself during thread/resume.
		return nil
	}
	if w.Continue && w.GoalKey == "" {
		_, err := s.rpc.call(ctx, "turn/start", map[string]any{"threadId": w.ThreadID, "input": inputBlocks(agent.ResumeWorkPrompt)})
		if err != nil {
			return fmt.Errorf("codexapp continue interrupted work: %w", err)
		}
	}
	return nil
}

// ── goal sessions: every user task is the thread goal ────────────────────────

// goalOpTimeout bounds one host-side goal operation (a read plus a set/clear).
const goalOpTimeout = 20 * time.Second

// ErrGoalsUnsupported means the running Codex has no thread-goal API (feature
// disabled or a build predating it). A goal session cannot honestly run without
// it, so its launch fails with this rather than degrading to ordinary turns.
var ErrGoalsUnsupported = errors.New("codexapp: native goals unsupported by this Codex (features.goals disabled or build too old); a goal session cannot start")

// goalTask is one observed user message admitted as a task, with the state it
// was observed against. establishGoal acts only if that state still holds.
type goalTask struct {
	itemID   string
	turnID   string // the live turn the message was observed in
	text     string
	revision uint64 // s.goalRevision at observation
}

// observeUserTask admits a user message observed on a goal session
// (Config.Goals) as a task, whatever client submitted it: a daemon `prompt`
// (rail, web, provider), the creation prompt, or a native TUI typing straight
// into the shared thread with its own turn/start. The App Server broadcasts the
// canonical userMessage item for all of them, so observing it here is the one
// path that cannot be bypassed. Codex's own continuation prompts are internal
// context, not userMessage items, so they never re-enter here.
//
// It runs on the read loop, so the RPCs happen on their own goroutine; the
// decision is made later against the authoritative goal and the observed
// generation (establishGoal).
func (s *Supervisor) observeUserTask(method string, params json.RawMessage) {
	if !s.cfg.Goals || (method != "item/started" && method != "item/completed") {
		return
	}
	var wrap struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
		Item     struct {
			ID      string `json:"id"`
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"item"`
	}
	if json.Unmarshal(params, &wrap) != nil || wrap.Item.Type != "userMessage" || wrap.Item.ID == "" {
		return
	}
	var texts []string
	for _, b := range wrap.Item.Content {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			texts = append(texts, b.Text)
		}
	}
	text := strings.TrimSpace(strings.Join(texts, "\n"))
	if text == "" {
		return
	}
	s.mu.Lock()
	// Only a message inside the observed live turn on our thread is a task
	// being submitted now; anything else (history a client re-reads, a stale
	// item, a foreign thread that slipped past) is not.
	admit := s.threadID != "" && (wrap.ThreadID == "" || wrap.ThreadID == s.threadID) &&
		s.curTurn != "" && (wrap.TurnID == "" || wrap.TurnID == s.curTurn) && !s.closed && !s.interrupted
	if admit {
		if s.goalTasks == nil {
			s.goalTasks = map[string]bool{}
		}
		if s.goalTasks[wrap.Item.ID] {
			admit = false
		} else {
			s.goalTasks[wrap.Item.ID] = true
		}
	}
	task := goalTask{itemID: wrap.Item.ID, turnID: s.curTurn, text: text, revision: s.goalRevision}
	s.mu.Unlock()
	if !admit {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), goalOpTimeout)
		defer cancel()
		if err := s.establishGoal(ctx, task); err != nil {
			s.emit(notice("error", "goal: "+err.Error()))
		}
	}()
}

// errGoalMoved means the native goal changed between a task's observation and
// its decision (the model created/completed/paused it, or the user set or
// cleared it); the native state wins and the observed task is dropped.
var errGoalMoved = errors.New("goal state changed")

// establishGoal applies one admitted task to the native goal:
//
//   - no goal: the task becomes the goal (active). Codex continues it on every
//     idle turn from then on, in the same turn the user's message started, so
//     there is no second kickoff turn.
//   - complete: the task is genuinely new work — clear the finished goal and
//     start a fresh one, so its objective, budget and usage do not inherit the
//     finished goal's accounting.
//   - blocked / usageLimited: the user's message is the input Codex was waiting
//     for; reactivate the same goal (a fresh blocked audit) with its objective,
//     budget and usage intact.
//   - active: ordinary steering. Codex resumes its own (possibly deferred, after
//     a `stop`) continuation once the user's turn ends; nothing to set.
//   - paused / budgetLimited: explicit user decisions. The message runs as a
//     plain turn and the goal is left exactly as it is; only the goal verb (or
//     the model at the user's request) changes it.
//
// The decision is guarded by the state the task was observed against: if the
// goal moved since (the model created or finished one in the same quick turn,
// or the user set/cleared it), or the turn ended other than by completing (a
// `stop`), the native outcome wins and nothing is set.
func (s *Supervisor) establishGoal(ctx context.Context, task goalTask) error {
	s.goalOp.Lock()
	defer s.goalOp.Unlock()
	goal, err := s.readGoal(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	threadID := s.threadID
	stale := s.closed || s.interrupted || s.goalRevision != task.revision
	if !stale && s.curTurn != task.turnID {
		stop, ended := s.endedTurns[task.turnID]
		stale = !ended || stop != "completed"
	}
	s.mu.Unlock()
	if stale {
		return nil
	}
	// Each set is admitted only while the observed goal still looks the way this
	// decision assumed; a fresher native update (a notification that raced the
	// RPC) wins and the task is dropped.
	set := func(params map[string]any, text string, expect func(*threadGoal) bool) error {
		params["threadId"] = threadID
		err := s.setGoal(ctx, params, text, expect)
		if errors.Is(err, errGoalMoved) {
			return nil
		}
		return err
	}
	noGoal := func(g *threadGoal) bool { return g == nil }
	switch {
	case goal == nil:
		return set(map[string]any{"objective": task.text, "status": GoalActive}, "goal established: "+taskSummary(task.text), noGoal)
	case goal.Status == GoalComplete:
		completed := func(g *threadGoal) bool { return g != nil && g.Status == GoalComplete && g.key() == goal.key() }
		if err := s.clearGoal(ctx, "", completed); err != nil {
			if errors.Is(err, errGoalMoved) {
				return nil
			}
			return fmt.Errorf("clear completed goal: %w", err)
		}
		return set(map[string]any{"objective": task.text, "status": GoalActive}, "goal established (previous goal complete): "+taskSummary(task.text), noGoal)
	case goal.Status == GoalBlocked || goal.Status == GoalUsageLimited:
		same := func(g *threadGoal) bool { return g != nil && g.Status == goal.Status && g.key() == goal.key() }
		return set(map[string]any{"status": GoalActive}, "goal resumed from "+goal.Status+" by user input: "+taskSummary(goal.Objective), same)
	case goal.Status == GoalPaused || goal.Status == GoalBudgetLimited:
		s.emit(notice("info", "goal stays "+goal.Status+" (explicit); the message runs as a plain turn — use the goal verb to resume or raise the budget"))
		return nil
	default:
		return nil
	}
}

// setGoal issues thread/goal/set. expect, when given, must hold for the
// observed goal right before the RPC or nothing is sent (errGoalMoved). The
// revision captured at that point decides whether the RPC result is recorded:
// a notification that arrived meanwhile is fresher evidence and is kept.
func (s *Supervisor) setGoal(ctx context.Context, params map[string]any, text string, expect func(*threadGoal) bool) error {
	s.mu.Lock()
	before := s.goalRevision
	moved := expect != nil && !expect(s.goal)
	s.mu.Unlock()
	if moved {
		return errGoalMoved
	}
	raw, err := s.rpc.call(ctx, "thread/goal/set", params)
	if err != nil {
		if isRPCUnsupported(err) {
			return ErrGoalsUnsupported
		}
		return fmt.Errorf("set goal: %w", err)
	}
	var result struct {
		Goal *threadGoal `json:"goal"`
	}
	if json.Unmarshal(raw, &result) == nil && result.Goal != nil {
		s.mu.Lock()
		if s.goalRevision == before {
			s.goal = result.Goal
			s.goalRevision++
		}
		s.mu.Unlock()
	}
	if text != "" {
		s.emit(notice("info", text))
	}
	return nil
}

// clearGoal issues thread/goal/clear under the same expectation and
// fresher-evidence rules as setGoal.
func (s *Supervisor) clearGoal(ctx context.Context, text string, expect func(*threadGoal) bool) error {
	s.mu.Lock()
	threadID := s.threadID
	before := s.goalRevision
	moved := expect != nil && !expect(s.goal)
	s.mu.Unlock()
	if moved {
		return errGoalMoved
	}
	if _, err := s.rpc.call(ctx, "thread/goal/clear", map[string]any{"threadId": threadID}); err != nil {
		if isRPCUnsupported(err) {
			return ErrGoalsUnsupported
		}
		return fmt.Errorf("clear goal: %w", err)
	}
	s.mu.Lock()
	if s.goalRevision == before {
		s.goal = nil
		s.goalRevision++
	}
	s.mu.Unlock()
	if text != "" {
		s.emit(notice("info", text))
	}
	return nil
}

// GoalUpdate is an explicit host-side goal control (the `goal` steer verb).
// Status is one of the native statuses the host may set (active, paused,
// complete) or GoalClear; Objective and TokenBudget are optional and set only
// when non-zero. A set with only an objective keeps the current status.
type GoalUpdate struct {
	Status      string
	Objective   string
	TokenBudget int64
}

// GoalClear is the GoalUpdate.Status that removes the goal entirely.
const GoalClear = "clear"

// SetGoal applies an explicit goal control. Unlike establishGoal it is the
// user's decision, so it is not admission-guarded: it may pause, resume
// (including from budgetLimited, with a raised budget) or replace the goal, and
// any task admitted before it is dropped by that task's own guard. It never
// starts a turn itself: Codex continues an activated goal on its own once idle.
func (s *Supervisor) SetGoal(ctx context.Context, u GoalUpdate) error {
	s.goalOp.Lock()
	defer s.goalOp.Unlock()
	threadID := s.ThreadID()
	if threadID == "" {
		return errors.New("codexapp: no thread to set a goal on")
	}
	if u.Status == GoalClear {
		return s.clearGoal(ctx, "goal cleared by user", nil)
	}
	params := map[string]any{"threadId": threadID}
	switch u.Status {
	case "":
	case GoalActive, GoalPaused, GoalComplete:
		params["status"] = u.Status
	default:
		return fmt.Errorf("goal: status must be %s, %s, %s or %s", GoalActive, GoalPaused, GoalComplete, GoalClear)
	}
	if o := strings.TrimSpace(u.Objective); o != "" {
		params["objective"] = o
	}
	if u.TokenBudget > 0 {
		params["tokenBudget"] = u.TokenBudget
	}
	if len(params) == 1 {
		return errors.New("goal: nothing to set (need status, objective or token_budget)")
	}
	goal, err := s.readGoal(ctx)
	if err != nil {
		return err
	}
	if goal == nil && params["objective"] == nil {
		return errors.New("goal: the thread has no goal; give it an objective")
	}
	label := "goal updated by user"
	if u.Status != "" {
		label = "goal set " + u.Status + " by user"
	}
	return s.setGoal(ctx, params, label, nil)
}

// taskSummary condenses a task into one notice-sized line.
func taskSummary(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if t := strings.Join(strings.Fields(line), " "); t != "" {
			if len(t) > 120 {
				t = t[:117] + "…"
			}
			return t
		}
	}
	return ""
}
