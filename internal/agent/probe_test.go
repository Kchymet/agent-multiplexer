package agent

import (
	"context"
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
