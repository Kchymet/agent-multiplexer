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
// unmounted tree (a snap), protected WSL's deliberately absent host interop,
// and the healthy unprotected-host cases.
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
			name: "protected WSL uses attended host login", shell: "", wsl: true, jailed: true,
			symbol: "✓", want: []string{"protected WSL", "Windows-drive and interop paths are absent", "copy any printed URL into the host browser"}, absent: []string{"wslview"},
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
			name: "protected WSL ignores configured browser", shell: "chromium", wsl: true, jailed: true,
			symbol: "⚠", want: []string{"$BROWSER=chromium", "protected WSL", "host terminal"}, absent: []string{"wslview"},
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
			name: "windows path is unavailable in protected WSL", shell: "/mnt/c/Program Files/Google/Chrome/Application/chrome.exe", wsl: true, jailed: true,
			symbol: "⚠", want: []string{"cannot launch a host browser", "Windows-drive and interop paths are absent"}, absent: []string{"wslview"},
		},
		{
			name: "windows exe is unavailable in protected WSL", shell: "cmd.exe /c start", wsl: true, jailed: true,
			symbol: "⚠", want: []string{"cannot launch a host browser", "host terminal"}, absent: []string{"wslview"},
		},
		{
			name: "windows path off WSL is just hidden", shell: "cmd.exe", wsl: false, jailed: true,
			symbol: "✓", absent: []string{"wslview"},
		},
		{
			name: "wslview cannot cross protected WSL", shell: "wslview", wsl: true, jailed: true,
			symbol: "⚠", want: []string{"$BROWSER=wslview", "cannot launch a host browser", "copy any printed URL into the host browser"},
		},
		{
			name: "wslview remains valid in an unprotected host context", shell: "wslview", wsl: true, jailed: false,
			symbol: "✓", want: []string{"/usr/bin/wslview", "unscoped"},
		},
		{
			name: "unprotected WSL still steers Windows binaries to wslview", shell: "cmd.exe /c start", wsl: true, jailed: false,
			symbol: "⚠", want: []string{"Windows binary", "/mnt/c/Windows/System32/cmd.exe", "export BROWSER=wslview"},
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

	t.Run("protected macOS denies host application launch", func(t *testing.T) {
		p := host.probe("open", false, true)
		p.darwin = true
		out := strings.Join(checkBrowser(p), "\n")
		for _, want := range []string{"$BROWSER=open", "protected macOS", "LaunchServices is denied", "host terminal"} {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q:\n%s", want, out)
			}
		}
	})
}

// TestCheckBrowserWslviewMissing covers only an unprotected WSL host context;
// protected panes never recommend an interop helper they cannot use.
func TestCheckBrowserWslviewMissing(t *testing.T) {
	host := browserHost{bins: map[string]string{}}
	out := strings.Join(checkBrowser(host.probe("wslview", true, false)), "\n")
	if !strings.Contains(out, "wslview is not installed") || !strings.Contains(out, "apt install wslu") {
		t.Errorf("missing wslview not steered to wslu:\n%s", out)
	}
	// And the unset case on such a host installs before it exports.
	out = strings.Join(checkBrowser(host.probe("", true, false)), "\n")
	if !strings.Contains(out, "apt install wslu && export BROWSER=wslview") {
		t.Errorf("unset $BROWSER on a WSL host without wslu should say to install it:\n%s", out)
	}
}

// TestCheckBrowserDaemonDrift pins the environment gotcha: agents inherit the
// daemon's $BROWSER, so a value changed in this shell after the daemon started
// is flagged with the restart, and a matching daemon says nothing extra.
func TestCheckBrowserDaemonDrift(t *testing.T) {
	host := browserHost{bins: map[string]string{"wslview": "/usr/bin/wslview"}, visible: []string{"/usr"}}
	p := host.probe("wslview", true, false)
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
