package claudecfg

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"amux/internal/amuxcfg"
	"amux/internal/centralauth"
	"amux/internal/cfghome"
)

// AuthMode is the effective claude.auth selection. An unreadable or invalid
// setting yields central, which shares no host store; LaunchAuthError reports
// the configuration error so the launch fails instead of guessing.
func AuthMode() string {
	mode, err := amuxcfg.AuthMode("claude")
	if err != nil {
		return amuxcfg.AuthCentral
	}
	return mode
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
var providerEnv = []string{
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "CLAUDE_CODE_OAUTH_TOKEN",
	"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
}

// inheritedOverrides are the Claude model-account variables the launcher can
// pass from the daemon environment (see launchenv).
var inheritedOverrides = []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "CLAUDE_CODE_OAUTH_TOKEN"}

// CentralConflicts names (never values) the configuration that would outrank
// or redirect the central token: nonempty inherited credential variables and,
// in the agent's private settings, apiKeyHelper or a provider env entry.
func CentralConflicts(sp cfghome.Spec, environ []string) []string {
	found := map[string]bool{}
	for _, kv := range environ {
		key, value, _ := strings.Cut(kv, "=")
		for _, name := range inheritedOverrides {
			if key == name && value != "" {
				found["environment "+name] = true
			}
		}
	}
	// Before the first launch seeds the copy, its settings are the template's.
	settingsDir := sp.Dir
	if !cfghome.Seeded(sp) {
		settingsDir = sp.Template
	}
	for _, rel := range []string{"settings.json", "settings.local.json"} {
		b, err := os.ReadFile(filepath.Join(settingsDir, rel))
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
			found[rel+" apiKeyHelper"] = true
		}
		for _, name := range providerEnv {
			if v, ok := settings.Env[name]; ok && v != nil && fmt.Sprint(v) != "" {
				found[rel+" env."+name] = true
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

// ErrCentralLoginRequired is wrapped by launch errors when central mode has no
// stored credential yet.
var ErrCentralLoginRequired = errors.New("amux Claude central login required")

// LaunchAuthError reports, before a Claude agent pane starts, why its selected
// authentication cannot work. Messages name settings, never credential values.
func LaunchAuthError(sp cfghome.Spec, environ []string) error {
	mode, err := amuxcfg.AuthMode("claude")
	if err != nil {
		return err
	}
	switch mode {
	case amuxcfg.AuthCentral:
		st, err := centralauth.ClaudeStore().Status()
		if err != nil {
			return fmt.Errorf("amux Claude central credential unusable: %w; run `amux auth login` on the host", err)
		}
		if !st.Configured {
			return fmt.Errorf("%w: run `amux auth login` on the host to create one central Claude token for all sessions; "+
				"to keep a previous method instead run one of `amux config set claude.auth host` (host Claude login), "+
				"`amux config set claude.auth shared` (older amux login) or "+
				"`amux config set claude.auth environment` (API key, apiKeyHelper, cloud provider or gateway)", ErrCentralLoginRequired)
		}
		if conflicts := CentralConflicts(sp, environ); len(conflicts) > 0 {
			return fmt.Errorf("claude.auth is central, but %s would override or redirect the central token; "+
				"remove it, or run `amux config set claude.auth environment` to keep that method", strings.Join(conflicts, ", "))
		}
	case amuxcfg.AuthShared:
		if !SharedAuthEnabled() {
			return fmt.Errorf("claude.auth is shared, but no older amux shared Claude login exists; run `amux config unset claude.auth` and `amux auth login`")
		}
	}
	return nil
}
