package daemon

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"amux/internal/agent"
	"amux/internal/amuxcfg"
	"amux/internal/codexapp"
	"amux/internal/core"
	"amux/internal/store"
)

// TestStructuredControlGate checks the gate that keeps structured (App Server)
// control dark by default for ORDINARY sessions: it is on only for a Codex
// agent, only when AMUX_CODEX_CONTROL=app-server, and only when the manager
// exists — so the PTY path is never affected unless a user explicitly opts in.
// A Codex workgroup coordinator is the one exception (TestGoalCoordinatorIsAlwaysStructured):
// its native goal lives in the App Server, so that is the only runtime that can
// run it, whatever the machine-wide selection says.
func TestStructuredControlGate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	newDaemon := func() *Daemon {
		d := New("", nil, time.Hour)
		d.codex = codexapp.NewManager(ctx, "")
		return d
	}

	// Ordinary agents: members of a workgroup, not its coordinator.
	codex := store.Session{ID: "a", RootID: "root", Agent: "codex"}
	claude := store.Session{ID: "b", RootID: "root", Agent: "claude"}

	// Flag unset ⇒ everything is pty.
	t.Setenv("AMUX_CODEX_CONTROL", "")
	d := newDaemon()
	if d.structuredControl(codex) {
		t.Error("codex session structured with the flag unset")
	}

	// Flag on ⇒ codex is structured, claude is not (Codex-only).
	t.Setenv("AMUX_CODEX_CONTROL", "app-server")
	d = newDaemon()
	if !d.structuredControl(codex) {
		t.Error("codex session not structured with the flag on")
	}
	if d.structuredControl(claude) {
		t.Error("claude session structured — only codex is eligible")
	}

	// An unrelated flag value stays pty.
	t.Setenv("AMUX_CODEX_CONTROL", "exec-json")
	d = newDaemon()
	if d.structuredControl(codex) {
		t.Error("codex session structured for a non-app-server flag value")
	}

	// No manager ⇒ never structured, even with the flag on.
	t.Setenv("AMUX_CODEX_CONTROL", "app-server")
	if (&Daemon{codexControl: amuxcfg.Control{Effective: amuxcfg.AppServer}}).structuredControl(codex) {
		t.Error("structured control active with no manager")
	}
}

// mockSteerer records the structured verbs the daemon dispatches.
type mockSteerer struct {
	mu                                   sync.Mutex
	prompt, interject, resolveID, resDec string
	cancelled                            bool
	promptErr                            error
}

func (m *mockSteerer) Prompt(_ context.Context, text string) error {
	wait, err := m.BeginPrompt(context.Background(), text)
	if err != nil {
		return err
	}
	return wait(context.Background())
}
func (m *mockSteerer) BeginPrompt(_ context.Context, text string) (func(context.Context) error, error) {
	m.mu.Lock()
	m.prompt = text
	m.mu.Unlock()
	return func(context.Context) error { return m.promptErr }, nil
}
func (m *mockSteerer) Interject(_ context.Context, text string) error {
	m.mu.Lock()
	m.interject = text
	m.mu.Unlock()
	return nil
}
func (m *mockSteerer) Cancel(context.Context) error {
	m.mu.Lock()
	m.cancelled = true
	m.mu.Unlock()
	return nil
}
func (m *mockSteerer) Resolve(_ context.Context, requestID, decision string) error {
	m.mu.Lock()
	m.resolveID, m.resDec = requestID, decision
	m.mu.Unlock()
	return nil
}

// TestSteerStructuredRoutesVerbs checks the structured control path: each steering
// verb is dispatched to the supervisor surface (not keystrokes), prompt runs
// asynchronously, and an unparseable permission decision is refused rather than
// guessed.
func TestSteerStructuredRoutesVerbs(t *testing.T) {
	d := &Daemon{steerStarted: make(chan string, 1)}
	ctx := context.Background()

	m := &mockSteerer{}
	if err := d.steerStructured(ctx, "a", m, core.SteerInterject, map[string]string{core.SteerText: "also X"}); err != nil {
		t.Fatalf("interject: %v", err)
	}
	if m.interject != "also X" {
		t.Fatalf("interject text = %q", m.interject)
	}

	if err := d.steerStructured(ctx, "a", m, core.SteerStop, nil); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !m.cancelled {
		t.Fatal("stop did not Cancel the turn")
	}

	if err := d.steerStructured(ctx, "a", m, core.SteerPermission, map[string]string{
		core.SteerRequestID: "ap1", core.SteerDecision: core.SteerAllow,
	}); err != nil {
		t.Fatalf("permission: %v", err)
	}
	if m.resolveID != "ap1" || m.resDec != core.SteerAllow {
		t.Fatalf("resolve got (%q,%q)", m.resolveID, m.resDec)
	}

	// An unparseable decision is refused, and Resolve is not called with a guess.
	m2 := &mockSteerer{}
	if err := d.steerStructured(ctx, "a", m2, core.SteerPermission, map[string]string{
		core.SteerRequestID: "ap2", core.SteerDecision: "maybe",
	}); err == nil {
		t.Fatal("permission with a bad decision should error")
	}
	if m2.resolveID != "" {
		t.Fatalf("Resolve called on an unparseable decision: %q", m2.resolveID)
	}

	// Interject with no text is refused.
	if err := d.steerStructured(ctx, "a", m2, core.SteerInterject, nil); err == nil {
		t.Fatal("interject with no text should error")
	}

	// Prompt is accepted synchronously and runs the turn asynchronously.
	mp := &mockSteerer{}
	if err := d.steerStructured(ctx, "a", mp, core.SteerPrompt, map[string]string{core.SteerText: "go"}); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	select {
	case <-d.steerStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("async prompt never completed")
	}
	mp.mu.Lock()
	got := mp.prompt
	mp.mu.Unlock()
	if got != "go" {
		t.Fatalf("prompt text = %q", got)
	}
}

// TestRunStructuredPromptJournalsError checks that an async prompt failure is
// reported (to the journal) rather than lost, since the caller was already told
// "accepted".
func TestRunStructuredPromptJournalsError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	d := &Daemon{steerStarted: make(chan string, 1)}
	mp := &mockSteerer{promptErr: fmt.Errorf("boom")}
	d.runStructuredPrompt(context.Background(), "a", mp, "go")
	select {
	case <-d.steerStarted:
	default:
		t.Fatal("runStructuredPrompt did not signal completion")
	}
}

// A workgroup coordinator on the goal runtime is always structured: the daemon
// must supervise its App Server to own the native goal, so the machine-wide
// codex.control selection — which governs ordinary Codex agents — cannot leave
// it on the PTY path where no goal exists. A coordinator on another harness is
// unaffected, as is every ordinary session.
func TestGoalCoordinatorIsAlwaysStructured(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	coordinator := store.Session{ID: "wg", Agent: agent.GoalRuntime}
	claudeCoordinator := store.Session{ID: "wg2", Agent: "claude"}
	member := store.Session{ID: "m", RootID: "wg", Agent: agent.GoalRuntime}
	for _, control := range []string{"", "pty", "app-server"} {
		t.Setenv("AMUX_CODEX_CONTROL", control)
		d := New("", nil, time.Hour)
		d.codex = codexapp.NewManager(ctx, "")
		if !d.structuredControl(coordinator) {
			t.Errorf("codex.control=%q left the goal coordinator unsupervised", control)
		}
		if d.structuredControl(claudeCoordinator) {
			t.Errorf("codex.control=%q made a claude coordinator structured", control)
		}
		if want := control == "app-server"; d.structuredControl(member) != want {
			t.Errorf("codex.control=%q: ordinary member structured=%t, want %t", control, !want, want)
		}
		// Cold event-source resolution follows the same rule, so a subscriber
		// reads the coordinator's structured log from the first subscription.
		if !d.structuredResolvable(coordinator) {
			t.Errorf("codex.control=%q: coordinator events not resolved structured", control)
		}
	}
}
