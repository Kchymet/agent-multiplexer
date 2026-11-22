package harnessbin

import (
	"context"
	"fmt"
	"time"
)

// AdoptInput is what the adoption rules know about one harness kind on this
// machine. The caller (which can see PATH, the login shell and the harnesses'
// credential stores) gathers it; harnessbin only decides.
type AdoptInput struct {
	// EnvBin is AMUX_<KIND>_BIN, and ConfigBin harnesses.<kind>.bin. Either
	// one is an explicit choice of executable that a managed install would
	// never be launched over, so adoption leaves the kind alone.
	EnvBin, ConfigBin string
	// OnPath is the user's own executable for kind, when one resolves (PATH,
	// login shell, well-known install locations); "" when none does.
	OnPath string
	// PathVersion is OnPath's version, when it reported one.
	PathVersion string
	// LoggedIn names the credential that shows the user signed in to kind
	// (e.g. "~/.codex/auth.json"); "" when none was found.
	LoggedIn string
}

// Used reports whether the user already uses kind: its binary resolves or
// they are logged in to it.
func (in AdoptInput) Used() bool { return in.OnPath != "" || in.LoggedIn != "" }

// AdoptRetry bounds how often a failed adoption is retried.
const AdoptRetry = 6 * time.Hour

// AdoptDecision applies the adoption rules to one kind. Adoption installs and
// activates an amux-managed copy only when every rule allows it:
//
//   - the kind has no managed install yet (an existing one, pinned or not, is
//     only ever moved by the update rules);
//   - the user did not uninstall the managed copy (amux harness uninstall
//     records that, and only amux harness install clears it);
//   - no explicit executable is configured (AMUX_<KIND>_BIN or
//     harnesses.<kind>.bin win over a managed install, so it would be unused);
//   - the user already uses the kind: its binary resolves or they are logged
//     in. amux never installs a harness the user does not use;
//   - no recent adoption attempt failed (retried after AdoptRetry).
//
// It returns whether to adopt and, either way, why.
func (m *Manager) AdoptDecision(kind string, in AdoptInput) (bool, string) {
	if !Supported(kind) {
		return false, "amux cannot manage " + kind + " installs"
	}
	if cur := m.Current(kind); cur != "" {
		return false, "already managed (" + cur + ")"
	}
	st := m.LoadState(kind)
	switch {
	case st.Declined:
		return false, "managed install was uninstalled; amux harness install " + kind + " opts back in"
	case in.EnvBin != "":
		return false, "AMUX_" + upper(kind) + "_BIN selects the executable"
	case in.ConfigBin != "":
		return false, "harnesses." + kind + ".bin selects the executable"
	case !in.Used():
		return false, kind + " is not installed or logged in"
	case !st.AdoptAttemptAt.IsZero() && st.AdoptError != "" && m.now().Sub(st.AdoptAttemptAt) < AdoptRetry:
		return false, "last adoption failed (" + st.AdoptError + "); retrying after " + st.AdoptAttemptAt.Add(AdoptRetry).Format(time.RFC3339)
	}
	if in.OnPath != "" {
		return true, "found " + in.OnPath
	}
	return true, "logged in (" + in.LoggedIn + ")"
}

// AdoptResult is one kind's adoption outcome.
type AdoptResult struct {
	Kind    string
	Adopted bool
	Version string
	Reason  string
	Err     error
}

// Adopt runs the adoption rules for every supported kind and installs and
// activates a managed copy for each kind they allow. The version is the
// channel's (harnesses.<kind>.channel, else the source default), or the
// user's own version when that is newer, so adopting never moves new sessions
// to an older release than the user already ran.
func (m *Manager) Adopt(ctx context.Context, settings Settings, input func(kind string) AdoptInput) []AdoptResult {
	var out []AdoptResult
	for _, kind := range Kinds {
		in := input(kind)
		ok, why := m.AdoptDecision(kind, in)
		r := AdoptResult{Kind: kind, Reason: why}
		if ok {
			r.Version, r.Err = m.adopt(ctx, kind, settings.Kind(kind).Channel, in.PathVersion)
			r.Adopted = r.Err == nil
			_ = m.recordAdopt(kind, r.Err)
		}
		out = append(out, r)
	}
	return out
}

func (m *Manager) adopt(ctx context.Context, kind, channel, pathVersion string) (string, error) {
	rctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	version, err := m.Resolve(rctx, kind, channel)
	cancel()
	if err != nil {
		return "", err
	}
	ictx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	if ValidVersion(pathVersion) && CompareVersions(pathVersion, version) > 0 {
		if v, err := m.adoptInstall(ictx, kind, pathVersion); err == nil {
			return v, nil
		}
		// The user's exact build may not be published (a prerelease, a
		// vendor-patched copy); fall back to the channel.
	}
	return m.adoptInstall(ictx, kind, version)
}

// adoptInstall installs and activates version unless something activated a
// managed copy meanwhile (a concurrent amux harness install wins).
func (m *Manager) adoptInstall(ctx context.Context, kind, version string) (string, error) {
	unlock, err := m.lock(kind)
	if err != nil {
		return "", err
	}
	defer unlock()
	if cur := m.Current(kind); cur != "" {
		return cur, nil
	}
	if st := m.LoadState(kind); st.Declined {
		return "", fmt.Errorf("%s managed install was uninstalled meanwhile", kind)
	}
	if err := m.installLocked(ctx, kind, version); err != nil {
		return "", err
	}
	return version, m.activateLocked(kind, version)
}

func (m *Manager) recordAdopt(kind string, adoptErr error) error {
	unlock, err := m.lock(kind)
	if err != nil {
		return err
	}
	defer unlock()
	st := m.LoadState(kind)
	st.AdoptAttemptAt = m.now().UTC()
	st.AdoptError = ""
	if adoptErr != nil {
		st.AdoptError = adoptErr.Error()
	}
	return m.saveState(kind, st)
}

func upper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - 'a' + 'A'
		}
	}
	return string(b)
}
