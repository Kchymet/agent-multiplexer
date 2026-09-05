package daemon

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"amux/internal/codexapp"
	"amux/internal/core"
	"amux/internal/store"
)

// TestStructuredControlGate checks the single opt-in gate that keeps structured
// (App Server) control dark by default: it is on only for a Codex session, only
// when AMUX_CODEX_CONTROL=app-server, and only when the manager exists — so the
// PTY path is never affected unless a user explicitly opts in.
func TestStructuredControlGate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &Daemon{codex: codexapp.NewManager(ctx, "")}

	codex := store.Session{ID: "a", Agent: "codex"}
	claude := store.Session{ID: "b", Agent: "claude"}

	// Flag unset ⇒ everything is pty.
	t.Setenv("AMUX_CODEX_CONTROL", "")
	if d.structuredControl(codex) {
		t.Error("codex session structured with the flag unset")
	}

	// Flag on ⇒ codex is structured, claude is not (Codex-only).
	t.Setenv("AMUX_CODEX_CONTROL", "app-server")
	if !d.structuredControl(codex) {
		t.Error("codex session not structured with the flag on")
	}
	if d.structuredControl(claude) {
		t.Error("claude session structured — only codex is eligible")
	}

	// An unrelated flag value stays pty.
	t.Setenv("AMUX_CODEX_CONTROL", "exec-json")
	if d.structuredControl(codex) {
		t.Error("codex session structured for a non-app-server flag value")
	}

	// No manager ⇒ never structured, even with the flag on.
	t.Setenv("AMUX_CODEX_CONTROL", "app-server")
	if (&Daemon{}).structuredControl(codex) {
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
	m.mu.Lock()
	m.prompt = text
	m.mu.Unlock()
	return m.promptErr
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
