package selfupdate

import (
	"fmt"
	"strings"
	"time"

	"amux/internal/buildinfo"
)

// DisableEnv turns automatic updates off for any process that sees it
// (AMUX_NO_AUTO_UPDATE=1). The daemon reads its own environment, so set it
// where the daemon is started (shell rc), then restart the daemon.
const DisableEnv = "AMUX_NO_AUTO_UPDATE"

// DefaultCheckInterval bounds how often the daemon asks for a new release.
const DefaultCheckInterval = 6 * time.Hour

// EnvDisabled reports whether the opt-out variable is set to anything other
// than an explicit false.
func EnvDisabled(getenv func(string) string) bool {
	switch strings.ToLower(strings.TrimSpace(getenv(DisableEnv))) {
	case "", "0", "false", "no", "off":
		return false
	}
	return true
}

// AutoInput is everything the automatic check gate depends on.
type AutoInput struct {
	Running       string // product version of the running daemon
	ConfigEnabled bool   // resolved update.auto (on when unconfigured)
	EnvDisabled   bool
	LastCheck     time.Time
	Now           time.Time
	Interval      time.Duration
}

// AutoCheck decides whether the daemon should check for a release now. It
// returns a human reason when it should not. A development build is never
// auto-updated; a check is rate limited from the persisted LastCheck so
// restarts do not re-query, and a LastCheck in the future (clock change) does
// not suppress checks indefinitely.
func AutoCheck(in AutoInput) (bool, string) {
	switch {
	case in.EnvDisabled:
		return false, "automatic updates disabled by " + DisableEnv
	case !in.ConfigEnabled:
		return false, "automatic updates disabled (update.auto off)"
	case !buildinfo.IsRelease(in.Running):
		return false, fmt.Sprintf("development build %s: automatic updates never replace it (amux update --force installs the latest release)", in.Running)
	}
	interval := in.Interval
	if interval <= 0 {
		interval = DefaultCheckInterval
	}
	if !in.LastCheck.IsZero() && !in.LastCheck.After(in.Now) {
		if next := in.LastCheck.Add(interval); in.Now.Before(next) {
			return false, "next check due " + next.Format(time.RFC3339)
		}
	}
	return true, ""
}

// Action is what to do with a candidate release.
type Action int

const (
	ActionNone   Action = iota // nothing newer, or already installed
	ActionSwap                 // replace the binary now; daemon restarts into it at a safe point
	ActionStage                // download and verify; replace only together with a daemon restart
	ActionRefuse               // never install (incompatible database, or a dev build on disk)
)

func (a Action) String() string {
	return [...]string{"none", "swap", "stage", "refuse"}[a]
}

// PlanInstall classifies a candidate against the running build and the version
// actually installed at the target path (installed may be a release the CLI
// already swapped in, or a development build a user installed by hand, which
// automatic updates must not overwrite).
func PlanInstall(candidate, running Manifest, installed string) (Action, string) {
	current := running.Version
	if Newer(installed, current) {
		current = installed
	}
	if !Newer(candidate.Version, current) {
		return ActionNone, fmt.Sprintf("%s is current (latest release %s)", current, candidate.Version)
	}
	if !buildinfo.IsRelease(installed) {
		return ActionRefuse, fmt.Sprintf("installed binary is development build %q; not replacing it automatically", installed)
	}
	switch Compatibility(candidate, running) {
	case CompatHot:
		return ActionSwap, ""
	case CompatRestart:
		return ActionStage, fmt.Sprintf("%s changes the daemon protocol (%d→%d) or schema (%d→%d); it is installed together with a daemon restart",
			candidate.Version, running.DaemonProtocol, candidate.DaemonProtocol, running.DatabaseMaxSchema, candidate.DatabaseMaxSchema)
	}
	return ActionRefuse, fmt.Sprintf("%s supports database schema %d–%d, but this installation is at schema %d",
		candidate.Version, candidate.DatabaseMinSchema, candidate.DatabaseMaxSchema, running.DatabaseMaxSchema)
}

// SafePoint decides when the daemon may restart itself to load an update: only
// after it has observed no hosted agents (PTY panes or App Server sessions) on
// RequiredIdle consecutive polls. A daemon restart stops every agent it hosts,
// so it never restarts while one runs.
type SafePoint struct {
	RequiredIdle int
	idle         int
}

// Observe records one poll's live-agent count and reports whether restarting
// now is safe.
func (s *SafePoint) Observe(live int) bool {
	if live > 0 {
		s.idle = 0
		return false
	}
	s.idle++
	need := s.RequiredIdle
	if need <= 0 {
		need = 2
	}
	return s.idle >= need
}
