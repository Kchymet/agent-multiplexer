// Package claudecfg makes minimal, safe edits to Claude Code's user config on
// amux's behalf: pre-trusting directories amux creates (so no "trust this
// folder?" dialog) and installing the status hooks that report each agent's
// activity back to the rail.
package claudecfg

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"amux/internal/core"
)

var mu sync.Mutex // serialize our own read-modify-write

// Home is one Claude Code configuration directory — the tree $CLAUDE_CONFIG_DIR
// names. It holds the user-level settings (settings.json), the mixed config/state
// file .claude.json (MCP servers, per-project trust, onboarding flags), the OAuth
// credentials (.credentials.json), and per-machine state: the projects/ transcript
// tree, history, caches, plugins.
//
// amux distinguishes two homes. User() is the user's own — the template every
// agent's config is seeded from. At(dir) is an agent's private copy, which amux
// creates under the agent's sandbox dir and points Claude at via CLAUDE_CONFIG_DIR
// (see agent.Harness.Config). Every path lookup goes through a Home so resume,
// gap-fill, listing, and trust all read the same tree Claude itself writes.
type Home struct {
	Dir string
	// split marks Claude's default layout: with CLAUDE_CONFIG_DIR unset, Claude
	// keeps .claude.json beside ~/.claude (at ~/.claude.json), not inside it. An
	// explicit config dir keeps everything under Dir.
	split bool
}

// User is the user's own Claude Code home: $CLAUDE_CONFIG_DIR when set, else
// ~/.claude with .claude.json beside it. It is the template agents are seeded
// from, and the home amux's own process-level lookups (the package-level
// functions below) read.
func User() Home {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return Home{Dir: d}
	}
	home, _ := os.UserHomeDir()
	return Home{Dir: filepath.Join(home, ".claude"), split: true}
}

// At is the home at an explicit config dir — what an agent launched with
// CLAUDE_CONFIG_DIR=dir uses. Claude then keeps .claude.json inside dir too.
func At(dir string) Home { return Home{Dir: dir} }

// ProjectsRoot is where this home stores per-directory session transcripts.
func (h Home) ProjectsRoot() string { return filepath.Join(h.Dir, "projects") }

// ConfigPath is this home's .claude.json — beside the dir for the default
// layout, inside it for an explicit CLAUDE_CONFIG_DIR.
func (h Home) ConfigPath() string {
	if h.split {
		return filepath.Join(filepath.Dir(h.Dir), ".claude.json")
	}
	return filepath.Join(h.Dir, ".claude.json")
}

// SettingsPath is this home's user-level settings.json (hooks, permissions,
// env, status line, enabled plugins).
func (h Home) SettingsPath() string { return filepath.Join(h.Dir, "settings.json") }

// CredentialsPath is this home's OAuth credentials file. It is auth, not config:
// amux never copies it into an agent's home — see Template.
func (h Home) CredentialsPath() string { return filepath.Join(h.Dir, CredentialsFile) }

// CredentialsFile is the name of the OAuth credentials file inside a home.
const CredentialsFile = ".credentials.json"

// projectsRoot is the user home's transcript tree (package-level convenience).
func projectsRoot() string { return User().ProjectsRoot() }

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

// ProjectDirName is Claude Code's project-dir name for cwd — the load-bearing path
// munge that resume detection, gap-fill, and transcript listing all depend on.
// Exposed so a contract test and the doctor drift probe can verify amux's copy of
// the convention against Claude's actual on-disk layout (see MungeDrift).
func ProjectDirName(cwd string) string { return munge(cwd) }

// MungeDrift detects divergence between Claude Code's project-dir naming scheme
// and amux's copy of it: for every discovered transcript whose originating cwd we
// can read, it flags any whose project-dir name isn't derivable from that cwd (or
// any ancestor of it) by amux's path munge. A non-empty result means an upstream
// Claude change broke the '/'/'.' → '-' scheme — resume, capture, and listing
// would silently degrade — so the doctor probe surfaces it loudly instead of
// best-effort silence.
//
// The ancestor allowance matters: amux launches an agent in a dir and the agent
// may cd into a repo worktree beneath it, so Claude's project dir (keyed to the
// launch cwd) is munge(cwd) or munge of an ancestor — that's a normal cwd shift,
// not scheme drift, and must not be flagged. Only a project dir that matches no
// level is genuine munge drift. Transcripts with an unreadable cwd are skipped.
func MungeDrift() []string { return User().MungeDrift() }

// MungeDrift is the drift probe over this home's transcripts (see the
// package-level MungeDrift).
func (h Home) MungeDrift() []string {
	var out []string
	for _, s := range h.ListSessions() {
		if s.Cwd == "" || s.Project == "" {
			continue
		}
		if !mungeMatchesAncestor(s.Cwd, s.Project) {
			out = append(out, fmt.Sprintf("%s: stored under %q, not derivable from cwd %q by amux's path munge", s.ID, s.Project, s.Cwd))
		}
	}
	return out
}

// mungeMatchesAncestor reports whether project equals munge(cwd) or munge of any
// ancestor directory of cwd — i.e. whether the '/'/'.' → '-' scheme still explains
// where Claude stored the transcript, allowing for amux's launch-dir-vs-recorded-cwd
// shift.
func mungeMatchesAncestor(cwd, project string) bool {
	for p := cwd; ; {
		if munge(p) == project {
			return true
		}
		parent := filepath.Dir(p)
		if parent == p {
			return false
		}
		p = parent
	}
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
func (h Home) sessionPresent(cwd, uuid string) bool {
	base := filepath.Join(h.ProjectsRoot(), munge(cwd), uuid)
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
func TranscriptPath(cwd, uuid string) string { return User().TranscriptPath(cwd, uuid) }

// TranscriptPath is the transcript path for uuid under cwd in this home.
func (h Home) TranscriptPath(cwd, uuid string) string {
	return filepath.Join(h.ProjectsRoot(), munge(cwd), uuid+".jsonl")
}

// ProjectDir is the directory this home keeps cwd's transcripts (and their
// per-session working areas) in: <projects>/<munge(cwd)>.
func (h Home) ProjectDir(cwd string) string {
	return filepath.Join(h.ProjectsRoot(), munge(cwd))
}

// SessionExists reports whether a saved session with uuid exists for cwd.
func SessionExists(cwd, uuid string) bool { return User().SessionExists(cwd, uuid) }

// SessionExists reports whether uuid's transcript for cwd exists in this home.
func (h Home) SessionExists(cwd, uuid string) bool {
	if uuid == "" {
		return false
	}
	return h.sessionPresent(cwd, uuid)
}

// FindSession looks for uuid's transcript under each candidate cwd in order and
// returns the first cwd it lives under. Callers launch `claude --resume` with
// that cwd so Claude's own path munge lands on the same project dir where the
// transcript was written — necessary because amux's working-dir convention has
// changed over time, so a session pinned under one convention can have its
// transcript stored under another. ok is false if no candidate has it.
func FindSession(uuid string, cwds ...string) (cwd string, ok bool) {
	return User().FindSession(uuid, cwds...)
}

// FindSession is the resume lookup within this home (see the package-level
// FindSession).
func (h Home) FindSession(uuid string, cwds ...string) (cwd string, ok bool) {
	if uuid == "" {
		return "", false
	}
	for _, c := range cwds {
		if h.sessionPresent(c, uuid) {
			return c, true
		}
	}
	return "", false
}

// AnySession reports whether cwd has any saved Claude session transcript.
func AnySession(cwd string) bool { return User().AnySession(cwd) }

// AnySession reports whether cwd has any saved transcript in this home.
func (h Home) AnySession(cwd string) bool {
	ents, err := os.ReadDir(h.ProjectDir(cwd))
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

// SessionInfo describes one saved Claude Code session transcript discovered
// under the projects root: its session id, the working directory it ran in, the
// transcript path, and file metadata. It's what `amux agent sessions` reports so
// an agent can reason across every conversation on the machine without needing
// to know Claude Code's project-dir munge convention.
type SessionInfo struct {
	ID       string    `json:"id"`       // Claude session UUID (the transcript's base name)
	Cwd      string    `json:"cwd"`      // originating working dir, read from the transcript (best-effort)
	Project  string    `json:"project"`  // Claude Code's munged project-dir name (always present)
	Path     string    `json:"path"`     // absolute path to the .jsonl transcript
	Size     int64     `json:"size"`     // transcript size in bytes
	Modified time.Time `json:"modified"` // transcript mtime (proxy for last activity)
}

// ListSessions enumerates every Claude Code session transcript across all
// projects under the projects root, most-recently-modified first. It's
// best-effort: unreadable project dirs and files are skipped rather than failing
// the whole listing, so an agent always gets whatever is readable. Only the
// per-project top-level <uuid>.jsonl transcripts are reported (mirroring
// AnySession) — a session's subagents/ working area is not itself a session.
func ListSessions() []SessionInfo { return User().ListSessions() }

// ListSessions enumerates this home's transcripts, most recent first.
func (h Home) ListSessions() []SessionInfo {
	root := h.ProjectsRoot()
	projects, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []SessionInfo
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		projDir := filepath.Join(root, p.Name())
		ents, err := os.ReadDir(projDir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			fi, err := e.Info()
			if err != nil {
				continue
			}
			path := filepath.Join(projDir, e.Name())
			out = append(out, SessionInfo{
				ID:       strings.TrimSuffix(e.Name(), ".jsonl"),
				Cwd:      transcriptCwd(path),
				Project:  p.Name(),
				Path:     path,
				Size:     fi.Size(),
				Modified: fi.ModTime(),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Modified.After(out[j].Modified)
	})
	return out
}

// transcriptCwd reads the originating working directory from a transcript's
// first record that carries one. Claude Code stamps its JSONL lines with the
// session's cwd; we return "" if it can't be read (the munge is lossy, so the
// caller falls back to the project-dir name for display).
func transcriptCwd(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // transcript lines can be large
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var rec struct {
			Cwd string `json:"cwd"`
		}
		if json.Unmarshal(line, &rec) == nil && rec.Cwd != "" {
			return rec.Cwd
		}
	}
	return ""
}

// ConfigPath is the user home's .claude.json (~/.claude.json, or inside
// CLAUDE_CONFIG_DIR when set).
func ConfigPath() string { return User().ConfigPath() }

// PreferredModel returns the user's configured Claude Code model (the top-level
// "model" key in ~/.claude.json), or "" if unset or unreadable. amux uses it as
// the rational default when interactively configuring a new agent, so the user
// doesn't have to retype their usual model every time. Best-effort — callers
// treat "" as "let Claude pick its own default".
func PreferredModel() string { return User().PreferredModel() }

// PreferredModel is the "model" key of this home's .claude.json, or "".
func (h Home) PreferredModel() string {
	b, err := os.ReadFile(h.ConfigPath())
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
func TrustDir(dir string) error { return User().TrustDir(dir) }

// TrustDir marks dir as trusted in this home's .claude.json (see the
// package-level TrustDir).
func (h Home) TrustDir(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	mu.Lock()
	defer mu.Unlock()

	path := h.ConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
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

// SettingsPath is the user home's settings.json (honoring CLAUDE_CONFIG_DIR).
// This is where hook configuration lives — distinct from ConfigPath's .claude.json.
func SettingsPath() string { return User().SettingsPath() }

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

// HookEventState returns the activity state a Claude Code hook event implies, and
// whether amux listens on that event. This is the authoritative status contract
// (Claude runs `amux agent hook <state>` on each event); exposed so a contract
// test pins the mapping and catches an upstream rename of a hook event.
func HookEventState(event string) (state string, ok bool) {
	for _, he := range hookEvents {
		if he.event == event {
			return he.state, true
		}
	}
	return "", false
}

// HookEventNames returns, in order, the Claude hook events amux maps to a state.
func HookEventNames() []string {
	out := make([]string, len(hookEvents))
	for i, he := range hookEvents {
		out[i] = he.event
	}
	return out
}

// HookPayload is the JSON Claude Code pipes to amux's hook commands on stdin — the
// load-bearing hook wire shape. amux reads the session id and cwd for status, and
// the transcript path and event name for capture. Defined here (the Claude surface)
// and consumed by the `amux agent hook`/`capture` handlers, so a contract test can
// pin the field names against a recorded payload.
type HookPayload struct {
	SessionID      string `json:"session_id"`
	Cwd            string `json:"cwd"`
	TranscriptPath string `json:"transcript_path"`
	HookEventName  string `json:"hook_event_name"`
}

// captureEvents are the hook events on which amux snapshots the conversation
// transcript (`amux agent capture`). They span turn start, every tool boundary,
// subagent completion, compaction, and turn/session end, so a durable copy exists
// even if the agent is killed mid-turn — the case the "restarting" bug loses.
// Distinct from hookEvents, which drive activity state.
var captureEvents = []string{
	"UserPromptSubmit", "PostToolUse", "SubagentStop", "Stop", "PreCompact", "SessionEnd",
}

// ProjectSettingsLocalPath is the per-directory Claude Code settings file amux
// writes an agent's status hooks into: <dir>/.claude/settings.local.json. amux
// installs hooks here — scoped to each agent's own directory — rather than in the
// user-wide settings.json, so a stale entry can never break every session at once
// (the failure mode of the old global install). settings.local.json is the local
// scope: Claude merges its hooks with the user's, and by convention it's the
// personal/uncommitted file (amux also git-excludes it; see wsops.AgentCommand).
func ProjectSettingsLocalPath(dir string) string {
	return filepath.Join(dir, ".claude", "settings.local.json")
}

// InstallHooksIn points Claude Code's status hooks at amuxPath ("amux agent hook
// <state>") for the agent whose launch directory is dir, writing them into that
// dir's settings.local.json. Because Claude loads settings only from the launch
// directory (never a parent), dir must be the agent's actual cwd. Idempotent and
// preserves any non-amux hooks. Best-effort — callers proceed on error (status
// just falls back to "unknown").
func InstallHooksIn(dir, amuxPath string) error {
	mu.Lock()
	defer mu.Unlock()
	return writeHooks(ProjectSettingsLocalPath(dir), amuxPath)
}

// writeHooks installs amux's status + capture hook groups into the settings.json
// at settingsPath, pointed at amuxPath. It reads any existing file, replaces
// amux's own hook groups (so a moved binary or changed event set is corrected)
// while leaving other hooks untouched, and writes the result back atomically.
func writeHooks(settingsPath, amuxPath string) error {
	path := settingsPath
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

	// Default Claude to the fullscreen TUI renderer. It draws on the alternate
	// screen and handles mouse-wheel scrolling; the default inline renderer does
	// not, and inside an amux mirror pane (which forwards raw wheel events to a
	// mouse-tracking child) that means the agent tab can't be scrolled at all.
	// Set only when unset so a deliberate `/tui default` choice in this file wins.
	if _, ok := root["tui"]; !ok {
		root["tui"] = "fullscreen"
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

// UninstallHooks removes amux's status/capture hook groups from Claude Code's
// *user* settings.json. amux used to install its hooks there globally, pinned to
// the running binary's path — which broke every session at once once that binary
// (often a per-session dev build) vanished. Hooks now live per-agent (see
// InstallHooksIn), so this strips the stale global entries, dropping any event
// key and the hooks map itself once they're emptied. Idempotent: it writes
// nothing when there's nothing of amux's to remove. Best-effort — callers
// proceed on error.
func UninstallHooks() error {
	mu.Lock()
	defer mu.Unlock()

	path := SettingsPath()
	b, err := os.ReadFile(path)
	if err != nil {
		return nil // no user settings — nothing to clean
	}
	root := map[string]any{}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return err
	}
	hooks, ok := root["hooks"].(map[string]any)
	if !ok || hooks == nil {
		return nil
	}
	changed := false
	for event, v := range hooks {
		groups, ok := v.([]any)
		if !ok {
			continue
		}
		kept := make([]any, 0, len(groups))
		for _, g := range groups {
			if isAmuxHookGroup(g) {
				changed = true
				continue
			}
			kept = append(kept, g)
		}
		if len(kept) == 0 {
			delete(hooks, event) // drop the now-empty event rather than leave "Stop": []
		} else {
			hooks[event] = kept
		}
	}
	if !changed {
		return nil
	}
	if len(hooks) == 0 {
		delete(root, "hooks")
	}
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
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
