package daemon

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"amux/internal/harnessbin"
)

// kindSource installs a stub executable named for its kind.
type kindSource struct{ kind, latest string }

func (kindSource) DefaultChannel() string { return "stable" }
func (s kindSource) Latest(context.Context, string) (string, error) {
	return s.latest, nil
}
func (s kindSource) Fetch(_ context.Context, version, dir string) error {
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "bin", s.kind), []byte("#!/bin/sh\necho "+version+"\n"), 0o755)
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(old); log.SetFlags(flags) })
	return &buf
}

func adoptTestManager(t *testing.T, now time.Time) *harnessbin.Manager {
	t.Helper()
	return &harnessbin.Manager{
		Root:    filepath.Join(os.Getenv("HOME"), ".local", "share", "amux", "harnesses"),
		Sources: map[string]harnessbin.Source{"claude": kindSource{"claude", "2.1.0"}, "codex": kindSource{"codex", "0.171.0"}},
		InUse:   func() ([]string, error) { return nil, nil },
		Verify:  func(context.Context, string) (string, error) { return "ok", nil },
		Now:     func() time.Time { return now },
	}
}

// The daemon adopts the harnesses a launch from the same daemon would run,
// through the real adoption input (no seam): a detached daemon's minimal PATH
// lacks ~/.local/bin and its login shell prints startup noise and reports
// claude as an alias, yet both binaries resolve and both are adopted.
func TestHarnessUpdateAdoptsFromADaemonEnvironment(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", "/usr/bin:/bin:/usr/sbin:/sbin")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("AMUX_CLAUDE_BIN", "")
	t.Setenv("AMUX_CODEX_BIN", "")
	t.Setenv(harnessbin.DisableEnv, "")
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for kind, v := range map[string]string{"claude": "2.1.330 (Claude Code)", "codex": "codex-cli 0.171.0"} {
		if err := os.WriteFile(filepath.Join(bin, kind), []byte("#!/bin/sh\necho '"+v+"'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	shell := filepath.Join(home, "zsh")
	if err := os.WriteFile(shell, []byte("#!/bin/sh\necho 'Last login: Sun Nov 22 19:58:01 on ttys004'\n"+
		"case \"$2\" in *claude*) echo 'alias claude="+home+"/.claude/local/claude' ;; esac\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHELL", shell)
	logs := captureLog(t)
	now := time.Date(2026, 11, 22, 20, 0, 0, 0, time.UTC)
	m := adoptTestManager(t, now)

	runHarnessUpdateCheck(context.Background(), m, now, adoptionLog{})
	if m.Current("claude") != "2.1.330" || m.Current("codex") != "0.171.0" {
		t.Fatalf("adopted claude %q codex %q; want both (log:\n%s)", m.Current("claude"), m.Current("codex"), logs)
	}
	for _, kind := range []string{"claude", "codex"} {
		if st := m.LoadState(kind); st.AdoptDecision != harnessbin.AdoptDecisionAdopt || !strings.Contains(st.AdoptReason, filepath.Join(bin, kind)) {
			t.Errorf("%s state = %+v", kind, st)
		}
		if !strings.Contains(logs.String(), "adopting "+kind+": found "+filepath.Join(bin, kind)) {
			t.Errorf("no adopting line for %s in log:\n%s", kind, logs)
		}
	}
}

// A declined adoption is logged with its reason and what the daemon saw, once
// per daemon run (not every poll), and recorded for harness ls / doctor.
func TestHarnessUpdateLogsDeclinesOncePerRun(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv(harnessbin.DisableEnv, "")
	old := harnessAdoptInput
	harnessAdoptInput = func(context.Context, string) harnessbin.AdoptInput { return harnessbin.AdoptInput{} }
	t.Cleanup(func() { harnessAdoptInput = old })
	logs := captureLog(t)
	now := time.Date(2026, 11, 22, 20, 0, 0, 0, time.UTC)
	m := adoptTestManager(t, now)

	seen := adoptionLog{}
	runHarnessUpdateCheck(context.Background(), m, now, seen)
	runHarnessUpdateCheck(context.Background(), m, now.Add(harnessUpdatePoll), seen)
	for _, kind := range []string{"claude", "codex"} {
		line := "not adopting " + kind + ": " + kind + " is not installed or logged in [binary: none (PATH="
		if n := strings.Count(logs.String(), line); n != 1 {
			t.Errorf("%s decline logged %d times; want once per run. log:\n%s", kind, n, logs)
		}
		if st := m.LoadState(kind); st.AdoptDecision != harnessbin.AdoptDecisionSkip || !st.AdoptCheckedAt.Equal(now) {
			t.Errorf("%s decline not recorded: %+v", kind, st)
		}
	}

	// A new daemon run logs the decision again, and notify mode says why it
	// does not adopt at all.
	runHarnessUpdateCheck(context.Background(), m, now, adoptionLog{})
	if n := strings.Count(logs.String(), "not adopting claude: "); n != 2 {
		t.Errorf("claude decline logged %d times across two runs; want 2", n)
	}
	if err := harnessbin.Set(harnessbin.KeyUpdate, harnessbin.UpdateNotify); err != nil {
		t.Fatal(err)
	}
	runHarnessUpdateCheck(context.Background(), m, now, adoptionLog{})
	if !strings.Contains(logs.String(), "not adopting managed installs: harnesses.update is notify") {
		t.Errorf("notify mode not logged:\n%s", logs)
	}
}
