package main

import (
	"strings"
	"testing"
)

// browserHost is a canned host for checkBrowser: bins maps a command name or
// path to its resolved path; links maps a resolved path to its symlink target;
// visible lists what the sandbox mounts.
type browserHost struct {
	bins    map[string]string
	links   map[string]string
	visible []string
}

func (h browserHost) probe(shell string, wsl, jailed bool) browserProbe {
	return browserProbe{
		shell:  shell,
		wsl:    wsl,
		jailed: jailed,
		lookup: func(c string) string { return h.bins[c] },
		realPath: func(p string) string {
			if r, ok := h.links[p]; ok {
				return r
			}
			return p
		},
		reaches: func(p string) bool {
			for _, v := range h.visible {
				if p == v || strings.HasPrefix(p, v+"/") {
					return true
				}
			}
			return false
		},
	}
}

// TestCheckBrowser drives the $BROWSER check across the matrix a pane can
// hit: unset (WSL and not), a command that is missing, one that resolves on
// this PATH but is hidden by the sandbox's tmpfs $HOME, a symlink into an
// unmounted tree (a snap), a Windows .exe on WSL (which must be steered to
// wslview), and the healthy cases with the jail on and off.
func TestCheckBrowser(t *testing.T) {
	host := browserHost{
		bins: map[string]string{
			"wslview":  "/usr/bin/wslview",
			"xdg-open": "/usr/bin/xdg-open",
			"firefox":  "/usr/bin/firefox",
			"/mnt/c/Program Files/Google/Chrome/Application/chrome.exe": "/mnt/c/Program Files/Google/Chrome/Application/chrome.exe",
			"cmd.exe":  "/mnt/c/Windows/System32/cmd.exe",
			"mybrowse": "/home/tester/.local/bin/mybrowse",
		},
		links:   map[string]string{"/usr/bin/firefox": "/snap/bin/firefox"},
		visible: []string{"/usr", "/opt", "/mnt/c", "/mnt/wsl"},
	}
	for _, tc := range []struct {
		name   string
		shell  string
		wsl    bool
		jailed bool
		symbol string   // marker of the first line
		want   []string // substrings the output must contain
		absent []string // substrings it must not
	}{
		{
			name: "unset on WSL steers to wslview", shell: "", wsl: true, jailed: true,
			symbol: "·", want: []string{"$BROWSER is unset", "export BROWSER=wslview"},
		},
		{
			name: "unset elsewhere is a hint", shell: "", wsl: false, jailed: true,
			symbol: "·", want: []string{"$BROWSER is unset", "xdg-open"}, absent: []string{"wslview"},
		},
		{
			name: "missing command", shell: "chromium", wsl: false, jailed: true,
			symbol: "⚠", want: []string{`"chromium" not found`}, absent: []string{"wslview"},
		},
		{
			name: "missing command on WSL suggests wslview", shell: "chromium", wsl: true, jailed: true,
			symbol: "⚠", want: []string{`"chromium" not found`, "export BROWSER=wslview"},
		},
		{
			name: "hidden by the sandbox", shell: "mybrowse", wsl: false, jailed: true,
			symbol: "⚠", want: []string{"/home/tester/.local/bin/mybrowse", "hidden inside the agent sandbox", "tmpfs"},
		},
		{
			name: "hidden but jail off passes", shell: "mybrowse", wsl: false, jailed: false,
			symbol: "✓", want: []string{"unscoped"},
		},
		{
			name: "symlink into a snap", shell: "firefox", wsl: false, jailed: true,
			symbol: "⚠", want: []string{"/usr/bin/firefox", "resolves to /snap/bin/firefox"},
		},
		{
			name: "windows path with spaces on WSL", shell: "/mnt/c/Program Files/Google/Chrome/Application/chrome.exe", wsl: true, jailed: true,
			symbol: "⚠", want: []string{"Windows binary", "export BROWSER=wslview"},
		},
		{
			name: "windows exe on PATH on WSL", shell: "cmd.exe /c start", wsl: true, jailed: true,
			symbol: "⚠", want: []string{"Windows binary", "/mnt/c/Windows/System32/cmd.exe", "export BROWSER=wslview"},
		},
		{
			name: "windows path off WSL is just hidden", shell: "cmd.exe", wsl: false, jailed: true,
			symbol: "✓", absent: []string{"wslview"},
		},
		{
			name: "wslview healthy on WSL", shell: "wslview", wsl: true, jailed: true,
			symbol: "✓", want: []string{"/usr/bin/wslview", "reachable from the agent sandbox"},
		},
		{
			name: "command line with placeholder resolves its first word", shell: "xdg-open %s", wsl: false, jailed: true,
			symbol: "✓", want: []string{"/usr/bin/xdg-open"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines := checkBrowser(host.probe(tc.shell, tc.wsl, tc.jailed))
			if len(lines) == 0 {
				t.Fatal("expected output, got none")
			}
			if !strings.Contains(lines[0], tc.symbol) {
				t.Errorf("first line = %q, want marker %q", lines[0], tc.symbol)
			}
			joined := strings.Join(lines, "\n")
			for _, w := range tc.want {
				if !strings.Contains(joined, w) {
					t.Errorf("output missing %q:\n%s", w, joined)
				}
			}
			for _, a := range tc.absent {
				if strings.Contains(joined, a) {
					t.Errorf("output should not mention %q:\n%s", a, joined)
				}
			}
		})
	}
}

// TestCheckBrowserWslviewMissing: on WSL, $BROWSER=wslview with wslu not
// installed must name the package, not just report a missing command.
func TestCheckBrowserWslviewMissing(t *testing.T) {
	host := browserHost{bins: map[string]string{}}
	out := strings.Join(checkBrowser(host.probe("wslview", true, true)), "\n")
	if !strings.Contains(out, "wslview is not installed") || !strings.Contains(out, "apt install wslu") {
		t.Errorf("missing wslview not steered to wslu:\n%s", out)
	}
	// And the unset case on such a host installs before it exports.
	out = strings.Join(checkBrowser(host.probe("", true, true)), "\n")
	if !strings.Contains(out, "apt install wslu && export BROWSER=wslview") {
		t.Errorf("unset $BROWSER on a WSL host without wslu should say to install it:\n%s", out)
	}
}

// TestCheckBrowserDaemonDrift pins the environment gotcha: agents inherit the
// daemon's $BROWSER, so a value changed in this shell after the daemon started
// is flagged with the restart, and a matching daemon says nothing extra.
func TestCheckBrowserDaemonDrift(t *testing.T) {
	host := browserHost{bins: map[string]string{"wslview": "/usr/bin/wslview"}, visible: []string{"/usr"}}
	p := host.probe("wslview", true, true)
	p.daemonKnown, p.daemon = true, ""
	out := strings.Join(checkBrowser(p), "\n")
	if !strings.Contains(out, "running daemon has $BROWSER unset") || !strings.Contains(out, "amux daemon restart") {
		t.Errorf("daemon drift not flagged:\n%s", out)
	}
	p.daemon = "wslview"
	if out := strings.Join(checkBrowser(p), "\n"); strings.Contains(out, "running daemon") {
		t.Errorf("matching daemon env flagged as drift:\n%s", out)
	}
	p.daemonKnown = false
	p.daemon = "other"
	if out := strings.Join(checkBrowser(p), "\n"); strings.Contains(out, "running daemon") {
		t.Errorf("unknown daemon env must not be reported:\n%s", out)
	}
}

// TestResolveBrowser covers the shapes $BROWSER takes: a bare name, a spaced
// absolute path (tried whole), a quoted path, and a command line whose first
// word is the browser.
func TestResolveBrowser(t *testing.T) {
	lookup := func(c string) string {
		switch c {
		case "wslview":
			return "/usr/bin/wslview"
		case "/mnt/c/Program Files/x/chrome.exe":
			return c
		}
		return ""
	}
	for _, tc := range []struct{ in, cmd, path string }{
		{"wslview", "wslview", "/usr/bin/wslview"},
		{"wslview %s", "wslview", "/usr/bin/wslview"},
		{"/mnt/c/Program Files/x/chrome.exe", "/mnt/c/Program Files/x/chrome.exe", "/mnt/c/Program Files/x/chrome.exe"},
		{`"/mnt/c/Program Files/x/chrome.exe" --new-window`, "/mnt/c/Program Files/x/chrome.exe", "/mnt/c/Program Files/x/chrome.exe"},
		{`'wslview' -u`, "wslview", "/usr/bin/wslview"},
		{"nope --flag", "nope", ""},
	} {
		cmd, path := resolveBrowser(tc.in, lookup)
		if cmd != tc.cmd || path != tc.path {
			t.Errorf("resolveBrowser(%q) = (%q, %q), want (%q, %q)", tc.in, cmd, path, tc.cmd, tc.path)
		}
	}
}

func TestLookupEnviron(t *testing.T) {
	env := []byte("HOME=/home/t\x00BROWSER=wslview\x00BROWSERX=no\x00")
	if got := lookupEnviron(env, "BROWSER"); got != "wslview" {
		t.Errorf("BROWSER = %q, want wslview", got)
	}
	if got := lookupEnviron(env, "PATH"); got != "" {
		t.Errorf("absent key = %q, want empty", got)
	}
}
