package daemon

import (
	"context"

	"amux/internal/panespec"
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

func historyHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	return home
}

func marker(t *testing.T, id string) historyMarker {
	t.Helper()
	var m historyMarker
	b, err := os.ReadFile(historyMarkerPath(id))
	if err != nil || json.Unmarshal(b, &m) != nil {
		t.Fatalf("history marker for %s: %v", id, err)
	}
	return m
}

// A session migrated from PTY Codex keeps its earlier history readable through
// the structured record now canonical for every Codex session: the rollout is
// imported once, in order, as the log's prefix; a log holding only amux's
// notices about this launch keeps them after the prefix; reruns change
// nothing; the rollout itself is never modified.
func TestCodexPTYHistoryIsImportedOnceIntoStructuredLog(t *testing.T) {
	home := historyHome(t)
	const pty, noticed = "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"
	rollout := legacyRollout(t, home, pty, "PTY-PROMPT", "PTY-ANSWER")
	before, _ := os.ReadFile(rollout)
	legacyRollout(t, home, noticed, "NOTICED-PROMPT", "NOTICED-ANSWER")
	sessions := []store.Session{
		{ID: "migrated", Agent: "codex", ClaudeID: pty, Dir: t.TempDir()},
		{ID: "archived", Agent: "codex", ClaudeID: pty, Dir: t.TempDir(), Archived: true},
		{ID: "noticed", Agent: "codex", ClaudeID: noticed, Dir: t.TempDir()},
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
	// A cold-start notice was appended before the migration ran: the log
	// exists but holds no history.
	if err := codexapp.AppendNotice("noticed", "info", "starting this launch"); err != nil {
		t.Fatal(err)
	}

	d := New("", nil, 0)
	d.importCodexHistories()
	for _, id := range []string{"migrated", "archived"} {
		got := structuredLog(t, id)
		if len(got) != 3 || got[0].Type != harnessproto.TypePrompt || !strings.Contains(string(got[0].Payload), "PTY-PROMPT") ||
			!strings.Contains(string(got[1].Payload), "PTY-ANSWER") || got[2].Type != harnessproto.TypeNotice || !strings.Contains(string(got[2].Payload), "imported 2 lines") {
			t.Fatalf("%s log = %+v", id, got)
		}
		if m := marker(t, id); m.Outcome != "imported" || m.Lines != 2 {
			t.Fatalf("%s marker = %+v", id, m)
		}
	}
	if got := structuredLog(t, "noticed"); len(got) != 4 || !strings.Contains(string(got[0].Payload), "NOTICED-PROMPT") || !strings.Contains(string(got[3].Payload), "starting this launch") {
		t.Fatalf("notice-only log not migrated in order: %+v", got)
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

// A session that already ran structured keeps its canonical log untouched:
// its rollout also holds the log's own turns, which would be duplicated.
func TestCodexHistoryStructuredSessionKeepsLog(t *testing.T) {
	home := historyHome(t)
	const thread = "44444444-4444-4444-8444-444444444444"
	legacyRollout(t, home, thread, "ROLLOUT-PROMPT", "ROLLOUT")
	if err := codexapp.SaveIdentity(codexapp.Identity{SessionID: "s", ThreadID: thread, Resumable: true}); err != nil {
		t.Fatal(err)
	}
	if err := codexapp.AppendNotice("s", "info", "structured since creation"); err != nil {
		t.Fatal(err)
	}
	existing, _ := os.ReadFile(codexapp.EventLogPathFor("s"))
	if ok, err := importCodexHistory(store.Session{ID: "s", Agent: "codex", ClaudeID: thread, Dir: t.TempDir()}); ok || err != nil {
		t.Fatalf("import = %v, %v", ok, err)
	}
	if b, _ := os.ReadFile(codexapp.EventLogPathFor("s")); !bytes.Equal(b, existing) {
		t.Fatal("structured log rewritten")
	}
	if m := marker(t, "s"); m.Outcome != "structured" {
		t.Fatalf("marker = %+v", m)
	}
}

// A failed import refuses that session's launch through the real supervisor
// launch path, leaves the migration pending (no marker, no log), and the next
// launch after repair imports the history before anything else is logged.
func TestCodexHistoryImportFailureBlocksLaunchAndRetries(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores the unreadable-rollout fixture")
	}
	home := historyHome(t)
	const thread = "66666666-6666-4666-8666-666666666666"
	rollout := legacyRollout(t, home, thread, "RETRY-PROMPT", "RETRY")
	s := store.Session{ID: "retry", Agent: "codex", ClaudeID: thread, Dir: t.TempDir()}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	d := New("", nil, 0)
	d.codex = codexapp.NewManager(ctx, "")
	defer d.codex.Shutdown()
	if err := os.Chmod(rollout, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ensureSupervisorSpec(ctx, panespec.LaunchSpec{Session: s}); err == nil || !strings.Contains(err.Error(), "earlier history") {
		t.Fatalf("launch with an unreadable rollout = %v", err)
	}
	if _, err := os.Stat(historyMarkerPath(s.ID)); !os.IsNotExist(err) {
		t.Fatal("failed import recorded an outcome")
	}
	if _, err := os.Stat(codexapp.EventLogPathFor(s.ID)); !os.IsNotExist(err) {
		t.Fatal("failed import left a log")
	}
	if err := os.Chmod(rollout, 0o600); err != nil {
		t.Fatal(err)
	}
	// The repaired launch imports first; whatever later launch step fails in
	// this unsandboxed test cannot undo that.
	_, _ = d.ensureSupervisorSpec(ctx, panespec.LaunchSpec{Session: s})
	if got := structuredLog(t, s.ID); len(got) < 3 || !strings.Contains(string(got[0].Payload), "RETRY-PROMPT") {
		t.Fatalf("retry did not import history first: %+v", got)
	}
	if m := marker(t, s.ID); m.Outcome != "imported" {
		t.Fatalf("marker = %+v", m)
	}
}

// An oversized rollout line is replaced by an omission notice instead of
// failing the migration forever.
func TestCodexHistoryImportBoundsLines(t *testing.T) {
	home := historyHome(t)
	const thread = "77777777-7777-4777-8777-777777777777"
	rollout := legacyRollout(t, home, thread, "SMALL-PROMPT", "SMALL")
	f, err := os.OpenFile(rollout, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"type":"response_item","payload":{"text":"` + strings.Repeat("x", 200<<10) + `"}}` + "\n")
	_ = f.Close()
	old := maxImportLine
	maxImportLine = 100 << 10
	defer func() { maxImportLine = old }()
	if ok, err := importCodexHistory(store.Session{ID: "big", Agent: "codex", ClaudeID: thread, Dir: t.TempDir()}); !ok || err != nil {
		t.Fatalf("import = %v, %v", ok, err)
	}
	got := structuredLog(t, "big")
	if len(got) != 4 || !strings.Contains(string(got[2].Payload), "omitted rollout line 3") || !strings.Contains(string(got[3].Payload), "imported 3 lines") {
		t.Fatalf("bounded import = %+v", got)
	}
}
