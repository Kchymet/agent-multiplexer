package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestResolveBinaryOrder pins the precedence: AMUX_<KIND>_BIN, then config
// harnesses.<kind>.bin, then the amux-managed install, then PATH.
func TestResolveBinaryOrder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SHELL", "/nonexistent")
	pathDir := t.TempDir()
	pathBin := filepath.Join(pathDir, "claude")
	if err := os.WriteFile(pathBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", pathDir)
	t.Setenv("AMUX_CLAUDE_BIN", "")

	config, managed := "", ""
	oldManaged, oldConfig := managedBin, configuredBin
	t.Cleanup(func() { managedBin, configuredBin = oldManaged, oldConfig })
	managedBin = func(kind string) (string, string, bool) {
		if managed == "" || kind != "claude" {
			return "", "", false
		}
		return managed, "2.1.311", true
	}
	configuredBin = func(kind string) string {
		if kind != "claude" {
			return ""
		}
		return config
	}
	ctx := context.Background()
	check := func(wantSource, wantPath string) {
		t.Helper()
		b := ResolveBinary(ctx, "claude")
		if b.Source != wantSource || b.Path != wantPath {
			t.Fatalf("ResolveBinary = %+v, want source %s path %s", b, wantSource, wantPath)
		}
		if argv, _ := Argv("claude", ""); argv[0] != wantPath {
			t.Fatalf("Argv[0] = %q, want %q", argv[0], wantPath)
		}
	}

	check(BinaryFromPath, pathBin)
	managed = "/data/amux/harnesses/claude/2.1.311/bin/claude"
	check(BinaryFromManaged, managed)
	if l := ResolveBinary(ctx, "claude").Label(); l != "amux-managed 2.1.311" {
		t.Fatalf("Label = %q", l)
	}
	config = "/opt/pinned/claude"
	check(BinaryFromConfig, config)
	t.Setenv("AMUX_CLAUDE_BIN", "/env/claude")
	check(BinaryFromEnv, "/env/claude")

	// Probe reports the same choice.
	if r := Probe(ctx, "claude"); r.Binary.Source != BinaryFromEnv || r.Path != "/env/claude" {
		t.Fatalf("Probe = %+v", r)
	}
}
