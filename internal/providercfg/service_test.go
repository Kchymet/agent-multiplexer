package providercfg

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRunner records the service-manager commands a manager would run and replies
// from a scripted table, so install/uninstall/probe are testable without a
// systemd or launchd session — and without installing anything on the machine
// running the tests.
type fakeRunner struct {
	calls []string
	reply map[string]string // command line → output
	fail  map[string]bool   // command line → non-zero exit
}

func (f *fakeRunner) run(name string, args ...string) (string, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, line)
	out := f.reply[line]
	if f.fail[line] {
		return out, errors.New("exit status 1")
	}
	return out, nil
}

func testUnit(dir string) Unit {
	return Unit{
		Exec:       "/home/u/.local/bin/amux",
		Args:       []string{"provide"},
		Config:     filepath.Join(dir, "provider.toml"),
		LogPath:    filepath.Join(dir, "provider.log"),
		RestartSec: 5,
	}
}

// ---- rendering (pure, and the thing that actually reaches disk) ----------

func TestRenderSystemdUnit(t *testing.T) {
	got := RenderSystemdUnit(testUnit("/cfg"))
	for _, want := range []string{
		"[Unit]",
		"Description=" + description,
		"After=network-online.target",
		"[Service]",
		"ExecStart=/home/u/.local/bin/amux provide",
		"Restart=always",
		"RestartSec=5",
		"[Install]",
		"WantedBy=default.target",
		"/cfg/provider.toml", // the header points at the settings
	} {
		if !strings.Contains(got, want) {
			t.Errorf("unit missing %q:\n%s", want, got)
		}
	}
	// The token is a bearer credential and a unit file is world-readable: it must
	// never end up in the environment or the command line.
	if strings.Contains(strings.ToLower(got), "token") {
		t.Errorf("unit mentions a token:\n%s", got)
	}
}

// TestRenderSystemdUnitQuotesSpaces: systemd splits ExecStart on whitespace, so
// an amux installed under a path with a space must come back quoted rather than
// silently becoming two arguments.
func TestRenderSystemdUnitQuotesSpaces(t *testing.T) {
	u := testUnit("/cfg")
	u.Exec = "/home/My User/bin/amux"
	got := RenderSystemdUnit(u)
	if !strings.Contains(got, `ExecStart="/home/My User/bin/amux" provide`) {
		t.Errorf("unquoted ExecStart:\n%s", got)
	}
}

func TestRenderSystemdUnitEnvironment(t *testing.T) {
	u := testUnit("/cfg")
	u.Env = map[string]string{"B": "2", "A": "1"}
	got := RenderSystemdUnit(u)
	if !strings.Contains(got, "Environment=\"A=1\"\nEnvironment=\"B=2\"") {
		t.Errorf("environment is missing or unsorted:\n%s", got)
	}
}

func TestRenderLaunchdPlist(t *testing.T) {
	got := RenderLaunchdPlist(testUnit("/cfg"))
	for _, want := range []string{
		`<?xml version="1.0" encoding="UTF-8"?>`,
		"<key>Label</key>\n  <string>" + LaunchdLabel + "</string>",
		"<string>/home/u/.local/bin/amux</string>\n    <string>provide</string>",
		"<key>RunAtLoad</key>\n  <true/>",
		"<key>KeepAlive</key>\n  <true/>",
		"<key>StandardOutPath</key>\n  <string>/cfg/provider.log</string>",
		"</plist>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plist missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(strings.ToLower(got), "<string>token") {
		t.Errorf("plist mentions a token:\n%s", got)
	}
}

// TestRenderLaunchdPlistEscapes keeps a path with XML metacharacters from
// producing a plist launchd refuses to load.
func TestRenderLaunchdPlistEscapes(t *testing.T) {
	u := testUnit("/cfg")
	u.Exec = "/home/a&b/<amux>"
	got := RenderLaunchdPlist(u)
	if !strings.Contains(got, "<string>/home/a&amp;b/&lt;amux&gt;</string>") {
		t.Errorf("plist did not escape the path:\n%s", got)
	}
}

// ---- systemd install / uninstall / probe ---------------------------------

func TestSystemdInstall(t *testing.T) {
	dir := t.TempDir()
	r := &fakeRunner{}
	m := newSystemd(dir, r.run)

	if err := m.Install(testUnit(dir)); err != nil {
		t.Fatalf("Install: %v", err)
	}
	b, err := os.ReadFile(m.Path())
	if err != nil {
		t.Fatalf("unit not written: %v", err)
	}
	if string(b) != RenderSystemdUnit(testUnit(dir)) {
		t.Errorf("unit on disk differs from Render")
	}
	want := []string{
		"systemctl --user daemon-reload",
		"systemctl --user enable --now " + UnitName,
		// enable --now no-ops on an already-running service, so a reinstall over an
		// old binary must restart explicitly or keep serving the old one.
		"systemctl --user restart " + UnitName,
	}
	if strings.Join(r.calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("commands =\n%s\nwant\n%s", strings.Join(r.calls, "\n"), strings.Join(want, "\n"))
	}
}

// TestSystemdInstallWithoutSystemd: on WSL2 systemd is opt-in, and the failure
// must teach rather than leave a unit file behind that nothing will ever load.
func TestSystemdInstallWithoutSystemd(t *testing.T) {
	dir := t.TempDir()
	r := &fakeRunner{
		fail:  map[string]bool{"systemctl --user daemon-reload": true},
		reply: map[string]string{"systemctl --user daemon-reload": "Failed to connect to bus"},
	}
	m := newSystemd(dir, r.run)

	err := m.Install(testUnit(dir))
	if err == nil {
		t.Fatal("Install = nil, want an error when systemd is unreachable")
	}
	if !strings.Contains(err.Error(), "Failed to connect to bus") {
		t.Errorf("error hides systemctl's own complaint: %v", err)
	}
	if _, statErr := os.Stat(m.Path()); !os.IsNotExist(statErr) {
		t.Errorf("a failed install left %s behind", m.Path())
	}
}

func TestSystemdUninstall(t *testing.T) {
	dir := t.TempDir()
	r := &fakeRunner{}
	m := newSystemd(dir, r.run)
	if err := m.Install(testUnit(dir)); err != nil {
		t.Fatalf("Install: %v", err)
	}
	r.calls = nil

	if err := m.Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(m.Path()); !os.IsNotExist(err) {
		t.Errorf("unit still on disk after uninstall")
	}
	if got := strings.Join(r.calls, "\n"); !strings.Contains(got, "disable --now") {
		t.Errorf("uninstall never stopped the service:\n%s", got)
	}

	// Uninstalling twice is the same request as uninstalling once: "not installed
	// and not running" is already the goal state.
	if err := m.Uninstall(); err != nil {
		t.Errorf("second Uninstall = %v, want nil", err)
	}
}

func TestSystemdProbe(t *testing.T) {
	dir := t.TempDir()
	isEnabled := "systemctl --user is-enabled " + UnitName
	isActive := "systemctl --user is-active " + UnitName

	t.Run("not installed", func(t *testing.T) {
		r := &fakeRunner{reply: map[string]string{isEnabled: "", isActive: "inactive"}}
		if got := newSystemd(dir, r.run).Probe(); got.Installed || got.Active {
			t.Errorf("Probe = %+v, want nothing installed", got)
		}
	})

	t.Run("running", func(t *testing.T) {
		r := &fakeRunner{reply: map[string]string{isEnabled: "enabled", isActive: "active"}}
		m := newSystemd(dir, r.run)
		if err := m.Install(testUnit(dir)); err != nil {
			t.Fatalf("Install: %v", err)
		}
		got := m.Probe()
		if !got.Installed || !got.Enabled || !got.Active || got.Detail != "active" {
			t.Errorf("Probe = %+v, want installed+enabled+active", got)
		}
	})

	t.Run("failed", func(t *testing.T) {
		// systemctl reports its verdict on stdout with a non-zero exit; the probe
		// must read the word, not the exit status, or every dead service reads as
		// "unknown".
		r := &fakeRunner{
			reply: map[string]string{isEnabled: "enabled", isActive: "failed"},
			fail:  map[string]bool{isActive: true},
		}
		m := newSystemd(dir, r.run)
		if err := m.Install(testUnit(dir)); err != nil {
			t.Fatalf("Install: %v", err)
		}
		got := m.Probe()
		if got.Active || got.Detail != "failed" {
			t.Errorf("Probe = %+v, want detail %q and not active", got, "failed")
		}
	})
}

// ---- launchd -------------------------------------------------------------

func TestLaunchdInstall(t *testing.T) {
	dir := t.TempDir()
	r := &fakeRunner{}
	m := newLaunchd(dir, r.run)

	if err := m.Install(testUnit(dir)); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := os.Stat(m.Path()); err != nil {
		t.Fatalf("plist not written: %v", err)
	}
	joined := strings.Join(r.calls, "\n")
	// Bootout before bootstrap, or a reinstall keeps serving the stale plist.
	bootout, bootstrap := strings.Index(joined, "launchctl bootout"), strings.Index(joined, "launchctl bootstrap")
	if bootout < 0 || bootstrap < 0 || bootout > bootstrap {
		t.Errorf("bootout must precede bootstrap:\n%s", joined)
	}
}

// TestLaunchdInstallFallsBackToLoad: older macOS only speaks `launchctl load -w`,
// and an install that fails there should say both things it tried.
func TestLaunchdInstallFallsBackToLoad(t *testing.T) {
	dir := t.TempDir()
	m := newLaunchd(dir, nil)
	r := &fakeRunner{fail: map[string]bool{"launchctl bootstrap " + m.domain() + " " + m.Path(): true}}
	m = newLaunchd(dir, r.run)

	if err := m.Install(testUnit(dir)); err != nil {
		t.Fatalf("Install = %v, want the load -w fallback to carry it", err)
	}
	if !strings.Contains(strings.Join(r.calls, "\n"), "launchctl load -w") {
		t.Errorf("no fallback attempted:\n%s", strings.Join(r.calls, "\n"))
	}
}

func TestLaunchdProbe(t *testing.T) {
	dir := t.TempDir()
	m := newLaunchd(dir, nil)
	print := "launchctl print " + m.target()

	r := &fakeRunner{reply: map[string]string{print: "state = running\npid = 42"}}
	m = newLaunchd(dir, r.run)
	if err := m.Install(testUnit(dir)); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if got := m.Probe(); !got.Installed || !got.Active {
		t.Errorf("Probe = %+v, want installed and running", got)
	}

	r2 := &fakeRunner{fail: map[string]bool{print: true}}
	if got := newLaunchd(dir, r2.run).Probe(); got.Active || got.Detail != "not loaded" {
		t.Errorf("Probe = %+v, want an unloaded agent", got)
	}
}

// ---- linger --------------------------------------------------------------

func TestLingerVia(t *testing.T) {
	if os.Getenv("USER") == "" {
		t.Setenv("USER", "tester")
	}
	cmd := "loginctl show-user " + currentUser() + " --property=Linger"

	for name, tc := range map[string]struct {
		reply         string
		fail          bool
		want, wantOK  bool
		skipOnNonUnix bool
	}{
		"enabled":     {reply: "Linger=yes", want: true, wantOK: true},
		"disabled":    {reply: "Linger=no", want: false, wantOK: true},
		"no loginctl": {fail: true, want: false, wantOK: false},
		"garbage":     {reply: "???", want: false, wantOK: false},
	} {
		t.Run(name, func(t *testing.T) {
			r := &fakeRunner{reply: map[string]string{cmd: tc.reply}, fail: map[string]bool{cmd: tc.fail}}
			got, ok := lingerVia(r.run)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("lingerVia = (%v, %v), want (%v, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestLingerHintNamesTheUser(t *testing.T) {
	t.Setenv("USER", "tester")
	if got := LingerHint(); got != "loginctl enable-linger tester" {
		t.Errorf("LingerHint = %q", got)
	}
}
