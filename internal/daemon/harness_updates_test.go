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

	if res := runHarnessUpdateCheck(ctx, m, now); res != nil {
		t.Fatalf("default (off) ran a check: %+v", res)
	}
	if err := harnessbin.Set(harnessbin.KeyUpdate, harnessbin.UpdateAuto); err != nil {
		t.Fatal(err)
	}
	if res := runHarnessUpdateCheck(ctx, m, now); len(res) != 1 || !res[0].Updated {
		t.Fatalf("auto check = %+v", res)
	}
	if m.Current("claude") != "1.1.0" {
		t.Fatalf("current = %q", m.Current("claude"))
	}
	src.latest = "1.2.0"
	if res := runHarnessUpdateCheck(ctx, m, now.Add(time.Hour)); res != nil {
		t.Fatalf("check ran again before the interval: %+v", res)
	}
	if res := runHarnessUpdateCheck(ctx, m, now.Add(25*time.Hour)); len(res) != 1 || m.Current("claude") != "1.2.0" {
		t.Fatalf("check after interval = %+v current %q", res, m.Current("claude"))
	}
}
