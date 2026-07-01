// Package claudecfg makes minimal, safe edits to Claude Code's user config on
// amux's behalf: pre-trusting directories amux creates (so no "trust this
// folder?" dialog) and installing the status hooks that report each agent's
// activity back to the rail.
package claudecfg

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"amux/internal/core"
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

// sessionPresent reports whether uuid's session is resumable on disk for cwd —
// meaning an actual transcript exists. Claude Code writes it as
// <projects>/<munge(cwd)>/<uuid>.jsonl.
//
// A bare <uuid>/ directory (Claude's per-session working area, holding e.g.
// subagents/) does NOT count: it can outlive the transcript when the agent is
// killed before flushing, and `claude --resume` on such a session fails outright
// ("No conversation found") — which would leave the agent unable to open at all
// instead of falling back to a fresh start. So we require the transcript itself:
// the <uuid>.jsonl file, or a .jsonl inside the <uuid>/ working dir.
func sessionPresent(cwd, uuid string) bool {
	base := filepath.Join(projectsRoot(), munge(cwd), uuid)
	if fi, err := os.Stat(base + ".jsonl"); err == nil && !fi.IsDir() {
		return true
	}
	return dirHasTranscript(base)
}

// dirHasTranscript reports whether dir directly contains a .jsonl transcript.
// Only the immediate entries are considered — a subagents/ subdir with its own
// .jsonl files is not the session's own transcript and must not count.
func dirHasTranscript(dir string) bool {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			return true
		}
	}
	return false
}

// TranscriptPath returns the path where Claude Code stores uuid's transcript for
// cwd: <projects>/<munge(cwd)>/<uuid>.jsonl. It routes the munge convention
// through this package so gap-fill (restoring a captured backup into the path
// Claude expects) stays consistent with resume detection (sessionPresent), which
// reads the very same location.
func TranscriptPath(cwd, uuid string) string {
	return filepath.Join(projectsRoot(), munge(cwd), uuid+".jsonl")
}

// SessionExists reports whether a saved session with uuid exists for cwd.
func SessionExists(cwd, uuid string) bool {
	if uuid == "" {
		return false
	}
	return sessionPresent(cwd, uuid)
}

// FindSession looks for uuid's transcript under each candidate cwd in order and
// returns the first cwd it lives under. Callers launch `claude --resume` with
// that cwd so Claude's own path munge lands on the same project dir where the
// transcript was written — necessary because amux's working-dir convention has
// changed over time, so a session pinned under one convention can have its
// transcript stored under another. ok is false if no candidate has it.
func FindSession(uuid string, cwds ...string) (cwd string, ok bool) {
	if uuid == "" {
		return "", false
	}
	for _, c := range cwds {
		if sessionPresent(c, uuid) {
			return c, true
		}
	}
	return "", false
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

// ConfigPath is ~/.claude.json (honoring CLAUDE_CONFIG_DIR if set).
func ConfigPath() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, ".claude.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude.json")
}

// PreferredModel returns the user's configured Claude Code model (the top-level
// "model" key in ~/.claude.json), or "" if unset or unreadable. amux uses it as
// the rational default when interactively configuring a new agent, so the user
// doesn't have to retype their usual model every time. Best-effort — callers
// treat "" as "let Claude pick its own default".
func PreferredModel() string {
	b, err := os.ReadFile(ConfigPath())
	if err != nil {
		return ""
	}
	var root struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(b, &root) != nil {
		return ""
	}
	return strings.TrimSpace(root.Model)
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

// SettingsPath is Claude Code's user settings.json (honoring CLAUDE_CONFIG_DIR).
// This is where hook configuration lives — distinct from ConfigPath's .claude.json.
func SettingsPath() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, "settings.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "settings.json")
}

// hookEvents maps each Claude Code hook event amux listens on to the activity
// state it implies. Driven by hooks rather than by scraping the transcript or
// pane, this is the authoritative source of an agent's status.
var hookEvents = []struct{ event, state string }{
	{"SessionStart", core.StateReady},       // launched, no turn yet
	{"UserPromptSubmit", core.StateRunning}, // a turn began
	{"Notification", core.StateWaiting},     // needs the user (permission / idle prompt)
	{"Stop", core.StateReady},               // finished the turn
	{"SessionEnd", core.StateIdle},          // agent exited
}

// captureEvents are the hook events on which amux snapshots the conversation
// transcript (`amux agent capture`). They span turn start, every tool boundary,
// subagent completion, compaction, and turn/session end, so a durable copy exists
// even if the agent is killed mid-turn — the case the "restarting" bug loses.
// Distinct from hookEvents, which drive activity state.
var captureEvents = []string{
	"UserPromptSubmit", "PostToolUse", "SubagentStop", "Stop", "PreCompact", "SessionEnd",
}

// HooksVersion is bumped whenever the installed hook set changes, so the one-time
// installer (see cmd/amux ensureHooks) re-runs and picks up the new hooks.
const HooksVersion = "2"

// InstallHooks points Claude Code's status hooks at amuxPath ("amux agent hook
// <state>"), writing them into the user settings.json. It is idempotent and
// preserves any non-amux hooks: existing amux entries are replaced (so a moved
// binary or changed event set is corrected), other hooks are left untouched.
// Best-effort — callers proceed on error (status just falls back to "unknown").
func InstallHooks(amuxPath string) error {
	mu.Lock()
	defer mu.Unlock()

	path := SettingsPath()
	root := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber()
		_ = dec.Decode(&root)
	}

	hooks, ok := root["hooks"].(map[string]any)
	if !ok || hooks == nil {
		hooks = map[string]any{}
		root["hooks"] = hooks
	}
	// Build the amux commands per event: the status hook (activity state) and, on
	// the capture events, the transcript-snapshot hook. Some events (Stop,
	// SessionEnd, UserPromptSubmit) get both.
	amuxCmds := map[string][]string{}
	for _, he := range hookEvents {
		amuxCmds[he.event] = append(amuxCmds[he.event], amuxPath+" agent hook "+he.state)
	}
	for _, ev := range captureEvents {
		amuxCmds[ev] = append(amuxCmds[ev], amuxPath+" agent capture")
	}
	events := make([]string, 0, len(amuxCmds))
	for ev := range amuxCmds {
		events = append(events, ev)
	}
	sort.Strings(events) // stable settings.json output

	for _, event := range events {
		var groups []any
		if existing, ok := hooks[event].([]any); ok {
			for _, g := range existing {
				if !isAmuxHookGroup(g) { // keep the user's own hooks; drop old amux ones
					groups = append(groups, g)
				}
			}
		}
		for _, cmd := range amuxCmds[event] {
			groups = append(groups, map[string]any{
				"hooks": []any{map[string]any{"type": "command", "command": cmd}},
			})
		}
		hooks[event] = groups
	}

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".amux.tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// isAmuxHookGroup reports whether a hook group is one amux installed, recognized
// by an "amux … hook …" command — so reinstalling replaces it instead of
// stacking. The " hook " match covers both the current "amux agent hook <state>"
// form and the legacy "amux hook <state>" one, so old installs migrate cleanly.
func isAmuxHookGroup(g any) bool {
	m, ok := g.(map[string]any)
	if !ok {
		return false
	}
	hs, ok := m["hooks"].([]any)
	if !ok {
		return false
	}
	for _, h := range hs {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		// Recognize both current forms — "amux agent hook <state>" and
		// "amux agent capture" — and the legacy "amux hook <state>", so a reinstall
		// replaces amux's own groups instead of stacking them.
		if cmd, _ := hm["command"].(string); strings.Contains(cmd, "amux") &&
			(strings.Contains(cmd, " agent ") || strings.Contains(cmd, " hook ")) {
			return true
		}
	}
	return false
}
