package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RuntimeModelRecord is a runtime's latest self-reported model selection. It is
// separate from HookRecord because a status-line refresh is not an activity
// transition, and runtimes without lifecycle hooks (notably Codex) can still
// participate in the same reconciliation path.
type RuntimeModelRecord struct {
	Model   string `json:"model"`
	Updated int64  `json:"updated"`
}

// RuntimeModelDir holds model observations keyed by the runtime conversation id.
func RuntimeModelDir() string { return filepath.Join(StateDir(), "models") }

func runtimeModelPath(sessionID string) string {
	return filepath.Join(RuntimeModelDir(), sanitizeID(sessionID))
}

// WriteRuntimeModel records the current model reported by a runtime. Blank
// identities and models are harmless no-ops: self-reporting must never disturb
// the agent that invoked it.
func WriteRuntimeModel(sessionID, model string) error {
	sessionID = strings.TrimSpace(sessionID)
	model = strings.TrimSpace(model)
	if sessionID == "" || model == "" {
		return nil
	}
	if err := os.MkdirAll(RuntimeModelDir(), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(RuntimeModelRecord{Model: model, Updated: time.Now().UnixMilli()})
	if err != nil {
		return err
	}
	dst := runtimeModelPath(sessionID)
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// RuntimeModel returns the latest model observation for sessionID.
func RuntimeModel(sessionID string) (RuntimeModelRecord, bool) {
	if strings.TrimSpace(sessionID) == "" {
		return RuntimeModelRecord{}, false
	}
	b, err := os.ReadFile(runtimeModelPath(sessionID))
	if err != nil {
		return RuntimeModelRecord{}, false
	}
	var r RuntimeModelRecord
	if json.Unmarshal(b, &r) != nil || strings.TrimSpace(r.Model) == "" {
		return RuntimeModelRecord{}, false
	}
	return r, true
}
