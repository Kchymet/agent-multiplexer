package codexapp

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// goal_intent.go persists, per structured session, whether the thread's native
// goal was last observed active while amux supervised it. It is the Codex
// counterpart of a Claude coordinator's goal file: a durable record a resume
// decides from whatever launched it — a daemon restart, an agent restart, a
// relaunch after the App Server exited, or a reconnect to a live App Server.
//
// The in-memory RestartWork snapshot only reached a new supervisor through the
// restart journal on two launch paths, and reconnect never consulted it, so a
// goal paused while amux was not watching (a native client pausing on its way
// out, a pause landing in a transport gap) came back paused whenever another
// path started the runtime first.
//
// Only observations made while the transport is live and the restore decision
// for this supervisor has been made are recorded (Supervisor.intentReady), so a
// pause amux saw is the user's and stays a pause, while one it could not see
// does not erase the record of an active goal.

// goalIntent is the sidecar's content. ActiveGoalKey is the threadGoal.key() of
// the goal last observed active on ThreadID, or "" when the last observation was
// any other state (paused, blocked, limited, complete) or no goal.
type goalIntent struct {
	ThreadID      string `json:"threadId"`
	ActiveGoalKey string `json:"activeGoalKey,omitempty"`
}

// GoalIntentPathFor is a session's goal-intent sidecar, beside its identity.
func GoalIntentPathFor(sessionID string) string {
	return filepath.Join(stateDir(), sanitize(sessionID)+".goal.json")
}

// loadGoalIntent reads a sidecar. ok=false when there is none (never recorded,
// a daemon predating it, or removed) or it is unreadable.
func loadGoalIntent(path string) (goalIntent, bool) {
	if path == "" {
		return goalIntent{}, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return goalIntent{}, false
	}
	var gi goalIntent
	if json.Unmarshal(b, &gi) != nil || gi.ThreadID == "" {
		return goalIntent{}, false
	}
	return gi, true
}

// saveGoalIntent writes a sidecar atomically. Best-effort like SaveIdentity.
func saveGoalIntent(path string, gi goalIntent) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(gi)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// RemoveGoalIntent deletes a session's goal-intent sidecar. Missing is not an
// error.
func RemoveGoalIntent(sessionID string) error {
	err := os.Remove(GoalIntentPathFor(sessionID))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// persistGoalIntentLocked records the current observation when this supervisor
// may (see intentReady) and it differs from what was last written. Called with
// s.mu held, after every recorded observation.
func (s *Supervisor) persistGoalIntentLocked() {
	path := s.cfg.GoalIntentPath
	if path == "" || !s.intentReady || s.closed || s.interrupted || s.threadID == "" {
		return
	}
	gi := goalIntent{ThreadID: s.threadID}
	if s.goal != nil && s.goal.Status == GoalActive {
		gi.ActiveGoalKey = s.goal.key()
	}
	if s.intentWritten && s.intent == gi {
		return
	}
	if saveGoalIntent(path, gi) == nil {
		s.intent, s.intentWritten = gi, true
	}
}

// armGoalIntent ends the window in which observations are not recorded (startup
// or reconnect, until restoreGoal has decided) and records the current state.
func (s *Supervisor) armGoalIntent() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.intentReady = true
	s.persistGoalIntentLocked()
}

// goalWasActive reports whether goal key on threadID was active before this
// resume. The durable record is authoritative when it covers this thread: it is
// rewritten on every observed change, so it is never older than a restart
// journal snapshot. The journal (or a reconnect's pre-gap observation) is the
// fallback for a session with no record yet.
func goalWasActive(gi goalIntent, recorded bool, w *RestartWork, threadID, key string) bool {
	if key == "" {
		return false
	}
	if recorded && gi.ThreadID == threadID {
		return gi.ActiveGoalKey == key
	}
	return w != nil && w.ThreadID == threadID && w.GoalKey == key
}
