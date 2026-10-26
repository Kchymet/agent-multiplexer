package daemon

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"amux/internal/codexapp"
	"amux/internal/store"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// legacyRollout writes a synthetic PTY-era rollout for thread id into the
// user's Codex home and returns its path.
func legacyRollout(t *testing.T, home, id, prompt, answer string) string {
	t.Helper()
	dir := filepath.Join(home, "codex", "sessions", "2026", "07", "02")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-07-02T10-00-00-"+id+".jsonl")
	lines := []string{
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"` + prompt + `"}],"internal_chat_message_metadata_passthrough":{"content_item_kinds":["user.text"]}}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + answer + `"}]}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func structuredLog(t *testing.T, id string) []harnessproto.RuntimeEvent {
	t.Helper()
	b, err := os.ReadFile(codexapp.EventLogPathFor(id))
	if err != nil {
		t.Fatal(err)
	}
	var out []harnessproto.RuntimeEvent
	for _, line := range bytes.Split(bytes.TrimSpace(b), []byte("\n")) {
		var ev harnessproto.RuntimeEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		out = append(out, ev)
	}
	return out
}

// A session migrated from PTY Codex keeps its earlier history readable through
// the structured record now canonical for every Codex session: the rollout is
// imported once, in order, as the log's prefix; reruns and already structured
// sessions are untouched; the rollout itself is never modified.
func TestCodexPTYHistoryIsImportedOnceIntoStructuredLog(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	const pty, structured = "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"
	rollout := legacyRollout(t, home, pty, "PTY-PROMPT", "PTY-ANSWER")
	before, _ := os.ReadFile(rollout)
	legacyRollout(t, home, structured, "OTHER-PROMPT", "OTHER-ANSWER")
	sessions := []store.Session{
		{ID: "migrated", Agent: "codex", ClaudeID: pty, Dir: t.TempDir()},
		{ID: "archived", Agent: "codex", ClaudeID: pty, Dir: t.TempDir(), Archived: true},
		{ID: "already", Agent: "codex", ClaudeID: structured, Dir: t.TempDir()},
		{ID: "never-run", Agent: "codex", Dir: t.TempDir()},
	}
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sessions {
		if err := db.PutSession(s); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	// "already" is structured: its log exists and is canonical.
	if err := codexapp.AppendNotice("already", "info", "structured since creation"); err != nil {
		t.Fatal(err)
	}
	existing, _ := os.ReadFile(codexapp.EventLogPathFor("already"))

	d := New("", nil, 0)
	d.importCodexHistories()
	for _, id := range []string{"migrated", "archived"} {
		got := structuredLog(t, id)
		if len(got) != 3 || got[0].Type != harnessproto.TypePrompt || !strings.Contains(string(got[0].Payload), "PTY-PROMPT") ||
			!strings.Contains(string(got[1].Payload), "PTY-ANSWER") || got[2].Type != harnessproto.TypeNotice || !strings.Contains(string(got[2].Payload), "imported 2 lines") {
			t.Fatalf("%s log = %+v", id, got)
		}
	}
	if b, _ := os.ReadFile(codexapp.EventLogPathFor("already")); !bytes.Equal(b, existing) {
		t.Fatal("an existing structured log was rewritten")
	}
	if _, err := os.Stat(codexapp.EventLogPathFor("never-run")); !os.IsNotExist(err) {
		t.Fatalf("never-run session got a log: %v", err)
	}
	if b, _ := os.ReadFile(rollout); !bytes.Equal(b, before) {
		t.Fatal("rollout modified")
	}
	// Idempotent, and later structured appends follow the imported prefix.
	first, _ := os.ReadFile(codexapp.EventLogPathFor("migrated"))
	if err := codexapp.AppendNotice("migrated", "info", "LATER"); err != nil {
		t.Fatal(err)
	}
	d.importCodexHistories()
	if imported, err := importCodexHistory(sessions[0]); imported || err != nil {
		t.Fatalf("second import = %v, %v", imported, err)
	}
	if got := structuredLog(t, "migrated"); len(got) != 4 || !strings.Contains(string(got[3].Payload), "LATER") {
		t.Fatalf("history duplicated or reordered: %+v", got)
	}
	if b, _ := os.ReadFile(codexapp.EventLogPathFor("migrated")); !bytes.HasPrefix(b, first) {
		t.Fatal("imported prefix changed")
	}
}

// The thread the launch resumes (a persisted identity) is the one imported.
func TestCodexHistoryImportFollowsIdentityThread(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	const pinned, resumed = "44444444-4444-4444-8444-444444444444", "55555555-5555-4555-8555-555555555555"
	legacyRollout(t, home, pinned, "STALE-PROMPT", "STALE")
	legacyRollout(t, home, resumed, "RESUMED-PROMPT", "RESUMED")
	if err := codexapp.SaveIdentity(codexapp.Identity{SessionID: "s", ThreadID: resumed, Resumable: true}); err != nil {
		t.Fatal(err)
	}
	if ok, err := importCodexHistory(store.Session{ID: "s", Agent: "codex", ClaudeID: pinned, Dir: t.TempDir()}); !ok || err != nil {
		t.Fatalf("import = %v, %v", ok, err)
	}
	if got := structuredLog(t, "s"); !strings.Contains(string(got[0].Payload), "RESUMED-PROMPT") {
		t.Fatalf("imported the wrong thread: %+v", got)
	}
}
