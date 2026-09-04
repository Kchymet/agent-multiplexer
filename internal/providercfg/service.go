package providercfg

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"amux/internal/core"
)

// Service identity. The systemd unit and the launchd label name the same thing —
// one amux provider per user — so installing twice replaces rather than stacks.
const (
	// UnitName is the systemd user unit `amux provide install` writes.
	UnitName = "amux-provide.service"
	// LaunchdLabel is the launchd agent label (reverse-DNS, as launchd expects).
	LaunchdLabel = "com.kchymet.amux.provide"
	// LaunchdPlist is the plist filename under ~/Library/LaunchAgents.
	LaunchdPlist = LaunchdLabel + ".plist"

	description = "amux remote provider (dials an orchestrator and serves panes)"
)

// ErrUnsupported is returned by Service on a platform amux has no user-service
// backend for. Provider mode still runs in the foreground there; only the
// install/uninstall verbs are unavailable.
var ErrUnsupported = errors.New("no user-service backend for this platform")

// Unit is everything a rendered service needs. It is the platform-neutral input
// to both renderers, so one construction site produces the systemd unit and the
// launchd plist alike.
type Unit struct {
	Exec       string            // absolute path to the amux binary
	Args       []string          // arguments after the binary (normally just "provide")
	Config     string            // config file path, recorded in the unit header
	LogPath    string            // launchd stdout/stderr sink (systemd uses the journal)
	Env        map[string]string // extra environment for the service process
	RestartSec int               // restart backoff in seconds
}

// DefaultUnit is the unit amux installs: run `<exec> provide`, which reads the
// config file, restarting on exit so a crash or a rebooted machine comes back.
func DefaultUnit(execPath, configPath string) Unit {
	return Unit{
		Exec:       execPath,
		Args:       []string{"provide"},
		Config:     configPath,
		LogPath:    filepath.Join(core.StateDir(), "provider.log"),
		RestartSec: 5,
	}
}

// Probe is what doctor needs to know about the installed service: whether the
// unit is on disk, whether the service manager has it enabled (starts at login /
// boot), whether it is running right now, and a human detail line for the rest.
type Probe struct {
	Installed bool
	Enabled   bool
	Active    bool
	Detail    string
}

// Manager is one platform's user-service backend.
type Manager interface {
	// Kind names the backend for humans ("systemd (user)", "launchd").
	Kind() string
	// Path is where the unit/plist lives.
	Path() string
	// Render returns the unit text Install would write — pure, so it is testable
	// and `--dry-run` can show exactly what lands on disk.
	Render(Unit) string
	// Install writes the unit and enables + starts the service. It is idempotent:
	// installing over an existing service replaces and restarts it.
	Install(Unit) error
	// Uninstall stops and disables the service and removes the unit. A service
	// that was never installed is not an error.
	Uninstall() error
	// Probe reports the current state, best-effort.
	Probe() Probe
}

// Service returns the user-service backend for this platform.
func Service() (Manager, error) {
	switch runtime.GOOS {
	case "linux":
		return newSystemd(systemdDir(), execRun), nil
	case "darwin":
		return newLaunchd(launchAgentsDir(), execRun), nil
	default:
		return nil, fmt.Errorf("%w (%s)", ErrUnsupported, runtime.GOOS)
	}
}

// runner runs a service-manager command and returns its combined output. The
// output is returned even when the command fails, because `systemctl is-active`
// reports its verdict on stdout with a non-zero exit — the probe reads the word,
// not the status.
type runner func(name string, args ...string) (string, error)

// execRun is the production runner: a real subprocess with a short deadline, so
// a wedged service manager degrades to a doctor line instead of hanging amux.
func execRun(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// ---- systemd (Linux, WSL2) ----------------------------------------------

// systemdDir is the systemd user-unit directory. It is XDG's, not amux's own
// config dir: systemd only reads units from $XDG_CONFIG_HOME/systemd/user.
func systemdDir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "systemd", "user")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "systemd", "user")
}

type systemdManager struct {
	dir string
	run runner
}

func newSystemd(dir string, run runner) *systemdManager { return &systemdManager{dir: dir, run: run} }

func (m *systemdManager) Kind() string { return "systemd (user)" }
func (m *systemdManager) Path() string { return filepath.Join(m.dir, UnitName) }

func (m *systemdManager) Render(u Unit) string { return RenderSystemdUnit(u) }

func (m *systemdManager) Install(u Unit) error {
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(m.Path(), []byte(RenderSystemdUnit(u)), 0o644); err != nil {
		return err
	}
	if out, err := m.run("systemctl", "--user", "daemon-reload"); err != nil {
		// The overwhelmingly common cause on WSL2 is systemd being off entirely,
		// which no amount of retrying fixes — so say what to turn on.
		_ = os.Remove(m.Path())
		return fmt.Errorf("systemctl --user daemon-reload failed: %w%s\n%s", err, detail(out), systemdMissingHint())
	}
	if out, err := m.run("systemctl", "--user", "enable", "--now", UnitName); err != nil {
		return fmt.Errorf("systemctl --user enable --now %s: %w%s", UnitName, err, detail(out))
	}
	// Installing over a running service must load the new unit, not keep the old
	// one alive: enable --now is a no-op when the service is already running.
	if out, err := m.run("systemctl", "--user", "restart", UnitName); err != nil {
		return fmt.Errorf("systemctl --user restart %s: %w%s", UnitName, err, detail(out))
	}
	return nil
}

func (m *systemdManager) Uninstall() error {
	// disable --now on an absent unit is an error we deliberately ignore: the goal
	// is "not installed and not running", which that state already satisfies.
	_, _ = m.run("systemctl", "--user", "disable", "--now", UnitName)
	if err := os.Remove(m.Path()); err != nil && !os.IsNotExist(err) {
		return err
	}
	_, _ = m.run("systemctl", "--user", "daemon-reload")
	_, _ = m.run("systemctl", "--user", "reset-failed", UnitName)
	return nil
}

func (m *systemdManager) Probe() Probe {
	p := Probe{}
	if _, err := os.Stat(m.Path()); err == nil {
		p.Installed = true
	}
	enabled, _ := m.run("systemctl", "--user", "is-enabled", UnitName)
	active, _ := m.run("systemctl", "--user", "is-active", UnitName)
	p.Enabled = firstWord(enabled) == "enabled"
	p.Active = firstWord(active) == "active"
	p.Detail = firstWord(active)
	if p.Detail == "" {
		p.Detail = "unknown"
	}
	return p
}

// RenderSystemdUnit renders the systemd user unit. It is a pure function of the
// Unit so the exact bytes that reach ~/.config/systemd/user are testable.
func RenderSystemdUnit(u Unit) string {
	var b strings.Builder
	b.WriteString("# " + description + "\n")
	b.WriteString("# Written by `amux provide install`. Settings live in " + u.Config + ";\n")
	b.WriteString("# edit that file and run `systemctl --user restart " + UnitName + "`.\n")
	b.WriteString("[Unit]\n")
	b.WriteString("Description=" + description + "\n")
	b.WriteString("Documentation=https://github.com/kchymet/agent-multiplexer/blob/master/docs/remote-provider.md\n")
	// The provider dials out, so it wants the network; it also backs off and
	// retries forever, so a not-yet-ready network delays the first connection
	// rather than failing the unit.
	b.WriteString("After=network-online.target\n")
	b.WriteString("Wants=network-online.target\n")
	// A terminal registration rejection (bad or revoked token) makes the provider
	// exit on purpose. Without a rate limit systemd would spin on it forever; with
	// one it gives up and shows as failed, which is what doctor reports. These live
	// in [Unit], not [Service] — systemd moved them there in v230 and warns about
	// the old location.
	b.WriteString("StartLimitIntervalSec=300\n")
	b.WriteString("StartLimitBurst=10\n\n")

	b.WriteString("[Service]\n")
	b.WriteString("Type=simple\n")
	b.WriteString("ExecStart=" + shellJoin(append([]string{u.Exec}, u.Args...)) + "\n")
	b.WriteString("Restart=always\n")
	b.WriteString("RestartSec=" + strconv.Itoa(restartSec(u)) + "\n")
	for _, kv := range sortedEnv(u.Env) {
		b.WriteString("Environment=" + kv + "\n")
	}
	b.WriteString("\n[Install]\n")
	b.WriteString("WantedBy=default.target\n")
	return b.String()
}

// systemdMissingHint is the teaching half of a daemon-reload failure: on WSL2
// systemd is opt-in, and without it no user service can exist at all.
func systemdMissingHint() string {
	if !IsWSL() {
		return "\nis a systemd user session running for this user? (`systemctl --user status`)"
	}
	return "\nWSL needs systemd turned on: put\n" +
		"    [boot]\n    systemd=true\n" +
		"in /etc/wsl.conf, then run `wsl --shutdown` from Windows and reopen this distro."
}

// ---- launchd (macOS) -----------------------------------------------------

func launchAgentsDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents")
}

type launchdManager struct {
	dir string
	run runner
}

func newLaunchd(dir string, run runner) *launchdManager { return &launchdManager{dir: dir, run: run} }

func (m *launchdManager) Kind() string { return "launchd" }
func (m *launchdManager) Path() string { return filepath.Join(m.dir, LaunchdPlist) }

func (m *launchdManager) Render(u Unit) string { return RenderLaunchdPlist(u) }

// target is the launchd domain-specific name for this user's agent.
func (m *launchdManager) target() string { return fmt.Sprintf("gui/%d/%s", os.Getuid(), LaunchdLabel) }
func (m *launchdManager) domain() string { return fmt.Sprintf("gui/%d", os.Getuid()) }

func (m *launchdManager) Install(u Unit) error {
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return err
	}
	if u.LogPath != "" {
		_ = os.MkdirAll(filepath.Dir(u.LogPath), 0o755)
	}
	if err := os.WriteFile(m.Path(), []byte(RenderLaunchdPlist(u)), 0o644); err != nil {
		return err
	}
	// Reinstall must replace: bootout the old agent (absent is fine) before
	// bootstrapping the new plist, or launchd keeps serving the stale one.
	_, _ = m.run("launchctl", "bootout", m.target())
	if out, err := m.run("launchctl", "bootstrap", m.domain(), m.Path()); err != nil {
		// Older macOS (and some sandboxed contexts) only speak load -w.
		if out2, err2 := m.run("launchctl", "load", "-w", m.Path()); err2 != nil {
			return fmt.Errorf("launchctl bootstrap %s: %w%s (load -w also failed: %v%s)",
				m.Path(), err, detail(out), err2, detail(out2))
		}
		return nil
	}
	_, _ = m.run("launchctl", "enable", m.target())
	_, _ = m.run("launchctl", "kickstart", "-k", m.target())
	return nil
}

func (m *launchdManager) Uninstall() error {
	_, _ = m.run("launchctl", "bootout", m.target())
	_, _ = m.run("launchctl", "unload", "-w", m.Path())
	if err := os.Remove(m.Path()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (m *launchdManager) Probe() Probe {
	p := Probe{}
	if _, err := os.Stat(m.Path()); err == nil {
		p.Installed = true
		p.Enabled = true // a plist in LaunchAgents is loaded at login by definition
	}
	out, err := m.run("launchctl", "print", m.target())
	if err != nil {
		p.Detail = "not loaded"
		return p
	}
	p.Active = strings.Contains(out, "state = running")
	if p.Active {
		p.Detail = "running"
	} else {
		p.Detail = "loaded, not running"
	}
	return p
}

// RenderLaunchdPlist renders the launchd user agent. Pure, like its systemd
// counterpart, so the XML that lands in ~/Library/LaunchAgents is testable from
// any platform.
func RenderLaunchdPlist(u Unit) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<!-- ` + description + ` -->` + "\n")
	b.WriteString(`<!-- Written by ` + "`amux provide install`" + `. Settings live in ` + xmlEscape(u.Config) + `. -->` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n<dict>\n")
	b.WriteString("  <key>Label</key>\n  <string>" + LaunchdLabel + "</string>\n")
	b.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	for _, a := range append([]string{u.Exec}, u.Args...) {
		b.WriteString("    <string>" + xmlEscape(a) + "</string>\n")
	}
	b.WriteString("  </array>\n")
	b.WriteString("  <key>RunAtLoad</key>\n  <true/>\n")
	b.WriteString("  <key>KeepAlive</key>\n  <true/>\n")
	b.WriteString("  <key>ThrottleInterval</key>\n  <integer>" + strconv.Itoa(restartSec(u)) + "</integer>\n")
	b.WriteString("  <key>ProcessType</key>\n  <string>Background</string>\n")
	if u.LogPath != "" {
		b.WriteString("  <key>StandardOutPath</key>\n  <string>" + xmlEscape(u.LogPath) + "</string>\n")
		b.WriteString("  <key>StandardErrorPath</key>\n  <string>" + xmlEscape(u.LogPath) + "</string>\n")
	}
	if len(u.Env) > 0 {
		b.WriteString("  <key>EnvironmentVariables</key>\n  <dict>\n")
		keys := make([]string, 0, len(u.Env))
		for k := range u.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteString("    <key>" + xmlEscape(k) + "</key>\n    <string>" + xmlEscape(u.Env[k]) + "</string>\n")
		}
		b.WriteString("  </dict>\n")
	}
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

// ---- linger (Linux) ------------------------------------------------------

// Linger reports whether this user's systemd services keep running when no login
// session is open. Without it a WSL2 provider dies the moment the last terminal
// closes — the exact failure `amux provide install` exists to prevent. known is
// false when the answer can't be determined (no loginctl, not Linux).
func Linger() (enabled, known bool) { return lingerVia(execRun) }

func lingerVia(run runner) (enabled, known bool) {
	if runtime.GOOS != "linux" {
		return false, false
	}
	out, err := run("loginctl", "show-user", currentUser(), "--property=Linger")
	if err != nil {
		return false, false
	}
	_, v, ok := strings.Cut(strings.TrimSpace(out), "=")
	if !ok {
		return false, false
	}
	return strings.EqualFold(strings.TrimSpace(v), "yes"), true
}

// LingerHint is the one command that makes the service survive logout.
func LingerHint() string { return "loginctl enable-linger " + currentUser() }

func currentUser() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	if u := os.Getenv("LOGNAME"); u != "" {
		return u
	}
	return strconv.Itoa(os.Getuid())
}

// IsWSL reports whether we're running under WSL, which changes the advice amux
// gives (systemd is opt-in there, and linger matters more).
func IsWSL() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	if os.Getenv("WSL_DISTRO_NAME") != "" || os.Getenv("WSL_INTEROP") != "" {
		return true
	}
	b, err := os.ReadFile("/proc/version")
	return err == nil && strings.Contains(strings.ToLower(string(b)), "microsoft")
}

// ---- shared helpers ------------------------------------------------------

func restartSec(u Unit) int {
	if u.RestartSec > 0 {
		return u.RestartSec
	}
	return 5
}

// shellJoin renders an argv for a systemd ExecStart line, quoting any argument
// that contains whitespace (systemd splits ExecStart on spaces).
func shellJoin(argv []string) string {
	out := make([]string, len(argv))
	for i, a := range argv {
		if strings.ContainsAny(a, " \t\"") {
			out[i] = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(a) + `"`
		} else {
			out[i] = a
		}
	}
	return strings.Join(out, " ")
}

func sortedEnv(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, `"`+k+`=`+env[k]+`"`)
	}
	return out
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}

func firstWord(s string) string {
	f := strings.Fields(strings.TrimSpace(s))
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// detail appends a service manager's own output to an error, indented, so the
// user sees systemctl's complaint rather than just an exit status.
func detail(out string) string {
	out = strings.TrimSpace(out)
	if out == "" {
		return ""
	}
	return "\n    " + strings.ReplaceAll(out, "\n", "\n    ")
}
