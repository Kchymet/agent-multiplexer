package source

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"amux/internal/core"
	"amux/internal/proctree"
	"amux/internal/tmuxctl"
)

// Claude surfaces Claude Code sessions via `claude agents --json`, which lists
// both local interactive sessions and background/cloud agents.
type Claude struct {
	// Bin is the claude executable; defaults to "claude" (overridable via
	// $AMUX_CLAUDE_BIN) so tests can stub it.
	Bin string
}

// NewClaude builds a Claude adapter, honoring $AMUX_CLAUDE_BIN.
func NewClaude() *Claude {
	bin := os.Getenv("AMUX_CLAUDE_BIN")
	if bin == "" {
		bin = "claude"
	}
	return &Claude{Bin: bin}
}

func (c *Claude) Name() string { return "claude" }

// claudeAgent mirrors the JSON records emitted by `claude agents --json`.
type claudeAgent struct {
	PID       int    `json:"pid"`
	Cwd       string `json:"cwd"`
	Kind      string `json:"kind"` // "interactive" for local sessions
	StartedAt int64  `json:"startedAt"`
	SessionID string `json:"sessionId"`
	Status    string `json:"status"`
}

func (c *Claude) Poll(ctx context.Context) ([]core.Session, error) {
	raw, err := runClaudeAgents(ctx, c.Bin)
	if err != nil {
		// claude not installed / not logged in: report nothing rather than erroring.
		return nil, nil
	}
	var agents []claudeAgent
	if err := json.Unmarshal(raw, &agents); err != nil {
		return nil, nil
	}

	paneWindows := tmuxctl.PaneWindows(ctx)

	sessions := make([]core.Session, 0, len(agents))
	for _, a := range agents {
		local := a.Kind == "" || a.Kind == "interactive"
		s := core.Session{
			ID:        a.SessionID,
			Title:     claudeTitle(a),
			Source:    "claude",
			Kind:      defaultStr(a.Kind, "interactive"),
			Status:    defaultStr(a.Status, "unknown"),
			Cwd:       a.Cwd,
			Pid:       a.PID,
			StartedAt: a.StartedAt,
		}
		if local {
			s.WindowID = proctree.WindowFor(a.PID, paneWindows)
			s.CanAttach = s.WindowID != ""
			s.CanKill = true // kill the window if attached, else SIGTERM the pid
		} else {
			// Background / cloud agent: no local pane to attach to.
			s.CanResume = a.SessionID != ""
		}
		sessions = append(sessions, s)
	}
	return sessions, nil
}

// claudeTitle derives a short, human label: the cwd basename, or the session id.
func claudeTitle(a claudeAgent) string {
	if a.Cwd != "" {
		base := filepath.Base(a.Cwd)
		if base != "" && base != "." && base != "/" {
			return base
		}
	}
	if a.SessionID != "" {
		return shortID(a.SessionID)
	}
	return "claude"
}

func shortID(id string) string {
	if i := strings.IndexByte(id, '-'); i > 0 {
		return id[:i]
	}
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func defaultStr(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
