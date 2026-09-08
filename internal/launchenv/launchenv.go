// Package launchenv constructs the environment of sandbox launcher processes.
// The returned environment is installed on bwrap itself, before its first exec;
// payload-only unset/clear options cannot sanitize the persistent namespace PID
// 1 process after it inherited the daemon's original environment.
package launchenv

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ModelAccount is a daemon-owned selection of the runtime account a child may
// use. It is deliberately separate from the generic environment overlay: a
// caller cannot acquire a credential merely by adding its variable name.
type ModelAccount uint8

const (
	NoModelAccount ModelAccount = iota
	ClaudeModelAccount
	CodexModelAccount
)

// ModelCapability is an opaque, daemon-owned grant for exactly one model
// account. Generic launch overlays cannot add model credentials. The optional
// explicit values exist for daemon-authorized synthetic endpoints and env-only
// account configurations; they are validated against the selected account.
type ModelCapability struct {
	account  ModelAccount
	explicit []string
}

// ForRuntime maps the daemon's authoritative harness selection onto its model
// account capability. Unknown runtimes receive no ambient model credentials.
func ForRuntime(runtime string) ModelCapability {
	switch strings.ToLower(strings.TrimSpace(runtime)) {
	case "claude":
		return ModelCapability{account: ClaudeModelAccount}
	case "codex":
		return ModelCapability{account: CodexModelAccount}
	default:
		return ModelCapability{}
	}
}

// ForModelEnvironment creates an explicit model-only capability. This is not a
// general overlay: every variable must belong to account. The daemon may use it
// for a configured env-only account, and tests may use it for a synthetic model
// endpoint without depending on ambient host credentials.
func ForModelEnvironment(account ModelAccount, entries []string) (ModelCapability, error) {
	if account == NoModelAccount {
		return ModelCapability{}, fmt.Errorf("model environment requires an account")
	}
	for _, entry := range entries {
		name, _, ok := split(entry)
		if !ok {
			return ModelCapability{}, fmt.Errorf("invalid model environment entry")
		}
		if !modelAccountNames[account][name] {
			return ModelCapability{}, fmt.Errorf("model environment variable %s does not belong to the selected account", name)
		}
	}
	return ModelCapability{account: account, explicit: append([]string(nil), entries...)}, nil
}

// ambientNames are non-authority process settings needed by ordinary command
// lookup, locale handling, and terminal rendering. Host XDG, cloud, provider,
// TLS, proxy, Git-control, dynamic-loader, and credential variables are absent
// by default. A future capability must be named explicitly here or in
// overlayNames; a secret-looking denylist is intentionally not used.
var ambientNames = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true,
	"LANG": true, "LANGUAGE": true, "TZ": true,
	"TERM": true, "COLORTERM": true, "COLORFGBG": true,
	"TERM_PROGRAM": true, "TERM_PROGRAM_VERSION": true,
	"NO_COLOR": true, "FORCE_COLOR": true, "CLICOLOR": true, "CLICOLOR_FORCE": true,
}

// overlayNames are the typed capabilities currently emitted by wsops.AgentEnv.
// Config-home and secure-storage paths are deliberate filesystem/account grants;
// AMUX fields are non-authoritative child identity hints. Authorization never
// derives from these strings.
var overlayNames = map[string]bool{
	"AMUX_AGENT": true, "AMUX_MODE": true, "AMUX_ROLE": true,
	"AMUX_ROOT": true, "AMUX_SCOPE": true, "AMUX_SESSION_ID": true,
	"AMUX_WORKGROUP": true, "AMUX_WORKSPACE": true,
	"CLAUDE_CONFIG_DIR": true, "CLAUDE_SECURESTORAGE_CONFIG_DIR": true,
	"CODEX_HOME": true,
}

var modelAccountNames = map[ModelAccount]map[string]bool{
	ClaudeModelAccount: {
		"ANTHROPIC_API_KEY": true, "ANTHROPIC_AUTH_TOKEN": true,
		"CLAUDE_CODE_OAUTH_TOKEN": true, "ANTHROPIC_BASE_URL": true,
	},
	CodexModelAccount: {
		"OPENAI_API_KEY": true, "CODEX_API_KEY": true, "OPENAI_BASE_URL": true,
	},
}

// Build filters ambient before the sandbox launcher starts, then applies the
// final caller overlay. Unknown non-empty overlay fields fail closed. Empty
// assignments are allowed because they can only remove inherited behavior (the
// Claude integration deliberately clears alternate credential variables).
func Build(ambient, overlay []string, capability ModelCapability) ([]string, error) {
	values := make(map[string]string)
	var order []string
	set := func(name, value string) {
		if _, ok := values[name]; !ok {
			order = append(order, name)
		}
		values[name] = value
	}
	for _, entry := range ambient {
		name, value, ok := split(entry)
		if !ok || (!allowedAmbient(name) && !modelAccountNames[capability.account][name]) {
			continue
		}
		if name == "PATH" {
			value = executablePath(value)
		}
		set(name, value)
	}
	for _, entry := range capability.explicit {
		name, value, _ := split(entry) // validated by ForModelEnvironment
		set(name, value)
	}
	for _, entry := range overlay {
		name, value, ok := split(entry)
		if !ok {
			return nil, fmt.Errorf("invalid launch environment entry")
		}
		if value != "" && !ambientNames[name] && !overlayNames[name] {
			return nil, fmt.Errorf("launch environment variable %s is not an explicit capability", name)
		}
		set(name, value)
	}
	out := make([]string, 0, len(order))
	for _, name := range order {
		out = append(out, name+"="+values[name])
	}
	return out, nil
}

// executablePath retains only absolute host-system paths that the namespace
// deliberately mounts. Relative entries and user-home/tool-cache paths would
// let a bare payload name resolve through attacker-controlled session content;
// user-installed runtimes are instead resolved and mounted explicitly by
// panespec before the trampoline runs.
func executablePath(value string) string {
	allowedRoots := []string{"/usr", "/bin", "/sbin", "/opt", "/nix", "/home/linuxbrew"}
	var kept []string
	for _, entry := range filepath.SplitList(value) {
		clean := filepath.Clean(entry)
		if !filepath.IsAbs(clean) {
			continue
		}
		for _, root := range allowedRoots {
			if clean == root || strings.HasPrefix(clean, root+string(filepath.Separator)) {
				kept = append(kept, clean)
				break
			}
		}
	}
	if len(kept) == 0 {
		return "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	return strings.Join(kept, string(filepath.ListSeparator))
}

func allowedAmbient(name string) bool {
	return ambientNames[name] || strings.HasPrefix(name, "LC_")
}

func split(entry string) (name, value string, ok bool) {
	name, value, ok = strings.Cut(entry, "=")
	if !ok || name == "" || strings.ContainsAny(name, "\x00=") {
		return "", "", false
	}
	return name, value, true
}
