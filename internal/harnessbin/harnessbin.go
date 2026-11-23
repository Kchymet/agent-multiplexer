// Package harnessbin owns amux-managed, standalone installs of the agent CLIs
// (Claude Code and Codex), independent of whatever the user installed globally.
//
// Layout, under <data dir>/harnesses (core.DataDir, normally
// ~/.local/share/amux/harnesses):
//
//	<kind>/<version>/bin/<kind>   one immutable directory per installed version
//	<kind>/current -> <version>   relative symlink, swapped atomically (rename)
//	<kind>/state.json             pinned / previous / last update-check result
//	<kind>/.lock                  flock serializing every mutation of <kind>
//	<kind>/.staging-*             in-progress downloads (removed on failure)
//
// A launch resolves current to the concrete version directory once (see
// Resolve), so a running session keeps executing the version it started with
// while new launches pick up an update. GC never removes the current, previous
// (rollback target) or pinned version, nor any version a live process is
// executing from.
package harnessbin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"amux/internal/core"
)

// Kinds are the harnesses amux can install. Hermes and other harnesses keep the
// PATH/override resolution only.
var Kinds = []string{"claude", "codex"}

// Supported reports whether amux can manage installs of kind.
func Supported(kind string) bool {
	for _, k := range Kinds {
		if k == kind {
			return true
		}
	}
	return false
}

// Root is the managed-install root.
func Root() string { return filepath.Join(core.DataDir(), "harnesses") }

var versionRE = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$`)

// ValidVersion reports whether v is a concrete release version that is safe to
// use as a directory name (no channel names, separators or traversal).
func ValidVersion(v string) bool { return versionRE.MatchString(v) && !strings.Contains(v, "..") }

// State is the persisted per-kind lifecycle metadata.
type State struct {
	// Pinned freezes the active version: update (manual or automatic) refuses
	// to move off it until unpinned.
	Pinned string `json:"pinned,omitempty"`
	// Previous is the version that was current before the last activation; it
	// is the rollback target and is never garbage-collected.
	Previous string `json:"previous,omitempty"`
	// Available is the newest version the last update check saw (notify mode).
	Available string `json:"available,omitempty"`
	// CheckedAt is when the last update check completed.
	CheckedAt time.Time `json:"checked_at,omitempty"`
	// CheckError is the last update check's failure, if any.
	CheckError string `json:"check_error,omitempty"`
	// Declined records that the user uninstalled the managed copy: automatic
	// adoption never reinstalls it. amux harness install clears it.
	Declined bool `json:"declined,omitempty"`
	// AdoptAttemptAt and AdoptError are the last automatic adoption attempt
	// and its failure (rate-limits retries; see AdoptRetry).
	AdoptAttemptAt time.Time `json:"adopt_attempt_at,omitempty"`
	AdoptError     string    `json:"adopt_error,omitempty"`
	// AdoptCheckedAt, AdoptDecision ("adopt" or "skip") and AdoptReason are
	// the last time the adoption rules ran for this kind, in the daemon or in
	// `amux harness adopt`, and what they decided and why. Declines are
	// recorded too, so a harness that is never adopted says why.
	AdoptCheckedAt time.Time `json:"adopt_checked_at,omitempty"`
	AdoptDecision  string    `json:"adopt_decision,omitempty"`
	AdoptReason    string    `json:"adopt_reason,omitempty"`
	// AdoptStartedAt is set while an allowed adoption installs and cleared
	// when it finishes, successfully or not, so an attempt that never finished
	// (a stalled download, a daemon stopped mid-install) shows.
	AdoptStartedAt time.Time `json:"adopt_started_at,omitempty"`
}

// Manager performs installs and lifecycle operations under one root. The zero
// value is not usable; use New.
type Manager struct {
	Root    string
	Sources map[string]Source
	// InUse lists executable paths of running processes; GC keeps any version
	// directory containing one. Defaults to a /proc (Linux) or ps (macOS) scan.
	InUse func() ([]string, error)
	// Verify runs an installed binary to confirm it executes. Defaults to
	// `<bin> --version` with a timeout.
	Verify func(ctx context.Context, bin string) (string, error)
	// Now is the clock (tests).
	Now func() time.Time
}

// New returns a Manager over the default root with the official sources.
func New() *Manager {
	return &Manager{Root: Root(), Sources: DefaultSources()}
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) source(kind string) (Source, error) {
	if !Supported(kind) {
		return nil, fmt.Errorf("amux cannot manage %q installs (supported: %s)", kind, strings.Join(Kinds, ", "))
	}
	s, ok := m.Sources[kind]
	if !ok || s == nil {
		return nil, fmt.Errorf("no install source for %s", kind)
	}
	return s, nil
}

func (m *Manager) kindDir(kind string) string { return filepath.Join(m.Root, kind) }

// VersionDir is the directory of one installed version.
func (m *Manager) VersionDir(kind, version string) string {
	return filepath.Join(m.kindDir(kind), version)
}

// BinPath is the launch executable inside a version directory.
func (m *Manager) BinPath(kind, version string) string {
	return filepath.Join(m.VersionDir(kind, version), "bin", kind)
}

func (m *Manager) currentLink(kind string) string { return filepath.Join(m.kindDir(kind), "current") }

// Current returns the active version, or "" when nothing is installed/active.
func (m *Manager) Current(kind string) string {
	target, err := os.Readlink(m.currentLink(kind))
	if err != nil {
		return ""
	}
	v := filepath.Base(target)
	if !ValidVersion(v) || !executable(m.BinPath(kind, v)) {
		return ""
	}
	return v
}

// Installed lists installed versions, oldest first by semantic version.
func (m *Manager) Installed(kind string) []string {
	entries, err := os.ReadDir(m.kindDir(kind))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && ValidVersion(e.Name()) && executable(m.BinPath(kind, e.Name())) {
			out = append(out, e.Name())
		}
	}
	sort.Slice(out, func(i, j int) bool { return CompareVersions(out[i], out[j]) < 0 })
	return out
}

// LoadState reads kind's state; a missing file is the zero State.
func (m *Manager) LoadState(kind string) State {
	var s State
	b, err := os.ReadFile(filepath.Join(m.kindDir(kind), "state.json"))
	if err == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s
}

func (m *Manager) saveState(kind string, s State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(m.kindDir(kind), "state.json"), append(b, '\n'), 0o644)
}

// lock takes kind's exclusive mutation lock, creating the kind dir.
func (m *Manager) lock(kind string) (func(), error) {
	if err := os.MkdirAll(m.kindDir(kind), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(m.kindDir(kind), ".lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock %s installs: %w", kind, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// Resolve turns a version request (a concrete version, a channel name such as
// "stable"/"latest", or "" for the source's default channel) into a version.
func (m *Manager) Resolve(ctx context.Context, kind, request string) (string, error) {
	src, err := m.source(kind)
	if err != nil {
		return "", err
	}
	if ValidVersion(request) {
		return request, nil
	}
	if request == "" {
		request = src.DefaultChannel()
	}
	v, err := src.Latest(ctx, request)
	if err != nil {
		return "", err
	}
	if !ValidVersion(v) {
		return "", fmt.Errorf("%s release channel %q returned an invalid version %q", kind, request, v)
	}
	return v, nil
}

// Install ensures version (a concrete version or a channel) is installed and,
// when activate is set, makes it current. It returns the concrete version.
func (m *Manager) Install(ctx context.Context, kind, request string, activate bool) (string, error) {
	version, err := m.Resolve(ctx, kind, request)
	if err != nil {
		return "", err
	}
	unlock, err := m.lock(kind)
	if err != nil {
		return "", err
	}
	defer unlock()
	if st := m.LoadState(kind); activate && st.Pinned != "" && st.Pinned != version {
		return "", fmt.Errorf("%s is pinned to %s (run amux harness unpin %s first): %w", kind, st.Pinned, kind, ErrPinned)
	}
	if err := m.installLocked(ctx, kind, version); err != nil {
		return "", err
	}
	if activate {
		if err := m.activateLocked(kind, version); err != nil {
			return "", err
		}
		// An explicit install opts back in to a kind the user had uninstalled.
		if st := m.LoadState(kind); st.Declined {
			st.Declined = false
			if err := m.saveState(kind, st); err != nil {
				return "", err
			}
		}
	}
	return version, nil
}

func (m *Manager) installLocked(ctx context.Context, kind, version string) error {
	if executable(m.BinPath(kind, version)) {
		return nil
	}
	src, err := m.source(kind)
	if err != nil {
		return err
	}
	stage := filepath.Join(m.kindDir(kind), ".staging-"+version+"-"+randHex())
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return err
	}
	defer os.RemoveAll(stage) // no-op after the rename below
	if err := src.Fetch(ctx, version, stage); err != nil {
		return fmt.Errorf("install %s %s: %w", kind, version, err)
	}
	bin := filepath.Join(stage, "bin", kind)
	if !executable(bin) {
		return fmt.Errorf("install %s %s: package has no executable bin/%s", kind, version, kind)
	}
	if _, err := m.verify(ctx, bin); err != nil {
		return fmt.Errorf("install %s %s: downloaded binary does not run: %w", kind, version, err)
	}
	dest := m.VersionDir(kind, version)
	_ = os.RemoveAll(dest) // a broken partial dir (no executable) from an older failure
	if err := os.Rename(stage, dest); err != nil {
		return fmt.Errorf("install %s %s: %w", kind, version, err)
	}
	return nil
}

func (m *Manager) verify(ctx context.Context, bin string) (string, error) {
	if m.Verify != nil {
		return m.Verify(ctx, bin)
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--version")
	cmd.Env = append(os.Environ(), "DISABLE_AUTOUPDATER=1")
	// Without a WaitDelay, Output keeps waiting for stdout to reach EOF after
	// the timeout kills bin, so a helper process that inherited the pipe would
	// hang the install (and the daemon's adoption) indefinitely.
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil && !errors.Is(err, exec.ErrWaitDelay) { // ErrWaitDelay: it exited 0
		return "", err
	}
	return strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0]), nil
}

// activateLocked atomically points current at version, recording the version it
// replaces as the rollback target.
func (m *Manager) activateLocked(kind, version string) error {
	if !executable(m.BinPath(kind, version)) {
		return fmt.Errorf("%s %s is not installed", kind, version)
	}
	prev := m.Current(kind)
	if prev == version {
		return nil
	}
	tmp := filepath.Join(m.kindDir(kind), ".current-"+randHex())
	if err := os.Symlink(version, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, m.currentLink(kind)); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	st := m.LoadState(kind)
	if prev != "" {
		st.Previous = prev
	}
	if st.Available != "" && CompareVersions(version, st.Available) >= 0 {
		st.Available = ""
	}
	return m.saveState(kind, st)
}

// Activate makes an installed version current. It refuses to move a pinned
// kind unless the target is the pin.
func (m *Manager) Activate(kind, version string) error {
	if !ValidVersion(version) {
		return fmt.Errorf("invalid version %q", version)
	}
	unlock, err := m.lock(kind)
	if err != nil {
		return err
	}
	defer unlock()
	if st := m.LoadState(kind); st.Pinned != "" && st.Pinned != version {
		return fmt.Errorf("%s is pinned to %s (run amux harness unpin %s first)", kind, st.Pinned, kind)
	}
	return m.activateLocked(kind, version)
}

// ErrPinned is returned by Update when the kind is pinned.
var ErrPinned = errors.New("pinned")

// Update installs and activates the newest version on the default channel
// (or request, when non-empty). It returns the old and new current versions.
func (m *Manager) Update(ctx context.Context, kind, request string) (from, to string, err error) {
	from = m.Current(kind)
	if st := m.LoadState(kind); st.Pinned != "" {
		return from, from, fmt.Errorf("%s is pinned to %s: %w", kind, st.Pinned, ErrPinned)
	}
	to, err = m.Install(ctx, kind, request, true)
	return from, to, err
}

// Pin installs (if needed) and activates version, then freezes it.
func (m *Manager) Pin(ctx context.Context, kind, request string) (string, error) {
	version, err := m.Resolve(ctx, kind, request)
	if err != nil {
		return "", err
	}
	unlock, err := m.lock(kind)
	if err != nil {
		return "", err
	}
	defer unlock()
	if err := m.installLocked(ctx, kind, version); err != nil {
		return "", err
	}
	if err := m.activateLocked(kind, version); err != nil {
		return "", err
	}
	st := m.LoadState(kind)
	st.Pinned = version
	st.Declined = false
	return version, m.saveState(kind, st)
}

// Unpin lifts a pin; the current version stays active.
func (m *Manager) Unpin(kind string) error {
	unlock, err := m.lock(kind)
	if err != nil {
		return err
	}
	defer unlock()
	st := m.LoadState(kind)
	st.Pinned = ""
	return m.saveState(kind, st)
}

// Rollback re-activates the previous version (swapping previous and current).
func (m *Manager) Rollback(kind string) (from, to string, err error) {
	unlock, err := m.lock(kind)
	if err != nil {
		return "", "", err
	}
	defer unlock()
	st := m.LoadState(kind)
	from = m.Current(kind)
	if st.Pinned != "" {
		return from, from, fmt.Errorf("%s is pinned to %s (run amux harness unpin %s first)", kind, st.Pinned, kind)
	}
	if st.Previous == "" || st.Previous == from {
		return from, from, fmt.Errorf("%s has no previous version to roll back to", kind)
	}
	if !executable(m.BinPath(kind, st.Previous)) {
		return from, from, fmt.Errorf("%s previous version %s is no longer installed", kind, st.Previous)
	}
	to = st.Previous
	return from, to, m.activateLocked(kind, to)
}

// RecordCheck stores the result of an update check.
func (m *Manager) RecordCheck(kind, available string, checkErr error) error {
	unlock, err := m.lock(kind)
	if err != nil {
		return err
	}
	defer unlock()
	st := m.LoadState(kind)
	st.CheckedAt = m.now().UTC()
	st.CheckError = ""
	if checkErr != nil {
		st.CheckError = checkErr.Error()
	} else if cur := m.Current(kind); available != "" && (cur == "" || CompareVersions(available, cur) > 0) {
		st.Available = available
	} else {
		st.Available = ""
	}
	return m.saveState(kind, st)
}

// Uninstall removes every managed version of kind not in use by a running
// process, and the current link, so launches fall back to PATH. Versions still
// executing are left for a later GC.
func (m *Manager) Uninstall(kind string) (removed, kept []string, err error) {
	if !Supported(kind) {
		return nil, nil, fmt.Errorf("amux cannot manage %q installs", kind)
	}
	unlock, err := m.lock(kind)
	if err != nil {
		return nil, nil, err
	}
	defer unlock()
	if err := os.Remove(m.currentLink(kind)); err != nil && !os.IsNotExist(err) {
		return nil, nil, err
	}
	if err := m.saveState(kind, State{Declined: true}); err != nil {
		return nil, nil, err
	}
	return m.gcLocked(kind, map[string]bool{})
}

// GC removes installed versions other than current, previous, pinned, and any
// a live process is executing. When the running-process scan fails it removes
// nothing (it cannot prove a version unused).
func (m *Manager) GC(kind string) (removed, kept []string, err error) {
	unlock, err := m.lock(kind)
	if err != nil {
		return nil, nil, err
	}
	defer unlock()
	st := m.LoadState(kind)
	keep := map[string]bool{}
	for _, v := range []string{m.Current(kind), st.Previous, st.Pinned} {
		if v != "" {
			keep[v] = true
		}
	}
	return m.gcLocked(kind, keep)
}

func (m *Manager) gcLocked(kind string, keep map[string]bool) (removed, kept []string, err error) {
	m.cleanStaging(kind)
	installed := m.Installed(kind)
	var candidates []string
	for _, v := range installed {
		if keep[v] {
			kept = append(kept, v)
		} else {
			candidates = append(candidates, v)
		}
	}
	if len(candidates) == 0 {
		return nil, kept, nil
	}
	scan := m.InUse
	if scan == nil {
		scan = runningExecutables
	}
	exes, err := scan()
	if err != nil {
		return nil, installed, fmt.Errorf("cannot list running processes, keeping every version: %w", err)
	}
	for _, v := range candidates {
		// Match both the spelled and the symlink-resolved directory (macOS
		// reports /private/var for /var paths).
		prefixes := []string{m.VersionDir(kind, v) + string(filepath.Separator)}
		if real, err := filepath.EvalSymlinks(m.VersionDir(kind, v)); err == nil {
			prefixes = append(prefixes, real+string(filepath.Separator))
		}
		busy := false
		for _, exe := range exes {
			for _, prefix := range prefixes {
				if strings.HasPrefix(exe, prefix) {
					busy = true
				}
			}
		}
		if busy {
			kept = append(kept, v)
			continue
		}
		if err := os.RemoveAll(m.VersionDir(kind, v)); err != nil {
			return removed, kept, err
		}
		removed = append(removed, v)
	}
	return removed, kept, nil
}

// cleanStaging removes abandoned staging dirs and temporary links older than
// an hour (a live install is always younger and holds the lock anyway).
func (m *Manager) cleanStaging(kind string) {
	entries, _ := os.ReadDir(m.kindDir(kind))
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".staging-") && !strings.HasPrefix(e.Name(), ".current-") {
			continue
		}
		if info, err := e.Info(); err == nil && m.now().Sub(info.ModTime()) > time.Hour {
			_ = os.RemoveAll(filepath.Join(m.kindDir(kind), e.Name()))
		}
	}
}

// ManagedVersionRoot reports, for a path inside a managed version directory,
// that version directory (the unit a sandbox must mount to run it).
func ManagedVersionRoot(p string) (string, bool) {
	return managedVersionRoot(Root(), p)
}

func managedVersionRoot(root, p string) (string, bool) {
	rel, err := filepath.Rel(root, filepath.Clean(p))
	if err != nil {
		return "", false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) < 3 || !Supported(parts[0]) || !ValidVersion(parts[1]) {
		return "", false
	}
	return filepath.Join(root, parts[0], parts[1]), true
}

// CompareVersions orders dotted numeric versions; a pre-release suffix sorts
// before the release it precedes. It returns -1, 0 or +1.
func CompareVersions(a, b string) int {
	split := func(v string) ([]int, string) {
		core, pre, _ := strings.Cut(strings.SplitN(v, "+", 2)[0], "-")
		var nums []int
		for _, p := range strings.Split(core, ".") {
			n := 0
			for _, r := range p {
				if r < '0' || r > '9' {
					break
				}
				n = n*10 + int(r-'0')
			}
			nums = append(nums, n)
		}
		return nums, pre
	}
	an, ap := split(a)
	bn, bp := split(b)
	for i := 0; i < len(an) || i < len(bn); i++ {
		var x, y int
		if i < len(an) {
			x = an[i]
		}
		if i < len(bn) {
			y = bn[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	switch {
	case ap == bp:
		return 0
	case ap == "":
		return 1
	case bp == "":
		return -1
	case ap < bp:
		return -1
	default:
		return 1
	}
}

func executable(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0
}

func randHex() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// ActiveBin returns the concrete executable of kind's current managed version
// (resolved through the current link, so a caller that launches it keeps that
// version even if current later moves), or ok=false when none is installed.
func (m *Manager) ActiveBin(kind string) (path, version string, ok bool) {
	if !Supported(kind) {
		return "", "", false
	}
	v := m.Current(kind)
	if v == "" {
		return "", "", false
	}
	return m.BinPath(kind, v), v, true
}

// ActiveBin is Manager.ActiveBin over the default root.
func ActiveBin(kind string) (path, version string, ok bool) {
	return (&Manager{Root: Root()}).ActiveBin(kind)
}
