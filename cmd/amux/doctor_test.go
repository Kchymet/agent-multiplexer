package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"amux/internal/core"
	"amux/internal/provider"
	"amux/internal/providercfg"
)

// TestReconcile drives the pure reconciliation core: it flags worktree dirs and
// branches with no session and sessions with no dir, while recognizing a moved
// agent (dir under its original root) by agent id rather than mis-flagging it.
func TestReconcile(t *testing.T) {
	sessions := t.TempDir()
	// On disk: r1/{a1,aX}, and r2/a3 (a3 was moved to workgroup r9 but its dir
	// still lives under its original root r2).
	for _, p := range []string{"r1/a1", "r1/aX", "r2/a3", "r9/a4"} {
		if err := os.MkdirAll(filepath.Join(sessions, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A stray file (not a dir) under a root must be ignored.
	if err := os.WriteFile(filepath.Join(sessions, "r1", "note.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	roots := []core.WorkgroupRow{
		{ID: "r1", Agents: []core.AgentRow{
			{ID: "a1", Branch: "amux/r1-a1"},
			{ID: "a2", Branch: "amux/r1-a2"}, // known, but no dir on disk
		}},
		{ID: "r9", Agents: []core.AgentRow{
			{ID: "a3", Branch: "amux/r2-a3"}, // moved out of r2; dir still under r2
			{ID: "a4", Branch: ""},           // live agent with a blank stored branch
		}},
	}
	disk := []branchRef{
		{Repo: "acme/api", Branch: "amux/r1-a1"},  // known via stored branch
		{Repo: "acme/api", Branch: "amux/r9-a4"},  // known via agent id (blank stored branch)
		{Repo: "acme/api", Branch: "amux/gone-x"}, // orphan (deleted agent)
	}

	orphanDirs, missingDirs, orphanBranches := reconcile(sessions, roots, disk)

	if want := []string{"r1/aX"}; !reflect.DeepEqual(orphanDirs, want) {
		t.Errorf("orphanDirs = %v, want %v", orphanDirs, want)
	}
	if want := []string{"a2"}; !reflect.DeepEqual(missingDirs, want) {
		t.Errorf("missingDirs = %v, want %v", missingDirs, want)
	}
	if want := []string{"acme/api · amux/gone-x"}; !reflect.DeepEqual(orphanBranches, want) {
		t.Errorf("orphanBranches = %v, want %v", orphanBranches, want)
	}
}

// TestAgentIDFromBranchRoundTrips pins that agentIDFromBranch is the inverse of
// core.BranchFor for the agent id — the pair encode/decode the branch scheme, so
// the reconciliation's blank-stored-branch fallback stays correct even if the
// scheme changes. (It would catch agent ids gaining a hyphen, which the
// LastIndex split assumes they don't.)
func TestAgentIDFromBranchRoundTrips(t *testing.T) {
	for _, tc := range []struct{ root, agent string }{
		{"r1", "a1"},
		{"3f9c61", "3581b6"},
	} {
		if got := agentIDFromBranch(core.BranchFor(tc.root, tc.agent)); got != tc.agent {
			t.Errorf("agentIDFromBranch(BranchFor(%q,%q)) = %q, want %q", tc.root, tc.agent, got, tc.agent)
		}
	}
	// A legacy amux/<root> branch names no agent.
	if got := agentIDFromBranch("amux/f12442"); got != "" {
		t.Errorf("agentIDFromBranch(legacy root branch) = %q, want empty", got)
	}
}

// TestCheckHotkeys drives the terminal Option/Meta hotkey check across the
// detection matrix: iTerm2 (definitive via the stubbed profile lookup, in each
// of its three outcomes), Terminal.app (hint only), and everything else
// (silent). Env vars and the plist lookup are both stubbed, so the test runs
// the same on every platform.
func TestCheckHotkeys(t *testing.T) {
	noLookup := func(string) (int, error) {
		t.Error("optionKeySends called for a non-iTerm2 terminal")
		return 0, nil
	}
	for _, tc := range []struct {
		name    string
		env     map[string]string
		sends   int
		sendErr error
		lookup  func(string) (int, error) // overrides sends/sendErr when set
		symbol  string                    // leading marker of the first line; "" = no output
		want    string                    // substring the output must contain
	}{
		{name: "no terminal env", env: map[string]string{}, lookup: noLookup},
		{name: "other terminal", env: map[string]string{"TERM_PROGRAM": "WezTerm"}, lookup: noLookup},
		{
			name:   "iTerm2 Normal warns with fix",
			env:    map[string]string{"LC_TERMINAL": "iTerm2", "ITERM_PROFILE": "Default"},
			sends:  0,
			symbol: "⚠",
			want:   `"Esc+"`,
		},
		{
			name:   "iTerm2 Esc+ passes",
			env:    map[string]string{"TERM_PROGRAM": "iTerm.app"},
			sends:  2,
			symbol: "✓",
			want:   "hotkeys work",
		},
		{
			name:   "iTerm2 Meta passes",
			env:    map[string]string{"LC_TERMINAL": "iTerm2"},
			sends:  1,
			symbol: "✓",
		},
		{
			name:    "iTerm2 unreadable plist degrades to hint",
			env:     map[string]string{"LC_TERMINAL": "iTerm2"},
			sendErr: os.ErrNotExist,
			symbol:  "·",
			want:    `"Esc+"`,
		},
		{
			name:   "Apple_Terminal hints",
			env:    map[string]string{"TERM_PROGRAM": "Apple_Terminal"},
			lookup: noLookup,
			symbol: "·",
			want:   "Use Option as Meta key",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lookup := tc.lookup
			if lookup == nil {
				lookup = func(string) (int, error) { return tc.sends, tc.sendErr }
			}
			lines := checkHotkeys(func(k string) string { return tc.env[k] }, lookup)
			if tc.symbol == "" {
				if lines != nil {
					t.Fatalf("expected silence, got %q", lines)
				}
				return
			}
			if len(lines) == 0 {
				t.Fatalf("expected output starting with %q, got none", tc.symbol)
			}
			joined := strings.Join(lines, "\n")
			if !strings.Contains(lines[0], tc.symbol) {
				t.Errorf("first line = %q, want marker %q", lines[0], tc.symbol)
			}
			if !strings.Contains(joined, tc.want) {
				t.Errorf("output %q missing %q", joined, tc.want)
			}
		})
	}
}

// TestCheckHotkeysLooksUpActiveProfile pins that the iTerm2 branch asks the
// plist lookup for the profile named by ITERM_PROFILE — the setting is
// per-profile, so reading any other profile's value would be wrong.
func TestCheckHotkeysLooksUpActiveProfile(t *testing.T) {
	env := map[string]string{"LC_TERMINAL": "iTerm2", "ITERM_PROFILE": "Work"}
	var got string
	checkHotkeys(func(k string) string { return env[k] }, func(profile string) (int, error) {
		got = profile
		return 2, nil
	})
	if got != "Work" {
		t.Errorf("optionKeySends profile = %q, want %q", got, "Work")
	}
}

// TestReconcileClean verifies no drift is reported when store and disk agree.
func TestReconcileClean(t *testing.T) {
	sessions := t.TempDir()
	if err := os.MkdirAll(filepath.Join(sessions, "r1", "a1"), 0o755); err != nil {
		t.Fatal(err)
	}
	roots := []core.WorkgroupRow{{ID: "r1", Agents: []core.AgentRow{{ID: "a1", Branch: "amux/r1-a1"}}}}
	disk := []branchRef{{Repo: "acme/api", Branch: "amux/r1-a1"}}

	od, md, ob := reconcile(sessions, roots, disk)
	if len(od)+len(md)+len(ob) != 0 {
		t.Errorf("expected no drift, got dirs=%v missing=%v branches=%v", od, md, ob)
	}
}

// TestReportProviderNotConfigured: provider mode is opt-in, so a machine that
// never registered is healthy — doctor points at the command instead of raising
// a finding.
func TestReportProviderNotConfigured(t *testing.T) {
	sandboxCLI(t)
	out, err := captureOutput(t, func() error { reportProvider(); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "not configured") || !strings.Contains(out, "amux provide install") {
		t.Errorf("Provider section = %q, want it to name the install command", out)
	}
	if strings.Contains(out, "✗") {
		t.Errorf("an unconfigured provider reported a failure:\n%s", out)
	}
}

// TestReportProviderReportsTheChain covers the configured-but-not-installed
// case: the config and credential are reported, and the missing service is the
// next step rather than an error.
func TestReportProviderReportsTheChain(t *testing.T) {
	sandboxCLI(t)
	token := filepath.Join(t.TempDir(), "provider.token")
	if err := os.WriteFile(token, []byte("s3cr3t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := providercfg.Config{
		Orchestrator: "orch.example.com:7443", TokenFile: token, Name: "box",
		PublishSessions: true, RuntimeEvents: true,
	}
	if err := providercfg.Save(cfg); err != nil {
		t.Fatal(err)
	}

	out, err := captureOutput(t, func() error { reportProvider(); return nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"orchestrator orch.example.com:7443",
		"name box",
		"sessions",
		"runtime-events",
		token,
		"mode 0600",
		"amux provide install", // the service is the missing step
		"the provider has not run yet",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Provider section missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "s3cr3t") {
		t.Errorf("doctor printed the bearer token itself:\n%s", out)
	}
}

// TestReportProviderFlagsALooseToken: the service reads the credential
// unattended, so nobody is watching for a stray 0644.
func TestReportProviderFlagsALooseToken(t *testing.T) {
	sandboxCLI(t)
	token := filepath.Join(t.TempDir(), "provider.token")
	if err := os.WriteFile(token, []byte("s3cr3t"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := providercfg.Save(providercfg.Config{Orchestrator: "h:1", TokenFile: token}); err != nil {
		t.Fatal(err)
	}
	out, err := captureOutput(t, func() error { reportProvider(); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "want 0600") || !strings.Contains(out, "chmod 600") {
		t.Errorf("a world-readable token was not flagged:\n%s", out)
	}
}

// TestReportProviderStatusOutlivesItsProcess pins the one lie the status file
// can tell: a "registered" record whose process is gone (SIGKILL, a reboot).
// Doctor must contradict it rather than repeat it.
func TestReportProviderStatusOutlivesItsProcess(t *testing.T) {
	sandboxCLI(t)
	token := filepath.Join(t.TempDir(), "provider.token")
	if err := os.WriteFile(token, []byte("s3cr3t"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := providercfg.Save(providercfg.Config{Orchestrator: "h:1", TokenFile: token}); err != nil {
		t.Fatal(err)
	}
	// A pid that cannot be running: 0 is never a live user process.
	if err := provider.WriteStatus(provider.StatusPath(), provider.Status{
		PID: 0, State: provider.StateRegistered, ProviderID: "prov-1",
		RegisteredAt: time.Now().Add(-time.Hour), HeartbeatAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	out, err := captureOutput(t, func() error { reportProvider(); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "is gone") {
		t.Errorf("doctor repeated a stale `registered` record:\n%s", out)
	}
	if !strings.Contains(out, "prov-1") {
		t.Errorf("status section dropped the providerId:\n%s", out)
	}
}
