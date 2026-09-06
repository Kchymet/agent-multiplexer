package codexcfg

import (
	"path/filepath"
	"strings"

	"amux/internal/cfghome"
)

// Env is the environment variable that relocates Codex's home.
const Env = "CODEX_HOME"

// configEntries are the parts of a Codex home that are configuration: the user
// config, global instructions, custom prompts, skills, and rules. The sessions/
// rollout tree, history, logs, and memories are per-machine state a copy starts
// empty; account and MCP OAuth credentials are shared (see Template).
var configEntries = []string{ConfigFile, "AGENTS.md", "prompts", "skills", "rules"}

// Template describes how the user's Codex home (UserHome()) is templated into an
// agent's private home at dir. config.toml is compared without its [projects.*]
// trust tables, which amux itself writes into the copy at launch (TrustDir).
func Template(agentID, dir string) cfghome.Spec {
	entries := make([]cfghome.Entry, 0, len(configEntries))
	for _, rel := range configEntries {
		e := cfghome.Entry{Rel: rel}
		if rel == ConfigFile {
			e.Normalize = normalizeConfig
		}
		entries = append(entries, e)
	}
	return cfghome.Spec{
		Kind: "codex", AgentID: agentID, Env: Env,
		Template: UserHome().Dir(), Dir: dir,
		Entries: entries,
		Shared:  []string{AuthFile, MCPCredentialsFile, "mcp-oauth-locks"},
		// Codex opens MCP credentials with O_NOFOLLOW when refreshing tokens.
		// A hard link shares the credential without failing that regular-file check.
		HardlinkShared: []string{MCPCredentialsFile},
	}
}

// AgentHome is where an agent's private Codex home lives: under its sandbox dir.
func AgentHome(agentDir string) string { return filepath.Join(agentDir, ".amux", "codex") }

// normalizeConfig strips the [projects."…"] trust tables from config.toml so the
// trust amux grants the agent's own dir never reads as an edit. Everything up to
// the next table header after a projects header is dropped.
func normalizeConfig(_ cfghome.Spec, b []byte) []byte {
	var out []string
	skipping := false
	for _, ln := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "[") {
			skipping = strings.HasPrefix(t, "[projects.")
		}
		if !skipping {
			out = append(out, ln)
		}
	}
	return []byte(strings.TrimRight(strings.Join(out, "\n"), "\n") + "\n")
}
