package daemon

import (
	"errors"
	"os"
	"testing"
)

// TestClaudeUsageLive reads the real usage report once with the token in
// AMUX_CLAUDE_USAGE_TOKEN. It logs only the outcome, never the token.
func TestClaudeUsageLive(t *testing.T) {
	token := os.Getenv("AMUX_CLAUDE_USAGE_TOKEN")
	if os.Getenv("AMUX_CLAUDE_USAGE_LIVE") != "1" || token == "" {
		t.Skip("set AMUX_CLAUDE_USAGE_LIVE=1 and AMUX_CLAUDE_USAGE_TOKEN to read the live usage report")
	}
	report, err := fetchClaudeUsage(t.Context(), token)
	var ue *claudeUsageError
	if errors.As(err, &ue) {
		t.Fatalf("refused: %s (pause %s)", ue.code, ue.pause)
	}
	if err != nil {
		t.Fatalf("transport: %T", err)
	}
	t.Logf("windows: %+v", report.Windows)
	if report.Resets != nil {
		t.Logf("resets available: %d", *report.Resets)
	} else {
		t.Log("resets: not reported")
	}
}

// TestClaudeProbeLive sends one quota probe (about 9 tokens) with the token
// in AMUX_CLAUDE_USAGE_TOKEN and logs the windows it reports.
func TestClaudeProbeLive(t *testing.T) {
	token := os.Getenv("AMUX_CLAUDE_USAGE_TOKEN")
	if os.Getenv("AMUX_CLAUDE_PROBE_LIVE") != "1" || token == "" {
		t.Skip("set AMUX_CLAUDE_PROBE_LIVE=1 and AMUX_CLAUDE_USAGE_TOKEN to send one live quota probe")
	}
	windows, err := fetchClaudeProbe(t.Context(), token)
	var ue *claudeUsageError
	if errors.As(err, &ue) {
		t.Fatalf("refused: %s (pause %s)", ue.code, ue.pause)
	}
	if err != nil {
		t.Fatalf("transport: %T", err)
	}
	t.Logf("windows: %+v", windows)
}
