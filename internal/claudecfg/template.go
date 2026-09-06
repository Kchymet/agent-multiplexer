package claudecfg

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"sort"

	"amux/internal/cfghome"
)

// Env is the environment variable that relocates Claude Code's home.
const Env = "CLAUDE_CONFIG_DIR"

// configEntries are the parts of a Claude Code home that are CONFIGURATION —
// what the user set up and would expect a fresh agent to inherit: settings,
// memory (CLAUDE.md), custom commands/skills/agents/hooks/output styles, key
// bindings, the status-line script, and installed plugins. Everything else in
// the home is per-machine STATE (projects/ transcripts, history, caches,
// shell-snapshots, file-history, session-env, statsig, todos, …) that a copy
// starts empty, or AUTH (.credentials.json) that is shared — see Template.
var configEntries = []string{
	"settings.json", "settings.local.json", "CLAUDE.md", "keybindings.json",
	"statusline-command.sh", "commands", "skills", "agents", "hooks",
	"output-styles", "plugins",
}

// configKeys are the keys of .claude.json that carry configuration rather than
// state. The file is mostly Claude's own bookkeeping (startup counters, caches,
// onboarding flags, per-project trust) that it rewrites constantly; only these
// are compared for drift and folded back on Promote.
var configKeys = []string{"mcpServers", "model"}

// Template describes how the user's Claude Code home (User()) is templated into
// an agent's private home at dir — the spec cfghome executes. Config entries are
// copied; .claude.json is copied minus its per-project trust table (amux
// re-trusts the agent's own dir at launch) and compared only on its config
// keys. After amux auth login, credentials use a dedicated shared directory;
// until then the legacy credential symlink points back to the template.
func Template(agentID, dir string) cfghome.Spec {
	user := User()
	entries := []cfghome.Entry{{
		Rel: ".claude.json", Src: user.ConfigPath(),
		Seed: seedClaudeJSON, Normalize: normalizeClaudeJSON, Merge: mergeClaudeJSON,
	}}
	for _, rel := range configEntries {
		e := cfghome.Entry{Rel: rel}
		if rel == "settings.json" || rel == "settings.local.json" {
			// Settings may reference files inside the home by absolute path (a
			// status-line script, a hook command). Point the copy at its own files,
			// and compare with the paths mapped back so that is never drift.
			e.Seed = rewriteTemplatePaths
			e.Normalize = restoreTemplatePaths
		}
		entries = append(entries, e)
	}
	sp := cfghome.Spec{
		Kind: "claude", AgentID: agentID, Env: Env,
		Template: user.Dir, Dir: dir,
		Entries: entries,
		Shared:  []string{CredentialsFile},
	}
	if SharedAuthEnabled() {
		sp.Shared = nil
		sp.AuthDir = SharedAuthDir()
		sp.AuthEnv = SecureStorageEnv
		sp.AuthUnsetEnv = credentialOverrides
	}
	return sp
}

// AgentHome is where an agent's private Claude home lives: under its sandbox
// dir, so it is writable inside the scope and removed with the agent.
func AgentHome(agentDir string) string { return filepath.Join(agentDir, ".amux", "claude") }

// seedClaudeJSON drops the per-project table from a copied .claude.json: those
// are the user's trust/history entries for their own directories, not config for
// the agent, and the agent's own dir is trusted fresh at launch.
func seedClaudeJSON(_ cfghome.Spec, b []byte) []byte {
	root, ok := decodeObject(b)
	if !ok {
		return b
	}
	delete(root, "projects")
	return encodeObject(root)
}

// normalizeClaudeJSON projects .claude.json onto its config keys, in canonical
// order, so Claude's constant rewrites of its bookkeeping keys never register.
func normalizeClaudeJSON(_ cfghome.Spec, b []byte) []byte {
	root, ok := decodeObject(b)
	if !ok {
		return b
	}
	keep := map[string]any{}
	for _, k := range configKeys {
		if v, ok := root[k]; ok {
			keep[k] = v
		}
	}
	return encodeObject(keep)
}

// mergeClaudeJSON folds the config keys of an agent's .claude.json into the
// template's current file, leaving the user's own state keys untouched.
func mergeClaudeJSON(_ cfghome.Spec, template, copy []byte) ([]byte, error) {
	dst, ok := decodeObject(template)
	if !ok {
		dst = map[string]any{}
	}
	src, ok := decodeObject(copy)
	if !ok {
		return template, nil
	}
	for _, k := range configKeys {
		if v, ok := src[k]; ok {
			dst[k] = v
		} else {
			delete(dst, k)
		}
	}
	return encodeObject(dst), nil
}

// rewriteTemplatePaths maps absolute references to the template dir onto the
// copy, so a settings.json that runs `bash ~/.claude/statusline.sh` runs the
// copy's script inside the sandbox (where ~/.claude does not exist).
func rewriteTemplatePaths(sp cfghome.Spec, b []byte) []byte {
	return bytes.ReplaceAll(b, []byte(sp.Template), []byte(sp.Dir))
}

// restoreTemplatePaths is rewriteTemplatePaths' inverse, applied before
// comparison so the rewrite itself is never reported as an edit.
func restoreTemplatePaths(sp cfghome.Spec, b []byte) []byte {
	return bytes.ReplaceAll(b, []byte(sp.Dir), []byte(sp.Template))
}

func decodeObject(b []byte) (map[string]any, bool) {
	root := map[string]any{}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return nil, false
	}
	return root, true
}

// encodeObject marshals with sorted keys (encoding/json sorts map keys), so
// equal content hashes equal.
func encodeObject(m map[string]any) []byte {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil
	}
	return b
}
