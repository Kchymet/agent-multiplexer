package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"amux/internal/agent"
	"amux/internal/provider"
	"amux/internal/providercfg"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// noHermesHost makes this process look like a machine with Claude and Codex
// installed and Hermes absent: the CLIs are found only by PATH lookup (no
// AMUX_*_BIN override, which would skip the lookup a real host goes through),
// and the login-shell fallback finds nothing either.
func noHermesHost(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	for name, version := range map[string]string{"claude": "2.1.0 (Claude Code)", "codex": "codex-cli 0.9.0"} {
		script := fmt.Sprintf("#!/bin/sh\necho '%s'\n", version)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	t.Setenv("HOME", dir)
	t.Setenv("SHELL", filepath.Join(dir, "no-shell"))
	for _, kind := range agent.Kinds() {
		t.Setenv("AMUX_"+strings.ToUpper(kind)+"_BIN", "")
	}
}

type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) logf(format string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, a...))
}

func (l *logSink) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// TestProviderRegistersWithoutHermesInstalled is the regression for "machines
// without hermes can't register": a Claude+Codex machine runs the real
// discovery, sends a register frame advertising exactly those two harnesses,
// completes the handshake, and never reports the absent optional Hermes as a
// failure — not on the first dial and not on a reconnect.
func TestProviderRegistersWithoutHermesInstalled(t *testing.T) {
	noHermesHost(t)
	logs := &logSink{}
	conns := make(chan net.Conn, 2)
	registered := make(chan provider.Status, 4)
	p := provider.New(provider.Config{
		Orchestrator:      "pipe",
		Token:             "tok",
		Name:              "no-hermes",
		DiscoverExecution: executionDiscovery(providercfg.Config{}, logs.logf),
		Logf:              logs.logf,
		Dial: func(ctx context.Context) (net.Conn, error) {
			a, b := net.Pipe()
			select {
			case conns <- b:
				return a, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
		OnStatus: func(st provider.Status) {
			if st.State == provider.StateRegistered {
				select {
				case registered <- st:
				default:
				}
			}
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = p.Run(ctx) }()

	want := []harnessproto.HarnessCapability{
		{Name: "claude", Version: "2.1.0 (Claude Code)"},
		{Name: "codex", Version: "codex-cli 0.9.0"},
	}
	for attempt := 1; attempt <= 2; attempt++ {
		var raw net.Conn
		select {
		case raw = <-conns:
		case <-time.After(10 * time.Second):
			t.Fatalf("attempt %d: provider never dialed", attempt)
		}
		oc := harnessproto.NewConn(raw)
		reg, err := oc.ReadHarness()
		if err != nil {
			t.Fatalf("attempt %d: read register: %v", attempt, err)
		}
		if reg.Type != harnessproto.HRegister || reg.Capabilities == nil || reg.Capabilities.Execution == nil {
			t.Fatalf("attempt %d: register frame without execution block: %+v", attempt, reg)
		}
		if got := reg.Capabilities.Execution.Harnesses; !reflect.DeepEqual(got, want) {
			t.Fatalf("attempt %d: advertised harnesses = %+v, want %+v", attempt, got, want)
		}
		if !reg.Capabilities.Execution.Supports("claude", harnessproto.IdentityMachine) ||
			reg.Capabilities.Execution.Supports("hermes", harnessproto.IdentityMachine) {
			t.Fatalf("attempt %d: execution block %+v", attempt, reg.Capabilities.Execution)
		}
		if err := oc.WriteMux(harnessproto.MuxMsg{
			Type: harnessproto.MRegistered, OK: true, Version: harnessproto.Version2,
			ProviderID: "prov-no-hermes", HeartbeatSeconds: 30, GraceSeconds: 60,
		}); err != nil {
			t.Fatalf("attempt %d: send registered: %v", attempt, err)
		}
		select {
		case st := <-registered:
			if st.ProviderID != "prov-no-hermes" {
				t.Fatalf("attempt %d: registered status = %+v", attempt, st)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("attempt %d: provider never reached registered; log:\n%s", attempt, logs.all())
		}
		_ = raw.Close() // force a reconnect: discovery reruns before the next dial
	}

	out := logs.all()
	if strings.Contains(out, "unavailable") || strings.Contains(out, "version check failed") {
		t.Fatalf("an absent optional harness was reported as a failure:\n%s", out)
	}
	if n := strings.Count(out, "harnesses: advertising"); n != 1 {
		t.Fatalf("discovery summary logged %d times across reconnects, want once:\n%s", n, out)
	}
	if !strings.Contains(out, "not installed (optional, skipped): hermes") {
		t.Fatalf("summary does not name hermes as skipped:\n%s", out)
	}
}

// TestDiscoverySummaryFlagsRealProblems keeps the quieter report honest: an
// installed CLI that fails its version check, and a harness the operator
// explicitly allowlisted but is not installed, are still reported as
// unavailable rather than folded into "optional, skipped".
func TestDiscoverySummaryFlagsRealProblems(t *testing.T) {
	notInstalled := fmt.Errorf("hermes: %w", agent.ErrNotInstalled)
	broken := fmt.Errorf("version check failed: exit status 1")
	results := []agent.ProbeResult{
		{Kind: "claude", Version: "2.1.0"},
		{Kind: "codex", Err: broken},
		{Kind: "hermes", Err: notInstalled},
	}

	caps, report := summarizeDiscovery(results, false)
	if len(caps.Harnesses) != 1 || caps.Harnesses[0].Name != "claude" {
		t.Fatalf("auto discovery advertised %+v, want claude only", caps.Harnesses)
	}
	if !strings.Contains(report, "not installed (optional, skipped): hermes") || !strings.Contains(report, "unavailable: codex: version check failed") {
		t.Fatalf("auto discovery report = %q", report)
	}

	_, report = summarizeDiscovery([]agent.ProbeResult{{Kind: "hermes", Err: notInstalled}}, true)
	if !strings.Contains(report, "none verified") || !strings.Contains(report, "unavailable: hermes") || strings.Contains(report, "optional") {
		t.Fatalf("allowlisted-but-absent report = %q", report)
	}
}
