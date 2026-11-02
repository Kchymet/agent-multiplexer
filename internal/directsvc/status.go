package directsvc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"amux/internal/core"
)

// Supervisor states, as written to the status file.
const (
	// StateDisabled: the setting is off (or absent); no listener runs.
	StateDisabled = "disabled"
	// StateRunning: the managed child is alive.
	StateRunning = "running"
	// StateBackoff: the child exited or could not start; it is retried at NextRetry.
	StateBackoff = "backoff"
	// StateInvalid: the saved setting cannot be read; nothing runs until it is fixed.
	StateInvalid = "invalid"
	// StateStopped: the supervising daemon has stopped, so no listener runs.
	StateStopped = "stopped"
)

// Status is what the supervisor publishes for `amux direct status` and doctor.
// It carries no secret: the listen address and endpoints are already in
// config.json, and nothing else here identifies a credential.
type Status struct {
	State       string    `json:"state"`
	Enabled     bool      `json:"enabled"`
	Fingerprint string    `json:"fingerprint,omitempty"` // configuration the current child runs (or would run)
	PID         int       `json:"pid,omitempty"`         // managed child
	DaemonPID   int       `json:"daemon_pid"`
	StartedAt   time.Time `json:"started_at,omitempty"`
	NextRetry   time.Time `json:"next_retry,omitempty"`
	Restarts    int       `json:"restarts"`
	LastError   string    `json:"last_error,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// StatusPath is the supervisor's status file.
func StatusPath() string { return filepath.Join(core.StateDir(), "direct-managed.json") }

// LogPath is where the managed child's stdout and stderr go.
func LogPath() string { return filepath.Join(core.StateDir(), "direct.log") }

// ReadStatus reads a status file.
func ReadStatus(path string) (Status, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Status{}, err
	}
	var st Status
	if err := json.Unmarshal(b, &st); err != nil {
		return Status{}, err
	}
	return st, nil
}

// writeStatus replaces the status file atomically, so a reader never sees a
// torn record.
func writeStatus(path string, st Status) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".direct-managed-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(append(b, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
