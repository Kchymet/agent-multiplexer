package selfupdate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// State is the self-update record shared by the CLI and the daemon, kept in
// <state dir>/update/state.json. It lets `amux version`/`doctor`/`status`
// report a pending update without network access, and lets the daemon load a
// binary that was installed (or staged) while agents were running.
type State struct {
	// LastCheck is when a release check last completed (successfully or not);
	// the automatic check is rate limited from it, across daemon restarts.
	LastCheck time.Time `json:"lastCheck,omitzero"`
	// Latest is the newest release version seen by that check.
	Latest string `json:"latest,omitempty"`
	// LastError is the last check/install failure, cleared on success.
	LastError string `json:"lastError,omitempty"`
	// Installed is the release version most recently written over Path. When
	// it is newer than the running daemon, the daemon restarts into it at the
	// next safe point.
	Installed string `json:"installed,omitempty"`
	Path      string `json:"path,omitempty"`
	// Staged is a downloaded, verified release that could not be swapped in
	// yet: its protocol or schema differs, so the binary is replaced only
	// together with a daemon restart.
	Staged *Staged `json:"staged,omitempty"`
	// Pending explains why the daemon has not loaded Installed/Staged yet
	// (e.g. "waiting for 3 running agents to stop"), written by the daemon.
	Pending   string    `json:"pending,omitempty"`
	PendingAt time.Time `json:"pendingAt,omitzero"`
}

// Staged is a verified release binary waiting in the staging directory.
type Staged struct {
	Manifest
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	Target string `json:"target"`
	Shim   string `json:"shim,omitempty"`
	// Forced marks a stage the user requested over a development build
	// (amux update --force); otherwise a dev build at Target is never replaced.
	Forced bool `json:"forced,omitempty"`
}

// Store is the on-disk home of the update state and staging area.
type Store struct{ Dir string }

func (s Store) statePath() string { return filepath.Join(s.Dir, "state.json") }
func (s Store) lockPath() string  { return filepath.Join(s.Dir, "lock") }

// StagingDir holds downloaded release files.
func (s Store) StagingDir() string { return filepath.Join(s.Dir, "staged") }

// Load reads the state; a missing file is the zero state.
func (s Store) Load() (State, error) {
	var st State
	b, err := os.ReadFile(s.statePath())
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return State{}, fmt.Errorf("%s: %w", s.statePath(), err)
	}
	return st, nil
}

// Save atomically writes the state.
func (s Store) Save(st State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	return WriteFileAtomic(s.statePath(), append(b, '\n'), 0o600)
}

// Update applies fn to the state under the lock and saves the result.
func (s Store) Update(fn func(*State)) error {
	unlock, err := s.Lock(10 * time.Second)
	if err != nil {
		return err
	}
	defer unlock()
	st, err := s.Load()
	if err != nil {
		st = State{}
	}
	fn(&st)
	return s.Save(st)
}

// ErrLocked reports another process holding the update lock.
var ErrLocked = errors.New("another amux update is in progress")

// Lock serializes updates across processes (CLI, daemon, a second CLI) with an
// exclusive flock, waiting up to wait.
func (s Store) Lock(wait time.Duration) (func(), error) {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(s.lockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			var once sync.Once
			return func() {
				once.Do(func() {
					_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
					_ = f.Close()
				})
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(deadline) {
			_ = f.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) {
				return nil, ErrLocked
			}
			return nil, err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// PendingVersion is the release the daemon still has to load, or "" when the
// running daemon (version running, executable path target) is current.
func (st State) PendingVersion(running, target string) string {
	if st.Staged != nil && (target == "" || st.Staged.Target == target) && Newer(st.Staged.Version, running) {
		return st.Staged.Version
	}
	if st.Installed != "" && (target == "" || st.Path == target) && Newer(st.Installed, running) {
		return st.Installed
	}
	return ""
}
