package agent

import (
	"os"
	"path/filepath"
	"testing"

	"amux/internal/codexcfg"
	"amux/internal/store"
)

func TestLatestRuntimeModels(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name   string
		lines  string
		decode func([]byte) string
		want   string
	}{
		{
			name: "claude latest assistant model",
			lines: `{"type":"assistant","message":{"model":"claude-sonnet-4-6"}}` + "\n" +
				`{"type":"user","message":{"content":"next"}}` + "\n" +
				`{"type":"assistant","message":{"model":"claude-opus-4-7"}}` + "\n",
			decode: claudeModelLine, want: "claude-opus-4-7",
		},
		{
			name: "codex latest turn context",
			lines: `{"type":"turn_context","payload":{"model":"gpt-5.5"}}` + "\n" +
				`{"type":"response_item","payload":{"type":"message"}}` + "\n" +
				`{"type":"turn_context","payload":{"model":"gpt-5.6-codex"}}` + "\n",
			decode: codexModelLine, want: "gpt-5.6-codex",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name+".jsonl")
			if err := os.WriteFile(path, []byte(tc.lines), 0o644); err != nil {
				t.Fatal(err)
			}
			if got, ok := latestModelLine(path, tc.decode); !ok || got != tc.want {
				t.Fatalf("latestModelLine = %q, %v; want %q, true", got, ok, tc.want)
			}
		})
	}
}

func TestCodexCurrentModelReadsRollout(t *testing.T) {
	dir := t.TempDir()
	const id = "11111111-1111-4111-8111-111111111111"
	s := store.Session{ID: "agent", Agent: "codex", Dir: dir, ClaudeID: id, Model: "gpt-5.5"}
	home := codexcfg.At(codexcfg.AgentHome(dir))
	path := home.NewRolloutPath(id)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	lines := `{"type":"turn_context","payload":{"model":"gpt-5.5"}}` + "\n" +
		`{"type":"turn_context","payload":{"model":"gpt-5.6-codex"}}` + "\n"
	if err := os.WriteFile(path, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, ok := (codexHarness{}).CurrentModel(s); !ok || got != "gpt-5.6-codex" {
		t.Fatalf("CurrentModel = %q, %v; want latest turn_context model", got, ok)
	}
}
