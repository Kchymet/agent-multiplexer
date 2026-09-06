// Package providercfg is the on-disk half of amux provider mode: the config file
// `amux provide install` writes, and the per-user service that runs `amux
// provide` from it (a systemd user unit on Linux/WSL2, a launchd agent on
// macOS). Running the provider by hand in a terminal that must stay open is the
// thing this package exists to replace — see docs/remote-provider.md.
//
// The config file never holds the bearer token. It holds the *path* to the token
// file (mode 0600), so rotating the credential is a write to that one file and
// needs no reinstall, and a config that leaks is not a credential that leaks.
package providercfg

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"amux/internal/agent"
	"amux/internal/core"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// Config is the provider configuration as it lives on disk. The field set is
// exactly the `amux provide` flag set minus the token itself; the TOML keys are
// the flag names verbatim, so the file reads like the command line it replaces.
type Config struct {
	Harnesses        []string          // nil/empty: automatic discovery; otherwise verified allowlist
	IdentityMode     string            // currently only machine is supported
	Orchestrator     string            // orchestrator address host:port
	TokenFile        string            // path to the 0600 file holding the bearer token
	Name             string            // provider display name (default: hostname)
	CAFile           string            // private CA to trust on top of the system roots
	ServerName       string            // TLS server name for SNI/verification
	MaxPanes         int               // capability: max concurrent panes (0 = unset)
	PublishSessions  bool              // advertise the "sessions" feature
	ReadOnlySessions bool              // publish inventory but reject lifecycle verbs
	RuntimeEvents    bool              // advertise "runtime-events" (needs PublishSessions)
	Labels           map[string]string // scheduling labels (advisory)
	Features         []string          // opaque capability feature strings
}

// Path is the provider config file. It sits beside amux's other user config so
// `amux config path`'s directory is the one place to look.
func Path() string { return filepath.Join(core.ConfigDir(), "provider.toml") }

// Validate reports whether a config is complete enough to run from: an
// orchestrator to dial, a token file to read the credential out of, and no
// feature that depends on one that is off.
func (c Config) Validate() error {
	if err := c.ValidateExecution(); err != nil {
		return err
	}
	if strings.TrimSpace(c.Orchestrator) == "" {
		return fmt.Errorf("need an orchestrator address (--orchestrator host:port)")
	}
	if strings.TrimSpace(c.TokenFile) == "" {
		return fmt.Errorf("need a token file (--token-file <path>); the token is never taken from argv")
	}
	if c.RuntimeEvents && !c.PublishSessions {
		return fmt.Errorf("--runtime-events requires --publish-sessions")
	}
	if c.ReadOnlySessions && !c.PublishSessions {
		return fmt.Errorf("--read-only-sessions requires --publish-sessions")
	}
	return nil
}

// Load reads the config file. A missing file surfaces as an fs.ErrNotExist-
// wrapping error, which callers distinguish with errors.Is: "not configured" is
// a normal state (provider mode is opt-in), not a failure.
func Load() (Config, error) { return LoadFrom(Path()) }

// LoadFrom reads a config from an explicit path.
func LoadFrom(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	c, err := Parse(b)
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Save writes the config file, creating the config dir. The file carries no
// secret (the token lives in TokenFile), so it is world-readable like amux's
// other config; the token file is the thing held at 0600.
func Save(c Config) error { return SaveTo(Path(), c) }

// SaveTo writes a config to an explicit path.
func SaveTo(path string, c Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, c.Marshal(), 0o644)
}

// Marshal renders the config as TOML. Output is deterministic (labels and
// features sorted) so rewriting an unchanged config leaves the file byte-identical
// and a diff shows only what the user actually changed.
func (c Config) Marshal() []byte {
	var b strings.Builder
	b.WriteString("# amux provider configuration — written by `amux provide install`.\n")
	b.WriteString("# Keys are the `amux provide` flag names; bare `amux provide` reads this file.\n")
	b.WriteString("# The bearer token is NOT here: it stays in the 0600 file named by token-file.\n\n")

	str := func(key, v string) {
		if v != "" {
			fmt.Fprintf(&b, "%s = %s\n", key, tomlString(v))
		}
	}
	str("orchestrator", c.Orchestrator)
	str("token-file", c.TokenFile)
	str("name", c.Name)
	str("identity-mode", c.IdentityMode)
	harnesses := NormalizeHarnesses(c.Harnesses)
	if len(harnesses) > 0 {
		quoted := make([]string, len(harnesses))
		for i, h := range harnesses {
			quoted[i] = tomlString(h)
		}
		fmt.Fprintf(&b, "harnesses = [%s]\n", strings.Join(quoted, ", "))
	}
	str("ca-file", c.CAFile)
	str("server-name", c.ServerName)
	if c.MaxPanes > 0 {
		fmt.Fprintf(&b, "max-panes = %d\n", c.MaxPanes)
	}
	boolean := func(key string, v bool) {
		if v {
			fmt.Fprintf(&b, "%s = true\n", key)
		}
	}
	boolean("publish-sessions", c.PublishSessions)
	boolean("read-only-sessions", c.ReadOnlySessions)
	boolean("runtime-events", c.RuntimeEvents)

	if len(c.Features) > 0 {
		feats := append([]string(nil), c.Features...)
		sort.Strings(feats)
		quoted := make([]string, len(feats))
		for i, f := range feats {
			quoted[i] = tomlString(f)
		}
		fmt.Fprintf(&b, "features = [%s]\n", strings.Join(quoted, ", "))
	}
	if len(c.Labels) > 0 {
		keys := make([]string, 0, len(c.Labels))
		for k := range c.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString("\n[labels]\n")
		for _, k := range keys {
			fmt.Fprintf(&b, "%s = %s\n", k, tomlString(c.Labels[k]))
		}
	}
	return []byte(b.String())
}

// Parse reads the TOML subset amux writes: comments, top-level scalar keys
// (string, integer, boolean), a string array, and the one [labels] table. It is
// deliberately not a general TOML parser — a file it cannot read is an error the
// user sees, never a setting silently dropped.
func Parse(b []byte) (Config, error) {
	c := Config{}
	table := ""
	for i, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(stripComment(raw))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			if !strings.HasSuffix(line, "]") {
				return Config{}, lineErr(i, "unterminated table header")
			}
			table = strings.TrimSpace(line[1 : len(line)-1])
			if table != "labels" {
				return Config{}, lineErr(i, fmt.Sprintf("unknown table [%s] (only [labels] exists)", table))
			}
			continue
		}
		key, rest, ok := strings.Cut(line, "=")
		if !ok {
			return Config{}, lineErr(i, "want `key = value`")
		}
		key = strings.TrimSpace(key)
		val, err := parseValue(strings.TrimSpace(rest))
		if err != nil {
			return Config{}, lineErr(i, err.Error())
		}
		if table == "labels" {
			s, ok := val.(string)
			if !ok {
				return Config{}, lineErr(i, "label values must be strings")
			}
			if c.Labels == nil {
				c.Labels = map[string]string{}
			}
			c.Labels[key] = s
			continue
		}
		if err := c.assign(key, val); err != nil {
			return Config{}, lineErr(i, err.Error())
		}
	}
	return c, nil
}

// assign sets one top-level key, rejecting both unknown keys and values of the
// wrong type.
func (c *Config) assign(key string, val any) error {
	str := func(dst *string) error {
		s, ok := val.(string)
		if !ok {
			return fmt.Errorf("%s must be a string", key)
		}
		*dst = s
		return nil
	}
	boolean := func(dst *bool) error {
		v, ok := val.(bool)
		if !ok {
			return fmt.Errorf("%s must be true or false", key)
		}
		*dst = v
		return nil
	}
	switch key {
	case "orchestrator":
		return str(&c.Orchestrator)
	case "token-file":
		return str(&c.TokenFile)
	case "name":
		return str(&c.Name)
	case "ca-file":
		return str(&c.CAFile)
	case "server-name":
		return str(&c.ServerName)
	case "max-panes":
		n, ok := val.(int)
		if !ok {
			return fmt.Errorf("max-panes must be an integer")
		}
		c.MaxPanes = n
		return nil
	case "publish-sessions":
		return boolean(&c.PublishSessions)
	case "read-only-sessions":
		return boolean(&c.ReadOnlySessions)
	case "runtime-events":
		return boolean(&c.RuntimeEvents)
	case "identity-mode":
		return str(&c.IdentityMode)
	case "harnesses":
		v, ok := val.([]string)
		if !ok {
			return fmt.Errorf("harnesses must be an array of strings")
		}
		c.Harnesses = v
		return nil
	case "features":
		v, ok := val.([]string)
		if !ok {
			return fmt.Errorf("features must be an array of strings")
		}
		c.Features = v
		return nil
	default:
		return fmt.Errorf("unknown key %q", key)
	}
}

// parseValue reads one TOML value: a basic string, a bool, an integer, or an
// array of strings.
func parseValue(s string) (any, error) {
	switch {
	case s == "":
		return nil, fmt.Errorf("missing value")
	case s[0] == '"':
		return unquote(s)
	case s == "true":
		return true, nil
	case s == "false":
		return false, nil
	case s[0] == '[':
		if !strings.HasSuffix(s, "]") {
			return nil, fmt.Errorf("unterminated array (amux writes arrays on one line)")
		}
		inner := strings.TrimSpace(s[1 : len(s)-1])
		if inner == "" {
			return []string{}, nil
		}
		var out []string
		for _, item := range splitTop(inner, ',') {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			v, err := unquote(item)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	default:
		n, err := strconv.Atoi(s)
		if err != nil {
			return nil, fmt.Errorf("cannot read value %q", s)
		}
		return n, nil
	}
}

// unquote reads a TOML basic string, honoring the escapes tomlString emits.
func unquote(s string) (string, error) {
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return "", fmt.Errorf("expected a quoted string, got %q", s)
	}
	var b strings.Builder
	body := s[1 : len(s)-1]
	for i := 0; i < len(body); i++ {
		ch := body[i]
		if ch != '\\' {
			b.WriteByte(ch)
			continue
		}
		i++
		if i >= len(body) {
			return "", fmt.Errorf("string ends in a backslash")
		}
		switch body[i] {
		case '"':
			b.WriteByte('"')
		case '\\':
			b.WriteByte('\\')
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		default:
			return "", fmt.Errorf("unsupported escape \\%c", body[i])
		}
	}
	return b.String(), nil
}

// tomlString quotes a value as a TOML basic string.
func tomlString(v string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range v {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// stripComment removes a trailing `#` comment, ignoring one inside a quoted
// string so a path or label containing '#' survives a round trip.
func stripComment(line string) string {
	inStr, esc := false, false
	for i := 0; i < len(line); i++ {
		switch {
		case esc:
			esc = false
		case line[i] == '\\' && inStr:
			esc = true
		case line[i] == '"':
			inStr = !inStr
		case line[i] == '#' && !inStr:
			return line[:i]
		}
	}
	return line
}

// splitTop splits on sep at the top level, ignoring separators inside quotes.
func splitTop(s string, sep byte) []string {
	var out []string
	start, inStr, esc := 0, false, false
	for i := 0; i < len(s); i++ {
		switch {
		case esc:
			esc = false
		case s[i] == '\\' && inStr:
			esc = true
		case s[i] == '"':
			inStr = !inStr
		case s[i] == sep && !inStr:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

func lineErr(i int, msg string) error { return fmt.Errorf("line %d: %s", i+1, msg) }

// NormalizeHarnesses applies the repeated --harness spelling in order. "auto"
// clears an earlier restriction; a later harness starts a new allowlist. The
// empty result means automatic discovery.
func NormalizeHarnesses(harnesses []string) []string {
	var out []string
	for _, raw := range harnesses {
		h := strings.TrimSpace(raw)
		if h == "auto" {
			out = nil
			continue
		}
		out = append(out, h)
	}
	return out
}

// ValidateExecution rejects configuration that would advertise unsupported modes.
func (c Config) ValidateExecution() error {
	if c.IdentityMode != "" && c.IdentityMode != harnessproto.IdentityMachine {
		return fmt.Errorf("amux providers support only identity-mode=machine")
	}
	for _, h := range NormalizeHarnesses(c.Harnesses) {
		if h == "" || !agent.Known(h) {
			return fmt.Errorf("unknown harness %q", h)
		}
	}
	return nil
}
