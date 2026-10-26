package daemon

import (
	"errors"
	"io/fs"
	"log"

	"amux/internal/agent"
	"amux/internal/claudegoal"
	"amux/internal/codexapp"
	"amux/internal/core"
	"amux/internal/engine"
	"amux/internal/hostprep"
	"amux/internal/panespec"
	"amux/internal/store"
)

// Embedding Key preserves the legacy live-agents journal's JSON shape. Older
// entries have no work intent and restore the conversation without submitting.
type restartRecord struct {
	engine.Key
	Runtime      string                `json:"runtime,omitempty"`
	Conversation string                `json:"conversation,omitempty"`
	ResumeWork   bool                  `json:"resumeWork,omitempty"`
	Codex        *codexapp.RestartWork `json:"codex,omitempty"`
}

func (d *Daemon) captureRestart(k engine.Key) restartRecord {
	r := restartRecord{Key: k}
	if k.Tab != panespec.TabAgent {
		return r
	}
	s, ok, err := lookupSession(k.AgentID)
	if err != nil || !ok || s.Archived {
		return r
	}
	r.Runtime, r.Conversation = agent.Canonical(s.Agent), s.ClaudeID
	if d.structuredControl(s) {
		if work, ok := d.codex.RestartSnapshot()[s.ID]; ok {
			r.Codex = &work
		}
		return r
	}
	// A waiting permission/question is not running work. Use the explicit hook
	// state rather than the coarser ActivityBusy (which includes waiting), or
	// inferred transcript freshness (which can outlive a deliberate pause).
	if h, ok := core.SessionHookState(s.ID, s.ClaudeID); ok {
		r.ResumeWork = h.State == core.StateRunning
	}
	return r
}

func (r restartRecord) apply(spec *panespec.LaunchSpec) *codexapp.RestartWork {
	s := spec.Session
	if r.Runtime != agent.Canonical(s.Agent) || r.Conversation != s.ClaudeID || s.Archived {
		return nil
	}
	spec.ResumeWork = r.ResumeWork
	return r.Codex
}

// startIntent is what a launch knows about the session's past: the restart
// journal record for it, if any, and whether an eligible goal may be resumed
// from its own durable record (resumeGoal). A `prompt` that cold-starts a
// stopped coordinator passes no goal resume: the prompt it carries is the turn,
// and the prompt hook makes it the goal.
type startIntent struct {
	record     *restartRecord
	resumeGoal bool
}

// restartIntent folds the host's restart evidence into one launch: the journal
// record (running work, the Codex thread goal identity), then — for a Claude
// goal coordinator — the goal file itself.
//
// The goal file (claudegoal.StatePath, in the coordinator's session directory)
// is the goal's durable record: unlike the running hook state, which an exit
// hook clears, and unlike the restart journal, which is a snapshot, it survives
// an agent restart, a daemon restart and a crash, so a restart decides from it
// directly. A goal that was active, or waiting on a background wake-up that died
// with the runtime, is relaunched with its continuation and no user turn
// (claudegoal.ContinueOnRestart); paused, complete, blocked, input-waiting and
// budget-limited goals are left to the user. The switch is goal.auto_resume
// (amuxcfg), captured when the daemon was built.
//
// Whatever the decision, a hook-goal coordinator's continuation is the
// goal-aware one (claudegoal.RestartPrompt): the generic ResumeWorkPrompt would
// reach its prompt hook as an ordinary user task and replace the goal with the
// words "this session was restarted".
func (d *Daemon) restartIntent(intent startIntent, spec *panespec.LaunchSpec) *codexapp.RestartWork {
	var work *codexapp.RestartWork
	if intent.record != nil {
		work = intent.record.apply(spec)
	}
	s := spec.Session
	if s.Archived || !agent.HookGoals(s) {
		return work
	}
	spec.ResumePrompt = claudegoal.RestartPrompt
	if !intent.resumeGoal || !d.goalAutoResume {
		return work
	}
	state, err := readHookGoal(s)
	if err != nil {
		log.Printf("amux: goal resume %s: %v", s.ID, err)
		return work
	}
	if !claudegoal.ContinueOnRestart(state) {
		return work
	}
	if !spec.ResumeWork {
		journal(s.ID, core.JournalInfo, "resuming goal after restart: "+claudegoal.Summary(state))
	}
	spec.ResumeWork = true
	return work
}

// readHookGoal reads a Claude coordinator's goal file through its pinned
// session root. A missing file is no goal.
func readHookGoal(sess store.Session) (claudegoal.State, error) {
	root, err := hostprep.OpenSession(sess.Dir)
	if err != nil {
		return claudegoal.State{}, err
	}
	defer root.Close()
	b, err := root.ReadFile(claudegoal.StateRel)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return claudegoal.State{}, err
	}
	return claudegoal.Decode(b)
}
