package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"amux/internal/core"
	"amux/internal/panespec"
	"amux/internal/providercfg"
)

// browserProbe is everything checkBrowser needs to know about the host, injected
// so the check runs the same on every platform under test. See probeBrowser for
// the live wiring.
type browserProbe struct {
	// shell is $BROWSER in doctor's own environment — what the daemon (and so
	// every agent pane) inherits the next time `amux` starts it.
	shell string
	// daemon is $BROWSER inside the running daemon, and daemonKnown whether it
	// could be read at all (no daemon, or no /proc). Agents inherit the daemon's
	// environment, not this shell's, so the two can disagree after an rc edit.
	daemon      string
	daemonKnown bool
	// lookup resolves a command name or path to an absolute path, "" if it is
	// not found; realPath follows symlinks (returning its input when it can't).
	lookup   func(string) string
	realPath func(string) string
	// wsl says the host is WSL; jailed that panes run inside the bwrap scope;
	// reaches whether a host path is visible from inside that scope.
	wsl     bool
	jailed  bool
	reaches func(string) bool
}

// checkBrowser reports doctor's Browser section: whether $BROWSER names a
// command that agents can actually run. Agents open links through it — `gh pr
// view --web`, Claude's OAuth login — and the failure is quiet: the tool prints
// a URL and nobody notices the browser never came up. The value is checked the
// way a pane would see it: resolved on this PATH, then tested against the
// sandbox, which hides everything under $HOME (only system roots, the WSL
// interop mounts, and the amux data tree are bound). On WSL a $BROWSER that
// points at a Windows .exe under /mnt/c is steered to wslview instead: the
// Windows path is per-machine, and wslview lives under /usr, is on every pane's
// PATH, and does the URL/path translation itself. Returns the lines to print.
func checkBrowser(p browserProbe) []string {
	var lines []string
	say := func(mark, format string, a ...any) {
		lines = append(lines, fmt.Sprintf("  %s browser   ", mark)+fmt.Sprintf(format, a...))
	}
	note := func(format string, a ...any) {
		lines = append(lines, "              "+fmt.Sprintf(format, a...))
	}
	wslview := p.lookup("wslview") != ""
	// The one-line fix for WSL: point $BROWSER at wslview, installing wslu first
	// if the host lacks it.
	wslFix := func() {
		if wslview {
			note("use wslview instead: export BROWSER=wslview")
		} else {
			note("use wslview instead: sudo apt install wslu && export BROWSER=wslview")
		}
	}

	value := strings.TrimSpace(p.shell)
	switch {
	case value == "" && p.wsl:
		say("·", "$BROWSER is unset — links agents open (gh --web, Claude's login) fall back to xdg-open, which has no Windows browser to reach")
		wslFix()
	case value == "":
		say("·", "$BROWSER is unset — links agents open (gh --web, Claude's login) use the platform opener (xdg-open / open)")
	default:
		cmd, path := resolveBrowser(value, p.lookup)
		real := path
		if path != "" && p.realPath != nil {
			if r := p.realPath(path); r != "" {
				real = r
			}
		}
		switch {
		case path == "" && p.wsl && cmd == "wslview":
			say("⚠", "$BROWSER=%s — wslview is not installed", value)
			note("install wslu: sudo apt install wslu")
		case path == "":
			say("⚠", "$BROWSER=%s — command %q not found on PATH", value, cmd)
			if p.wsl {
				wslFix()
			}
		case p.wsl && (isWindowsBinary(cmd) || isWindowsBinary(path) || isWindowsBinary(real)):
			say("⚠", "$BROWSER=%s points at a Windows binary (%s)", value, path)
			note("Windows paths differ per machine and interop is fragile inside the sandbox; wslview is on every pane's PATH and translates URLs itself")
			wslFix()
		case p.jailed && p.reaches != nil && !p.reaches(path):
			say("⚠", "$BROWSER=%s → %s is hidden inside the agent sandbox", value, path)
			note("the sandbox replaces $HOME with an empty tmpfs and mounts only the system roots (/usr, /opt, /home/linuxbrew, …)")
			note("install it system-wide, or point $BROWSER at one that is (e.g. xdg-open)")
		case p.jailed && p.reaches != nil && real != path && !p.reaches(real):
			say("⚠", "$BROWSER=%s → %s resolves to %s, which the agent sandbox does not mount", value, path, real)
			note("point $BROWSER at a browser installed under a system root (/usr, /opt, /home/linuxbrew, …), not a snap or flatpak")
		case p.jailed:
			say("✓", "$BROWSER=%s → %s · reachable from the agent sandbox", value, path)
		default:
			say("✓", "$BROWSER=%s → %s (panes run unscoped here, so anything on PATH works)", value, path)
		}
	}

	// The daemon owns the agents' environment: a $BROWSER set (or fixed) in this
	// shell after the daemon started reaches no agent until it restarts.
	if p.daemonKnown && strings.TrimSpace(p.daemon) != value {
		have := "unset"
		if d := strings.TrimSpace(p.daemon); d != "" {
			have = d
		}
		say("⚠", "the running daemon has $BROWSER %s — agents inherit its environment, not this shell's", have)
		note("restart it to pick up the new value: amux daemon restart")
	}
	return lines
}

// resolveBrowser turns a $BROWSER value into the command it names and that
// command's resolved path ("" when not found). The value may be a bare name
// (wslview), a path with spaces (/mnt/c/Program Files/…/chrome.exe), a quoted
// path, or a command line with arguments or a %s placeholder (firefox %s), so it
// is tried whole first — a spaced path resolves only that way — then as its
// first (possibly quoted) word.
func resolveBrowser(value string, lookup func(string) string) (cmd, path string) {
	if p := lookup(value); p != "" {
		return value, p
	}
	cmd = firstWord(value)
	if cmd == "" {
		return value, ""
	}
	return cmd, lookup(cmd)
}

// firstWord is the first shell word of s: a quoted span (either quote style)
// or the run up to the first whitespace.
func firstWord(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if q := s[0]; q == '"' || q == '\'' {
		if end := strings.IndexByte(s[1:], q); end >= 0 {
			return s[1 : 1+end]
		}
		return s[1:]
	}
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i]
	}
	return s
}

var windowsDrive = regexp.MustCompile(`^(/mnt/[A-Za-z]/|[A-Za-z]:\\)`)

// isWindowsBinary recognizes a $BROWSER that would run on the Windows side of
// WSL: a DrvFs path (/mnt/c/…), a Windows path (C:\…), or any .exe — including
// the bare cmd.exe / explorer.exe / powershell.exe idioms that resolve through
// the Windows PATH WSL appends.
func isWindowsBinary(s string) bool {
	return windowsDrive.MatchString(s) || strings.HasSuffix(strings.ToLower(s), ".exe")
}

// probeBrowser wires checkBrowser to the live host: this shell's $BROWSER, the
// daemon's (read from /proc when it is running), PATH resolution through
// resolveCmd, and the panespec scope rules for what a pane can see.
func probeBrowser(ctx context.Context, daemonUp bool) browserProbe {
	p := browserProbe{
		shell:  os.Getenv("BROWSER"),
		wsl:    providercfg.IsWSL(),
		jailed: panespec.Jailed(),
		reaches: func(path string) bool {
			return panespec.ScopeReaches(core.DataDir(), path)
		},
		lookup: func(cmd string) string {
			// resolveCmd's login-shell fallback splices the name into a shell
			// command line, so hand it only single words; a spaced path is an
			// absolute path and LookPath handles it directly.
			if strings.ContainsAny(cmd, " \t") {
				if p, err := exec.LookPath(cmd); err == nil {
					return p
				}
				return ""
			}
			path, _ := resolveCmd(ctx, cmd)
			return path
		},
		realPath: func(path string) string {
			if r, err := filepath.EvalSymlinks(path); err == nil {
				return r
			}
			return path
		},
	}
	if daemonUp {
		p.daemon, p.daemonKnown = daemonEnv("BROWSER")
	}
	return p
}

// daemonEnv reads one variable from the running daemon's environment via the
// pidfile and /proc (Linux only — elsewhere, or with no pidfile, it reports
// unknown rather than guessing). It is the only way to see what agents actually
// inherit, since the daemon keeps the environment of whichever shell started it.
func daemonEnv(key string) (value string, ok bool) {
	if runtime.GOOS != "linux" {
		return "", false
	}
	raw, err := os.ReadFile(core.PidPath())
	if err != nil {
		return "", false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return "", false
	}
	env, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return "", false
	}
	return lookupEnviron(env, key), true
}

// lookupEnviron finds key in a NUL-separated KEY=VALUE block (the /proc environ
// format); "" when absent.
func lookupEnviron(environ []byte, key string) string {
	for _, kv := range strings.Split(string(environ), "\x00") {
		if v, found := strings.CutPrefix(kv, key+"="); found {
			return v
		}
	}
	return ""
}
