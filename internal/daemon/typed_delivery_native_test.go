package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"amux/internal/agent"
	"amux/internal/composer"
	"amux/internal/core"
	"amux/internal/engine"
	"amux/internal/engine/local"
	"amux/internal/store"
)

// TestNativeClaudeTypedDelivery drives a real Claude Code TUI through the
// production engine and the one typed delivery path: amux must read the real
// composer as empty, see a draft a person typed, refuse to type into it, and
// once the composer is clear again type, verify and submit its own text as a
// user turn. CI runs it against the pinned Claude Code release.
func TestNativeClaudeTypedDelivery(t *testing.T) {
	bin := os.Getenv("AMUX_TEST_CLAUDE_BIN")
	if bin == "" {
		t.Skip("set AMUX_TEST_CLAUDE_BIN to a Claude Code binary")
	}
	cfg, dir := t.TempDir(), t.TempDir()
	state, _ := json.Marshal(map[string]any{
		"hasCompletedOnboarding": true,
		"oauthAccount": map[string]any{
			"accountUuid": "00000000-0000-4000-8000-000000000001", "emailAddress": "fixture@example.invalid",
			"organizationUuid": "00000000-0000-4000-8000-000000000002",
		},
		"projects": map[string]any{dir: map[string]any{"hasTrustDialogAccepted": true, "hasCompletedProjectOnboarding": true}},
	})
	if err := os.WriteFile(filepath.Join(cfg, ".claude.json"), state, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "settings.json"), []byte(`{"theme":"dark"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	const conversation = "21111111-2222-4333-8444-555555555555"
	const marker = "TYPED-DELIVERY-MARKER"

	eng := local.New()
	t.Cleanup(eng.Shutdown)
	env := append(blankEnv("CLAUDE"), "CLAUDE_CONFIG_DIR="+cfg, "TERM=xterm-256color")
	in, err := eng.Ensure(context.Background(), engine.Spec{
		Key: engine.Key{AgentID: "typed"}, Dir: dir, Env: env, Cols: 100, Rows: 30,
		Argv: []string{bin, "--session-id", conversation},
	})
	if err != nil {
		t.Fatalf("spawn %s: %v", bin, err)
	}
	obs, ok := in.(paneObserver)
	if !ok {
		t.Fatal("local engine instance does not expose its pane")
	}
	screen := func() string {
		snap, _ := obs.Screen()
		var b strings.Builder
		for y := 0; y < snap.Rows; y++ {
			b.WriteString(snap.Text(y) + "\n")
		}
		return b.String()
	}
	await := func(what string, ok func(composer.Reading) bool) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for {
			snap, _ := obs.Screen()
			if r := composer.Read(composer.RuntimeClaude, snap); ok(r) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("composer never %s; screen:\n%s", what, screen())
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	await("read as empty", composer.Reading.Empty)

	d := &Daemon{questionOpenFn: func(agent.Harness, store.Session) bool { return false }}
	h := agent.HarnessFor("claude")
	sess := store.Session{ID: "typed", Agent: "claude"}
	deliver := func(text string, automated bool) error {
		payload, err := steerPayload(core.SteerPrompt, h.Keys(), map[string]string{core.SteerText: text})
		if err != nil {
			t.Fatal(err)
		}
		return d.deliverTyped(context.Background(), typedRequest{
			harness: h, session: sess, instance: in, keys: h.Keys(), payload: payload, text: text, automated: automated,
		})
	}

	// A person types a draft (through Input, as an attached client does).
	const draft = "half a thought about polling"
	in.Input([]byte(draft))
	await("showed the draft", func(r composer.Reading) bool { return r.Found && composer.Same(r.Text, draft) })
	var busy *composerBusyError
	if err := deliver(marker+" into a draft", false); !errors.As(err, &busy) || busy.typed {
		t.Fatalf("delivery into a draft = %v, want refused untyped; screen:\n%s", err, screen())
	}
	if err := deliver(marker+" automatic", true); !errors.As(err, &busy) || busy.typed {
		t.Fatalf("automatic delivery while a person types = %v, want refused untyped", err)
	}
	await("kept the draft untouched", func(r composer.Reading) bool { return r.Found && composer.Same(r.Text, draft) })

	// The person deletes the draft; amux's own text then lands and is submitted.
	in.InputSequence([]engine.InputStep{{Bytes: []byte(strings.Repeat("\x7f", len(draft)))}})
	await("emptied", composer.Reading.Empty)
	if err := deliver(marker+" status\nsecond line", false); err != nil {
		t.Fatalf("delivery into an empty composer: %v; screen:\n%s", err, screen())
	}
	transcript := filepath.Join(cfg, "projects", strings.NewReplacer("/", "-", ".", "-").Replace(dir), conversation+".jsonl")
	awaitTranscript := func(what, needle string) string {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if b, err := os.ReadFile(transcript); err == nil && strings.Contains(string(b), needle) {
				return string(b)
			}
			time.Sleep(250 * time.Millisecond)
		}
		t.Fatalf("%s never became a user turn in %s; screen:\n%s", what, transcript, screen())
		return ""
	}
	if b := awaitTranscript("delivered text", marker+" status"); strings.Contains(b, draft) {
		t.Fatalf("the draft reached the transcript: %s", b)
	}

	// A long multi-line steer, the coordinator's usual shape: typed (never
	// pasted), it must reach the model verbatim as the user's own words.
	// Claude's paste detection collapsed such a steer into "[Pasted text #N +M
	// lines]" when it was typed in fast bursts; paced typing must not trigger it.
	await("emptied after the first delivery", composer.Reading.Empty)
	var long strings.Builder
	long.WriteString(marker + " long steer")
	for i := 1; i <= 12; i++ {
		fmt.Fprintf(&long, "\n- step %02d: check the failing job, read its log, find the root cause, fix it, push", i)
	}
	var collapsedSeen atomic.Bool
	stop, watched := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(watched)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if snap, ok := obs.Screen(); ok && composer.HasCollapsed(composer.Read(composer.RuntimeClaude, snap).Text) {
				collapsedSeen.Store(true)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	err = deliver(long.String(), false)
	close(stop)
	<-watched
	if err != nil {
		t.Fatalf("long multi-line delivery: %v; screen:\n%s", err, screen())
	}
	if collapsedSeen.Load() {
		t.Fatalf("Claude collapsed amux's paced typed steer into a paste placeholder")
	}
	b := awaitTranscript("the long steer", marker+" long steer")
	if strings.Contains(b, "pasted_content") || strings.Contains(b, "[Pasted text") {
		t.Fatalf("the long steer reached the model as pasted content: %s", b)
	}
	if !strings.Contains(b, "step 12: check the failing job, read its log, find the root cause, fix it, push") {
		t.Fatalf("the long steer's lines did not arrive verbatim: %s", b)
	}
}
