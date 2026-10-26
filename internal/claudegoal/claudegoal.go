// Package claudegoal is goal mode for a Claude Code workgroup coordinator.
//
// Claude Code has no native goal engine, so amux supplies one from two Claude
// hooks and a small state file in the coordinator's own session directory
// (StatePath):
//
//   - UserPromptSubmit (`amux agent goal prompt --hook`): every user task becomes
//     the active goal. A reply to a question the coordinator asked keeps the
//     current objective and re-activates it; an amux continuation nudge
//     (ResumePrompt) is never mistaken for a new task.
//   - Stop (`amux agent goal stop --hook`): while the goal is active the hook
//     blocks the stop and hands Claude a continuation prompt, so the coordinator
//     keeps working across turns until it marks the goal complete, asks the user
//     for input, reports itself blocked, or the user pauses it. A continuation
//     budget (MaxContinuations) bounds a goal that never converges. A goal
//     marked waiting lets exactly the next Stop through and is active again
//     after it, so the turn a background wake-up starts continues the goal.
//
// The model moves its own goal with `amux agent goal complete|ask|blocked`; the
// user pauses, resumes, replaces or clears it with the host's `goal` steer verb.
// The file is session-local state, not an authority boundary: it lives in the
// coordinator's writable sandbox, exactly like the model's own update_goal on
// Codex, and nothing outside the coordinator's goal loop trusts it.
//
// This package is pure state and wording; callers do the file I/O (the hook
// commands inside the sandbox, the daemon through a pinned session root).
package claudegoal

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// Goal statuses. active, paused, blocked, budgetLimited and complete share the
// Codex App Server's spellings so status surfaces read the same on both
// runtimes. input and waiting are Claude-only: input means the coordinator
// asked the user something and waits for the reply; waiting means it armed a
// background wake-up (a Monitor or run_in_background command on a member's
// events, a PR, CI) and lets exactly one Stop through so it can idle until that
// wake-up arrives instead of busy-polling under goal pressure.
const (
	StatusActive        = "active"
	StatusPaused        = "paused"
	StatusInput         = "input"
	StatusWaiting       = "waiting"
	StatusBlocked       = "blocked"
	StatusBudgetLimited = "budgetLimited"
	StatusComplete      = "complete"
)

// User-side goal controls (the host `goal` steer verb's status field).
const (
	ControlActive   = StatusActive
	ControlPaused   = StatusPaused
	ControlComplete = StatusComplete
	ControlClear    = "clear"
)

// MaxContinuations bounds how many times the Stop hook continues one goal
// without a user turn in between. Codex bounds a goal by tokens; Claude Code
// exposes no usage to a hook, so amux bounds the loop by continuations. Any user
// prompt (a new task, an answer, a resume) resets the count.
const MaxContinuations = 100

// MaxObjectiveBytes caps the objective kept in the state file. The full task is
// already in Claude's conversation; the file only needs enough to show and to
// restate it in a continuation prompt.
const MaxObjectiveBytes = 4 << 10

// ResumePrompt is the text amux types into an idle coordinator when the user
// resumes its goal. Its marker prefix tells the prompt hook it is a
// continuation, not a new task.
const ResumePrompt = resumeMarker + " The user resumed this goal. Continue working toward it."

const resumeMarker = "[amux goal]"

// EnvFile names the goal state file in a Claude goal coordinator's environment
// (wsops.AgentEnv sets it). Its absence means the session is not in Claude goal
// mode.
const EnvFile = "AMUX_GOAL_FILE"

// StatePath is the goal file under a coordinator's session directory. It sits
// beside the session's other private amux state (.amux/claude, the mailbox).
func StatePath(sessionDir string) string { return filepath.Join(sessionDir, StateRel) }

// StateRel is StatePath relative to the session directory.
var StateRel = filepath.Join(".amux", "goal.json")

// State is the persisted goal. The zero value means "no goal".
type State struct {
	Objective     string `json:"objective,omitempty"`
	Status        string `json:"status,omitempty"`
	Continuations int    `json:"continuations,omitempty"`
	// Note is the model's last word on a non-active status: the question it
	// asked (input), why it is blocked, or its completion summary.
	Note    string `json:"note,omitempty"`
	Created int64  `json:"created,omitempty"`
	Updated int64  `json:"updated,omitempty"`
}

// None reports whether there is no goal.
func (s State) None() bool { return s.Status == "" }

// Decode reads a state file's bytes. Missing or empty content is no goal; a
// corrupt file is an error so a caller never silently drops a goal.
func Decode(b []byte) (State, error) {
	var s State
	if len(strings.TrimSpace(string(b))) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return State{}, fmt.Errorf("goal state: %w", err)
	}
	return s, nil
}

// Encode renders the state for the file.
func Encode(s State) []byte {
	b, _ := json.MarshalIndent(s, "", "  ")
	return append(b, '\n')
}

// OnPrompt applies one user prompt. A continuation nudge keeps the objective,
// and a reply to a pending question (status input) is appended to it; anything
// else — including a prompt while the goal is waiting on members, which was not
// waiting on the user — is a new task and becomes the goal, replacing whatever was there (a
// completed, blocked or paused goal included — the user sending a task is the
// user's instruction to work on it).
func OnPrompt(s State, prompt string, now time.Time) State {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return s
	}
	resume := strings.HasPrefix(prompt, resumeMarker)
	if !s.None() && s.Objective != "" && (resume || s.Status == StatusInput) {
		if !resume {
			// The reply is input to the goal, not a new task: keep it beside the
			// objective so continuation prompts restate both.
			s.Objective = truncate(s.Objective + "\n\nUser reply: " + prompt)
		}
		s.Status = StatusActive
		s.Continuations = 0
		s.Note = ""
		s.Updated = now.Unix()
		return s
	}
	if resume {
		return s // a stray nudge with no goal to continue establishes nothing
	}
	return State{Objective: truncate(prompt), Status: StatusActive, Created: now.Unix(), Updated: now.Unix()}
}

// OnStop decides whether Claude may end its turn. While the goal is active it
// returns cont=true with the continuation prompt the Stop hook hands back to
// Claude, and the state with the continuation counted; past MaxContinuations
// the goal becomes budgetLimited and the turn ends.
//
// A waiting goal lets this one Stop through and becomes active again, so the
// next turn — the one the coordinator's armed wake-up starts — ends under goal
// pressure. The continuation count is kept, not reset: the budget bounds turns
// the hook forced, and a wake-up turn is not one of them, but a coordinator
// that only ever waits and wakes must not earn a fresh budget each cycle.
func OnStop(s State, now time.Time) (next State, reason string, cont bool) {
	if s.Status == StatusWaiting {
		s.Status = StatusActive
		s.Updated = now.Unix()
		return s, "", false
	}
	if s.Status != StatusActive {
		return s, "", false
	}
	if s.Continuations >= MaxContinuations {
		s.Status = StatusBudgetLimited
		s.Note = fmt.Sprintf("stopped after %d continuations without a user turn", s.Continuations)
		s.Updated = now.Unix()
		return s, "", false
	}
	s.Continuations++
	s.Updated = now.Unix()
	return s, ContinuationPrompt(s), true
}

// ContinuationPrompt is what the Stop hook feeds Claude to keep the goal going.
// It restates the objective and the only honest ways out of the loop.
func ContinuationPrompt(s State) string {
	return fmt.Sprintf(`amux goal mode: your goal is still active (continuation %d of at most %d), so this turn continues.

Objective: %s

Keep making real progress toward the verified end state: check members' evidence, dispatch or steer work, fix what falls short. Do not restate a plan without acting on it.
- Done and verified: member PRs are green and marked ready for review (`+"`gh pr ready <n>`"+`), not left draft — "do not merge" never means draft. Then run `+"`amux agent goal complete \"<summary>\"`"+` and give your final summary.
- Need a decision only the user can make, or information you cannot find: run `+"`amux agent goal ask \"<question>\"`"+`, then ask it plainly and end your turn. Choose a sensible default for anything optional instead.
- Waiting on members or CI: never busy-poll across continuations. If nothing can move until they report, first arm a background wake-up (a Monitor, or a run_in_background until-loop on the member's events or the PR/CI state), then run `+"`amux agent goal wait \"<what you are waiting on>\"`"+` and end your turn; the wake-up's notification resumes the goal. Never run goal wait without an armed wake-up.
- Hard blocker that repeats and needs the user: run `+"`amux agent goal blocked \"<reason>\"`"+` and explain it.`,
		s.Continuations, MaxContinuations, s.Objective)
}

// Mark applies one of the model's own transitions: complete, ask (input),
// wait (waiting) or blocked. They need a goal to act on.
func Mark(s State, status, note string, now time.Time) (State, error) {
	if s.None() {
		return s, fmt.Errorf("no goal is set: a goal is established by the user's next task")
	}
	switch status {
	case StatusComplete, StatusInput, StatusWaiting, StatusBlocked:
	default:
		return s, fmt.Errorf("unknown goal transition %q", status)
	}
	s.Status = status
	s.Note = truncate(strings.TrimSpace(note))
	s.Updated = now.Unix()
	return s, nil
}

// Control applies a user-side goal control (the host `goal` steer verb):
// pause, resume (active), complete, clear, optionally replacing the objective.
// resume reports whether the goal is now active after not being active, so the
// caller can nudge an idle coordinator back to work.
func Control(s State, status, objective string, now time.Time) (next State, resume bool, err error) {
	status = strings.TrimSpace(status)
	objective = strings.TrimSpace(objective)
	if status == ControlClear {
		if objective != "" {
			return s, false, fmt.Errorf("goal: status=clear takes no objective")
		}
		return State{}, false, nil
	}
	if status == "" && objective == "" {
		return s, false, fmt.Errorf("goal: nothing to set (need status or objective)")
	}
	switch status {
	case "", ControlActive, ControlPaused, ControlComplete:
	default:
		return s, false, fmt.Errorf("goal: status must be active, paused, complete or clear, got %q", status)
	}
	was := s.Status
	if objective != "" {
		if s.None() {
			s = State{Created: now.Unix(), Status: StatusActive}
		}
		s.Objective = truncate(objective)
	}
	if s.None() {
		return s, false, fmt.Errorf("goal: the coordinator has no goal; give it an objective")
	}
	if status != "" {
		s.Status = status
	}
	if s.Status == StatusActive {
		s.Continuations = 0
	}
	s.Note = ""
	s.Updated = now.Unix()
	return s, s.Status == StatusActive && was != StatusActive, nil
}

// Summary is one human line for `amux agent goal` and status surfaces.
func Summary(s State) string {
	if s.None() {
		return "no goal"
	}
	line := s.Status
	if s.Status == StatusActive && s.Continuations > 0 {
		line += fmt.Sprintf(" (continuation %d/%d)", s.Continuations, MaxContinuations)
	}
	line += ": " + firstLine(s.Objective)
	if s.Note != "" {
		line += " — " + firstLine(s.Note)
	}
	return line
}

func truncate(s string) string {
	if len(s) <= MaxObjectiveBytes {
		return s
	}
	cut := MaxObjectiveBytes
	for cut > 0 && (s[cut]&0xC0) == 0x80 { // do not split a UTF-8 sequence
		cut--
	}
	return s[:cut] + "…"
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.Join(strings.Fields(line), " "); t != "" {
			if len(t) > 120 {
				t = t[:117] + "…"
			}
			return t
		}
	}
	return ""
}
