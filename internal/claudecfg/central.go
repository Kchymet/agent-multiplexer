package claudecfg

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"

	"amux/internal/accounts"
	"amux/internal/amuxcfg"
	"amux/internal/centralauth"
	"amux/internal/cfghome"
)

// AuthMode is the source a launch without a workload uses. Every workload
// follows central since the account store's version 2 migration.
func AuthMode() string {
	if mode := accounts.GlobalMode("claude"); mode != "blocked" {
		return mode
	}
	return amuxcfg.AuthCentral
}

// HostStoreShared reports whether sessions may reach a host Claude credential
// store (the legacy credential-file link, macOS Keychain broker, or shared
// directory). Central and environment modes expose none of them.
func HostStoreShared() bool {
	switch AuthMode() {
	case amuxcfg.AuthHost:
		return true
	case amuxcfg.AuthShared:
		return SharedAuthEnabled()
	}
	return false
}

// providerEnv are settings env keys that select another credential, provider
// or endpoint. Claude ranks each above (or redirects) CLAUDE_CODE_OAUTH_TOKEN,
// so central mode refuses to start rather than silently use something else.
// The list follows Claude 2.1.x: alternate credentials (including the
// descriptor inputs), the API endpoint and its unix-socket transport, and
// every CLAUDE_CODE_USE_* provider switch.
var providerEnv = []string{
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "CLAUDE_CODE_OAUTH_TOKEN",
	"CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR", "CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR", "ANTHROPIC_UNIX_SOCKET",
	"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
	"CLAUDE_CODE_USE_ANTHROPIC_AWS", "CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD",
	"CLAUDE_CODE_USE_MANTLE", "CLAUDE_CODE_USE_GATEWAY",
}

// managedSettingsPaths are Claude's machine-wide managed settings files. Claude
// also merges drop-ins from managed-settings.d beside each (managedDropIns).
var managedSettingsPaths = map[string]string{
	"linux":  "/etc/claude-code/managed-settings.json",
	"darwin": "/Library/Application Support/ClaudeCode/managed-settings.json",
}

// enabled reports whether a provider/credential value is set. Claude treats
// "0" and "false" as unset for its CLAUDE_CODE_USE_* switches.
func enabled(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && value != "0" && !strings.EqualFold(value, "false")
}

// CentralConflicts names (never values) configuration that would outrank or
// redirect the central token: inherited credential, endpoint or cloud-provider
// variables, and apiKeyHelper or a provider env entry in any settings layer
// Claude reads for this launch — the agent's private user settings (the
// template's before the copy is seeded), the project settings in projectDir,
// and managed settings.
func CentralConflicts(sp cfghome.Spec, projectDir string, environ []string) []string {
	found := map[string]bool{}
	for _, kv := range environ {
		key, value, _ := strings.Cut(kv, "=")
		if slices.Contains(providerEnv, key) && enabled(value) {
			found["environment "+key] = true
		}
	}
	userDir := sp.Dir
	if !cfghome.Seeded(sp) {
		userDir = sp.Template
	}
	layers := []struct{ label, path string }{
		{"user settings.json", filepath.Join(userDir, "settings.json")},
		{"user settings.local.json", filepath.Join(userDir, "settings.local.json")},
	}
	if projectDir != "" {
		layers = append(layers,
			struct{ label, path string }{"project .claude/settings.json", filepath.Join(projectDir, ".claude", "settings.json")},
			struct{ label, path string }{"project .claude/settings.local.json", ProjectSettingsLocalPath(projectDir)})
	}
	if managed := managedSettingsPaths[runtime.GOOS]; managed != "" {
		layers = append(layers, struct{ label, path string }{"managed settings", managed})
		for _, path := range managedDropIns(managed) {
			layers = append(layers, struct{ label, path string }{"managed settings " + filepath.Base(path), path})
		}
	}
	for _, layer := range layers {
		b, err := os.ReadFile(layer.path)
		if err != nil {
			continue
		}
		var settings struct {
			APIKeyHelper any            `json:"apiKeyHelper"`
			Env          map[string]any `json:"env"`
		}
		if json.Unmarshal(b, &settings) != nil {
			continue // Claude reports its own settings errors
		}
		if helper, ok := settings.APIKeyHelper.(string); ok && strings.TrimSpace(helper) != "" {
			found[layer.label+" apiKeyHelper"] = true
		}
		for _, name := range providerEnv {
			if v, ok := settings.Env[name]; ok && v != nil && enabled(fmt.Sprint(v)) {
				found[layer.label+" env."+name] = true
			}
		}
	}
	out := make([]string, 0, len(found))
	for name := range found {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// managedDropIns lists the managed-settings.d/*.json files Claude merges after
// managed-settings.json: non-hidden names ending in .json, in name order.
func managedDropIns(managed string) []string {
	dir := filepath.Join(filepath.Dir(managed), "managed-settings.d")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries { // ReadDir sorts by name
		name := e.Name()
		if !e.IsDir() && strings.HasSuffix(name, ".json") && !strings.HasPrefix(name, ".") {
			out = append(out, filepath.Join(dir, name))
		}
	}
	return out
}

// ErrCentralLoginRequired is wrapped by launch errors when central mode has no
// stored credential yet.
var ErrCentralLoginRequired = errors.New("amux Claude central login required")

// LaunchAuthError reports, before a Claude agent pane starts, why its selected
// authentication cannot work. Messages name settings, never credential values.
func LaunchAuthError(sp cfghome.Spec, projectDir string, environ []string) error {
	mode := accounts.Mode(sp.AgentID, "claude")
	switch mode {
	case amuxcfg.AuthCentral:
		st, err := centralauth.ClaudeStore().Status()
		if err != nil {
			return fmt.Errorf("amux Claude central credential unusable: %w; run `amux auth login` on the host", err)
		}
		if !st.Configured {
			return fmt.Errorf("%w: run `amux auth login claude` on the host to create one central Claude login for all sessions", ErrCentralLoginRequired)
		}
		if conflicts := CentralConflicts(sp, projectDir, environ); len(conflicts) > 0 {
			return fmt.Errorf("%s would override or redirect the central Claude login; remove it, "+
				"or use `amux auth login claude --api-key` for an API key", strings.Join(conflicts, ", "))
		}
	}
	return nil
}

// HostStoreSharedFor respects an existing workload's pinned source.
func HostStoreSharedFor(id string) bool {
	mode := accounts.Mode(id, "claude")
	return mode == amuxcfg.AuthHost || mode == amuxcfg.AuthShared && SharedAuthEnabled()
}
