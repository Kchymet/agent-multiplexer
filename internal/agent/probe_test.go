package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestProbeVerifiesConfiguredExecutable(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		ok         bool
	}{
		{"working", "echo 'fixture 1.2.3'", true},
		{"broken", "echo 'fixture 1.2.3'; exit 1", false},
		{"silent", "exit 0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cli")
			if err := os.WriteFile(path, []byte("#!/bin/sh\n"+tc.body+"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("AMUX_CODEX_BIN", path)
			r := Probe(context.Background(), "codex")
			if (r.Err == nil) != tc.ok || r.Path != path {
				t.Fatalf("probe = %+v", r)
			}
		})
	}
}

func TestProbeResolvesConfiguredNameToActualPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codex-fixture")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho fixture 1.2.3\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("AMUX_CODEX_BIN", "codex-fixture")
	r := Probe(context.Background(), "codex")
	if r.Err != nil {
		t.Fatalf("Probe: %v", r.Err)
	}
	if r.Path != path {
		t.Errorf("Probe resolved %q, want executable path %q", r.Path, path)
	}
}

func TestProbeCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cli")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nwhile :; do :; done\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMUX_CLAUDE_BIN", path)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if r := Probe(ctx, "claude"); r.Err == nil {
		t.Fatal("accepted timed-out executable")
	}
	if time.Since(start) > time.Second {
		t.Fatal("probe ignored timeout")
	}
}

func TestResolverHonorsDeadline(t *testing.T) {
	dir := t.TempDir()
	shell := filepath.Join(dir, "slow-shell")
	if err := os.WriteFile(shell, []byte("#!/bin/sh\nwhile :; do :; done\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("HOME", dir)
	t.Setenv("SHELL", shell)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if got := resolveContext(ctx, "missing-fixture"); got != "missing-fixture" {
		t.Fatalf("resolveContext = %q, want unresolved name", got)
	}
	if time.Since(start) > time.Second {
		t.Fatal("resolver ignored context deadline")
	}
}

// TestProbeReportsAbsentExecutableAsNotInstalled pins the distinction provider
// discovery relies on to keep an optional harness optional: a CLI that is not
// on this host at all (by name or by an override path) is ErrNotInstalled,
// while an installed CLI that fails its version check is not.
func TestProbeReportsAbsentExecutableAsNotInstalled(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	t.Setenv("HOME", dir)
	t.Setenv("SHELL", filepath.Join(dir, "no-shell"))

	t.Setenv("AMUX_HERMES_BIN", "")
	if r := Probe(context.Background(), "hermes"); !errors.Is(r.Err, ErrNotInstalled) {
		t.Fatalf("hermes absent from PATH: probe = %+v, want ErrNotInstalled", r)
	}
	t.Setenv("AMUX_HERMES_BIN", filepath.Join(dir, "missing", "hermes"))
	if r := Probe(context.Background(), "hermes"); !errors.Is(r.Err, ErrNotInstalled) {
		t.Fatalf("missing override path: probe = %+v, want ErrNotInstalled", r)
	}

	broken := filepath.Join(dir, "hermes")
	if err := os.WriteFile(broken, []byte("#!/bin/sh\nexit 3\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMUX_HERMES_BIN", "")
	r := Probe(context.Background(), "hermes")
	if r.Err == nil || errors.Is(r.Err, ErrNotInstalled) {
		t.Fatalf("installed but failing CLI: probe = %+v, want a version-check failure", r)
	}
}
