package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"amux/internal/harnessbin"
)

type staticSource struct{ latest string }

func (staticSource) DefaultChannel() string { return "stable" }
func (s staticSource) Latest(context.Context, string) (string, error) {
	return s.latest, nil
}
func (staticSource) Fetch(_ context.Context, version, dir string) error {
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "bin", "claude"), []byte("#!/bin/sh\necho "+version+"\n"), 0o755)
}

// The scheduled check runs only when update != off and a managed harness's
// last check is older than the interval, so restarts don't re-query servers.
func TestHarnessUpdateCheckHonorsModeAndInterval(t *testing.T) {
	noAdoption(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	src := &staticSource{latest: "1.0.0"}
	m := &harnessbin.Manager{
		Root:    t.TempDir(),
		Sources: map[string]harnessbin.Source{"claude": src},
		InUse:   func() ([]string, error) { return nil, nil },
		Verify:  func(context.Context, string) (string, error) { return "", nil },
		Now:     func() time.Time { return now },
	}
	ctx := context.Background()
	if _, err := m.Install(ctx, "claude", "1.0.0", true); err != nil {
		t.Fatal(err)
	}
	src.latest = "1.1.0"

	if err := harnessbin.Set(harnessbin.KeyUpdate, harnessbin.UpdateOff); err != nil {
		t.Fatal(err)
	}
	if res := runHarnessUpdateCheck(ctx, m, now, nil); res != nil {
		t.Fatalf("off ran a check: %+v", res)
	}
	// Unset is auto, unless the environment opts out.
	if err := harnessbin.Set(harnessbin.KeyUpdate, ""); err != nil {
		t.Fatal(err)
	}
	t.Setenv(harnessbin.DisableEnv, "1")
	if res := runHarnessUpdateCheck(ctx, m, now, nil); res != nil {
		t.Fatalf("%s=1 ran a check: %+v", harnessbin.DisableEnv, res)
	}
	t.Setenv(harnessbin.DisableEnv, "")
	if res := runHarnessUpdateCheck(ctx, m, now, nil); len(res) != 1 || !res[0].Updated {
		t.Fatalf("auto check = %+v", res)
	}
	if m.Current("claude") != "1.1.0" {
		t.Fatalf("current = %q", m.Current("claude"))
	}
	src.latest = "1.2.0"
	if res := runHarnessUpdateCheck(ctx, m, now.Add(time.Hour), nil); res != nil {
		t.Fatalf("check ran again before the interval: %+v", res)
	}
	if res := runHarnessUpdateCheck(ctx, m, now.Add(25*time.Hour), nil); len(res) != 1 || m.Current("claude") != "1.2.0" {
		t.Fatalf("check after interval = %+v current %q", res, m.Current("claude"))
	}
}

// noAdoption makes every harness look unused, so a test's scheduled check
// never probes or adopts the developer's real installs.
func noAdoption(t *testing.T) {
	t.Helper()
	old := harnessAdoptInput
	harnessAdoptInput = func(context.Context, string) harnessbin.AdoptInput { return harnessbin.AdoptInput{} }
	t.Cleanup(func() { harnessAdoptInput = old })
}

// With the default (auto) mode the daemon adopts a managed install for a
// harness the user already uses, and never for one they don't, or one an
// explicit override selects, or when the environment opts out.
func TestHarnessUpdateCheckAdoptsUsedHarnessesByDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	m := &harnessbin.Manager{
		Root:    t.TempDir(),
		Sources: map[string]harnessbin.Source{"claude": &staticSource{latest: "2.0.0"}, "codex": &staticSource{latest: "0.9.0"}},
		InUse:   func() ([]string, error) { return nil, nil },
		Verify:  func(context.Context, string) (string, error) { return "", nil },
		Now:     func() time.Time { return now },
	}
	inputs := map[string]harnessbin.AdoptInput{
		"claude": {OnPath: "/usr/local/bin/claude", EnvBin: "/opt/claude"}, // explicit override
		"codex":  {},                                                       // not used
	}
	old := harnessAdoptInput
	harnessAdoptInput = func(_ context.Context, kind string) harnessbin.AdoptInput { return inputs[kind] }
	t.Cleanup(func() { harnessAdoptInput = old })
	ctx := context.Background()

	runHarnessUpdateCheck(ctx, m, now, nil)
	if m.Current("claude") != "" || m.Current("codex") != "" {
		t.Fatalf("adopted against an override or an unused harness: claude %q codex %q", m.Current("claude"), m.Current("codex"))
	}
	inputs["claude"] = harnessbin.AdoptInput{OnPath: "/usr/local/bin/claude"}
	t.Setenv(harnessbin.DisableEnv, "1")
	runHarnessUpdateCheck(ctx, m, now, nil)
	if m.Current("claude") != "" {
		t.Fatalf("adopted despite %s", harnessbin.DisableEnv)
	}
	t.Setenv(harnessbin.DisableEnv, "")
	if err := harnessbin.Set(harnessbin.KeyUpdate, harnessbin.UpdateNotify); err != nil {
		t.Fatal(err)
	}
	runHarnessUpdateCheck(ctx, m, now, nil)
	if m.Current("claude") != "" {
		t.Fatal("notify mode adopted (only auto installs)")
	}
	if err := harnessbin.Set(harnessbin.KeyUpdate, ""); err != nil {
		t.Fatal(err)
	}
	runHarnessUpdateCheck(ctx, m, now, nil)
	if m.Current("claude") != "2.0.0" || m.Current("codex") != "" {
		t.Fatalf("default adoption: claude %q codex %q", m.Current("claude"), m.Current("codex"))
	}
}
