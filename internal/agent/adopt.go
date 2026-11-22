package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"amux/internal/claudecfg"
	"amux/internal/codexcfg"
	"amux/internal/core"
	"amux/internal/harnessbin"
)

// adoptSeams are replaced by tests.
var (
	adoptLookPath = func(ctx context.Context, kind string) string {
		p := resolveContext(ctx, kind)
		if !filepath.IsAbs(p) || !executableFile(p) {
			return ""
		}
		// A managed install is not the user's own copy.
		if _, managed := harnessbin.ManagedVersionRoot(p); managed {
			return ""
		}
		return p
	}
	adoptVersion  = binaryVersion
	adoptKeychain = claudeKeychainItem
)

// AdoptInput gathers what harnessbin's adoption rules need for kind on this
// host: explicit executable overrides, the user's own binary (PATH, login
// shell, well-known install dirs) and its version, and whether the user is
// signed in to the harness.
func AdoptInput(ctx context.Context, kind string) harnessbin.AdoptInput {
	kind = Canonical(kind)
	in := harnessbin.AdoptInput{
		EnvBin:    os.Getenv("AMUX_" + strings.ToUpper(kind) + "_BIN"),
		ConfigBin: configuredBin(kind),
	}
	if in.EnvBin != "" || in.ConfigBin != "" {
		return in // decided already; don't spend a version probe
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if p := adoptLookPath(ctx, kind); p != "" {
		in.OnPath = p
		in.PathVersion = adoptVersion(ctx, p)
	}
	in.LoggedIn = loggedIn(ctx, kind)
	return in
}

// loggedIn names the credential showing the user signed in to kind, or "".
func loggedIn(ctx context.Context, kind string) string {
	var files []string
	switch kind {
	case "claude":
		files = []string{
			filepath.Join(claudecfg.User().Dir, ".credentials.json"),
			filepath.Join(core.AuthDir(), "claude", ".credentials.json"),
		}
	case "codex":
		files = []string{
			filepath.Join(string(codexcfg.UserHome()), "auth.json"),
			filepath.Join(core.AuthDir(), "codex", "auth.json"),
		}
	default:
		return ""
	}
	for _, f := range files {
		if fi, err := os.Stat(f); err == nil && fi.Mode().IsRegular() && fi.Size() > 0 {
			return f
		}
	}
	if kind == "claude" && adoptKeychain(ctx) {
		return "macOS keychain (Claude Code-credentials)"
	}
	return ""
}

// claudeKeychainItem reports whether Claude Code's macOS keychain item exists.
// find-generic-password without -w/-g reads only the item's attributes, so it
// never prompts for or reveals the secret.
func claudeKeychainItem(ctx context.Context) bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	cmd := exec.CommandContext(ctx, "/usr/bin/security", "find-generic-password", "-s", "Claude Code-credentials")
	cmd.WaitDelay = 100 * time.Millisecond
	return cmd.Run() == nil
}

var semverRE = regexp.MustCompile(`[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?`)

// binaryVersion runs `<path> --version` (self-updater off) and returns the
// first version-looking token, or "".
func binaryVersion(ctx context.Context, path string) string {
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Env = append(os.Environ(), "DISABLE_AUTOUPDATER=1")
	cmd.WaitDelay = 100 * time.Millisecond
	var out probeOutput
	cmd.Stdout = &out
	if cmd.Run() != nil {
		return ""
	}
	return semverRE.FindString(out.String())
}

func executableFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0
}
