package daemon

import (
	"amux/internal/agent"
	"amux/internal/codexapp"
	"amux/internal/core"
	"amux/internal/engine"
	"amux/internal/panespec"
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
