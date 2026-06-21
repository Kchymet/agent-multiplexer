// Package claudecfg makes minimal, safe edits to Claude Code's user config
// (~/.claude.json) on amux's behalf. Today that's pre-trusting directories amux
// creates so Claude Code doesn't show the interactive "trust this folder?"
// dialog for a freshly-spawned workspace.
package claudecfg

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

var mu sync.Mutex // serialize our own read-modify-write

// projectsRoot is where Claude Code stores per-directory session transcripts.
func projectsRoot() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, "projects")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "projects")
}

// munge maps an absolute path to Claude Code's project-dir name ('/' and '.'
// become '-'), e.g. /home/u/.local/x -> -home-u--local-x.
func munge(cwd string) string {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		abs = cwd
	}
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '.' {
			return '-'
		}
		return r
	}, abs)
}

// SessionExists reports whether a saved session with uuid exists for cwd.
func SessionExists(cwd, uuid string) bool {
	if uuid == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(projectsRoot(), munge(cwd), uuid+".jsonl"))
	return err == nil
}

// AnySession reports whether cwd has any saved Claude session transcript.
func AnySession(cwd string) bool {
	ents, err := os.ReadDir(filepath.Join(projectsRoot(), munge(cwd)))
	if err != nil {
		return false
	}
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".jsonl") {
			return true
		}
	}
	return false
}

// TurnActive reports whether the Claude session uuid for cwd is mid-turn — the
// agent is working on a response rather than sitting idle waiting for input. It
// inspects the transcript's last conversational message: the turn is finished
// only when that message is an assistant reply that ended its turn (plain text,
// no pending tool call). A user message (a fresh prompt or a returned tool
// result) or an assistant message that paused to call a tool both mean a reply
// is still in flight. Best-effort: an unreadable or unparsable transcript
// reports false (treated as idle).
func TurnActive(cwd, uuid string) bool {
	if uuid == "" {
		return false
	}
	line, ok := lastMessageLine(filepath.Join(projectsRoot(), munge(cwd), uuid+".jsonl"))
	if !ok {
		return false
	}
	var e struct {
		Message struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &e); err != nil {
		return false
	}
	if e.Message.Role != "assistant" {
		return true // a user prompt or tool result: the agent owes a reply
	}
	for _, b := range e.Message.Content {
		if b.Type == "tool_use" {
			return true // the assistant paused to call a tool: the turn continues
		}
	}
	return false // the assistant ended its turn: idle
}

// selectPrompt matches Claude Code's interactive selection cursor on a numbered
// option ("❯ 1.", "❯ 2." …). That cursor is shown only when Claude is blocked
// waiting for the user to choose — never at a plain ready input box or mid-turn.
var selectPrompt = regexp.MustCompile(`(?m)^\s*❯\s*\d+\.`)

// PromptBlocked reports whether pane (the captured visible text of a Claude Code
// pane) shows a prompt that is blocking on the user — a permission or selection
// dialog — as opposed to a finished, ready-for-input session. It keys off the
// numbered selection cursor and the permission question, neither of which
// appears at a plain ready prompt. Callers should only consult this once the
// transcript already shows the turn is not active, to avoid racing a spinner.
func PromptBlocked(pane string) bool {
	return strings.Contains(pane, "Do you want to proceed?") ||
		selectPrompt.MatchString(pane)
}

// lastMessageLine returns the last line of the JSONL transcript at path whose
// entry is a conversational message ("user" or "assistant"), skipping the
// non-message bookkeeping entries (mode, permission-mode, queue-operation, …)
// that can trail the conversation. It reads from the end in a growing window so
// long transcripts aren't read whole on every poll.
func lastMessageLine(path string) ([]byte, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, false
	}
	size := fi.Size()
	for window := int64(64 << 10); ; window *= 4 {
		if window > size {
			window = size
		}
		buf := make([]byte, window)
		if _, err := f.ReadAt(buf, size-window); err != nil && err != io.EOF {
			return nil, false
		}
		lines := bytes.Split(buf, []byte{'\n'})
		// When the window starts mid-file its first element is a partial line; skip it.
		first := 0
		if window < size {
			first = 1
		}
		for i := len(lines) - 1; i >= first; i-- {
			ln := bytes.TrimSpace(lines[i])
			if len(ln) == 0 {
				continue
			}
			var t struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(ln, &t) != nil {
				continue
			}
			if t.Type == "user" || t.Type == "assistant" {
				return ln, true
			}
		}
		if window >= size {
			return nil, false
		}
	}
}

// ConfigPath is ~/.claude.json (honoring CLAUDE_CONFIG_DIR if set).
func ConfigPath() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, ".claude.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude.json")
}

// TrustDir marks dir as trusted in ~/.claude.json. Best-effort: on any error the
// caller should proceed (Claude will just show the trust dialog once). The whole
// file is round-tripped with json.Number so large integer fields aren't mangled,
// and written atomically so a concurrent Claude process never sees a partial file.
func TrustDir(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	mu.Lock()
	defer mu.Unlock()

	path := ConfigPath()
	root := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber()
		_ = dec.Decode(&root)
	}

	projects, ok := root["projects"].(map[string]any)
	if !ok || projects == nil {
		projects = map[string]any{}
		root["projects"] = projects
	}
	entry, ok := projects[abs].(map[string]any)
	if !ok || entry == nil {
		entry = map[string]any{}
		projects[abs] = entry
	}
	if t, _ := entry["hasTrustDialogAccepted"].(bool); t {
		return nil // already trusted; don't rewrite
	}
	entry["hasTrustDialogAccepted"] = true

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".amux.tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
