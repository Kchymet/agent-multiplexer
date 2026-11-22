package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"amux/internal/agent"
	"amux/internal/core"
	"amux/internal/store"
)

func TestClaudeQuestionOpenIn(t *testing.T) {
	dir := t.TempDir()
	write := func(lines ...string) string {
		p := filepath.Join(dir, "t.jsonl")
		if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	const (
		prompt   = `{"type":"user","message":{"role":"user","content":"pick one"}}`
		ask      = `{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"AskUserQuestion","input":{"questions":[{"question":"Which?","options":[{"label":"A"},{"label":"B"}]}]}}],"stop_reason":"tool_use"}}`
		answered = `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"User answered: A"}]}}`
		bash     = `{"type":"assistant","message":{"id":"m2","role":"assistant","content":[{"type":"tool_use","id":"tu2","name":"Bash","input":{"command":"ls"}}],"stop_reason":"tool_use"}}`
		next     = `{"type":"user","message":{"role":"user","content":"never mind"}}`
	)
	cases := []struct {
		name  string
		lines []string
		want  bool
	}{
		{"question open", []string{prompt, ask}, true},
		{"question answered", []string{prompt, ask, answered}, false},
		{"other tool pending", []string{prompt, bash}, false},
		{"answered then another tool", []string{prompt, ask, answered, bash}, false},
		{"superseded by a new prompt", []string{prompt, ask, next}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := claudeQuestionOpenIn(write(tc.lines...)); got != tc.want {
				t.Fatalf("claudeQuestionOpenIn = %v, want %v", got, tc.want)
			}
		})
	}
	if claudeQuestionOpenIn(filepath.Join(dir, "missing")) {
		t.Fatal("missing transcript reported an open question")
	}
}

// TestSteerDismissesOpenQuestionBeforeTypedReply is the remote-answer path:
// with an AskUserQuestion selector up, a prompt or interject from a client with
// no terminal (the Android app) must close the selector before typing, or its
// keys drive the selector and Enter picks the focused option instead of
// delivering the reply.
func TestSteerDismissesOpenQuestionBeforeTypedReply(t *testing.T) {
	for _, verb := range []string{core.SteerPrompt, core.SteerInterject} {
		t.Run(verb, func(t *testing.T) {
			d, eng := steerDaemon(t)
			putSession(t, "a1", "claude")
			in := eng.running("a1")
			d.questionOpenFn = func(agent.Harness, store.Session) bool { return true }

			if err := d.steer(context.Background(), core.Action{ID: "a1", Fields: map[string]string{
				core.SteerVerb: verb, core.SteerText: "option B, please",
			}}); err != nil {
				t.Fatal(err)
			}
			if got := in.written(); got != "\x1boption B, please\r" {
				t.Fatalf("pane received %q, want Esc then the typed reply", got)
			}
			seq := in.sequences[len(in.sequences)-1]
			if len(seq) < 3 || string(seq[0].Bytes) != "\x1b" || seq[1].DelayBefore != 300*time.Millisecond {
				t.Fatalf("sequence %#v: want Esc, then text held 300ms for the composer", seq)
			}
		})
	}
}

func TestSteerTypesStraightThroughWithoutQuestion(t *testing.T) {
	d, eng := steerDaemon(t)
	putSession(t, "a1", "claude")
	in := eng.running("a1")
	d.questionOpenFn = func(agent.Harness, store.Session) bool { return false }
	if err := d.steer(context.Background(), core.Action{ID: "a1", Fields: map[string]string{
		core.SteerVerb: core.SteerPrompt, core.SteerText: "hi",
	}}); err != nil {
		t.Fatal(err)
	}
	if got := in.written(); got != "hi\r" {
		t.Fatalf("pane received %q, want no dismissal", got)
	}
}

// A runtime with no question selector never gets a dismissal key, whatever the
// detector says.
func TestSteerNoDismissalForRuntimeWithoutSelector(t *testing.T) {
	d, eng := steerDaemon(t)
	putSession(t, "a1", "codex")
	in := eng.running("a1")
	d.questionOpenFn = func(agent.Harness, store.Session) bool { return true }
	if err := d.steer(context.Background(), core.Action{ID: "a1", Fields: map[string]string{
		core.SteerVerb: core.SteerPrompt, core.SteerText: "hi",
	}}); err != nil {
		t.Fatal(err)
	}
	if got := in.written(); got != "hi\r" {
		t.Fatalf("pane received %q, want no dismissal for codex", got)
	}
}

// Esc is only ever pressed on a live turn: an idle or unknown Claude session is
// never treated as sitting on a question, whatever its transcript says.
func TestClaudeQuestionOpenNeedsALiveTurn(t *testing.T) {
	isolateHome(t)
	sess := store.Session{ID: "a1", Agent: "claude", Dir: t.TempDir(), ClaudeID: convID("a1")}
	h := agent.HarnessFor(sess.Agent)
	if claudeQuestionOpen(h, sess) {
		t.Fatal("session with no hook state reported an open question")
	}
	if err := core.WriteSessionHookState("a1", convID("a1"), core.StateReady, ""); err != nil {
		t.Fatal(err)
	}
	if claudeQuestionOpen(h, sess) {
		t.Fatal("finished turn reported an open question")
	}
	codex := store.Session{ID: "c1", Agent: "codex", Dir: t.TempDir()}
	if claudeQuestionOpen(agent.HarnessFor("codex"), codex) {
		t.Fatal("codex session reported a Claude question")
	}
}
