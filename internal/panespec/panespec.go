// Package panespec resolves what to run for one tab of an agent: the Claude agent
// process, an editor, or a shell jailed to the agent's worktree. It is shared by
// the native TUI (legacy direct-spawn) and the multiplexer server (which hands the
// spec to a harness), so pane launch behavior stays identical everywhere.
package panespec

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"amux/internal/agent"
	"amux/internal/cfghome"
	"amux/internal/core"
	"amux/internal/store"
	"amux/internal/wsops"
)

// Tabs an agent exposes.
const (
	TabAgent    = 0
	TabEditor   = 1
	TabTerminal = 2
)

// Resolve returns the launch spec (working dir, extra env KEY=VALUE, argv) for a
// tab of the agent: 0 the agent (Claude), 1 an editor, 2 a shell. Every pane is
// scoped to its working dir (see scope) so it can't read outside it.
//
// The Claude agent pane launches in the workspace root (where amux keeps the
// agent's .claude config and CLAUDE.md), the dir AgentCommand returns. The editor
// and terminal instead drop into the per-repo worktree subdir (AgentWorkdir), so
// the human lands directly in the repo.
func Resolve(agentID string, tab int) (dir string, env, argv []string, err error) {
	s, err := sessionFor(agentID)
	if err != nil {
		return "", nil, nil, err
	}
	// Only the agent tab runs AgentCommand: it has launch side effects (the
	// resume-vs-fresh decision can rewrite the pinned conversation id, plus trust
	// and hook installs) that must not fire from merely viewing another tab.
	switch tab {
	case TabEditor:
		dir, env, argv = wsops.AgentWorkdir(s), wsops.AgentEnv(s), []string{editorBin()}
	case TabTerminal:
		dir, env, argv = wsops.AgentWorkdir(s), wsops.AgentEnv(s), []string{shellBin()}
	default:
		dir, env, argv, err = wsops.AgentCommand(s)
		if err != nil {
			return "", nil, nil, err
		}
	}
	return dir, env, scope(dir, tab, s, argv, agentRepoSources(agentID)), nil
}

// AppServerCommand resolves the launch spec for a Codex App Server supervising an
// agent (AGE-181): the same working dir, env, and — crucially — the same sandbox
// scope and config binds as the agent's own pane, but running `codex app-server
// --listen <endpoint>` instead of the interactive TUI. Running it through scope()
// (not a bare exec) is what preserves the session's mount/config/identity grants;
// cwd alone does not. The codex binary is taken from the agent's resolved command
// so AMUX_CODEX_BIN and PATH resolution stay identical.
func AppServerCommand(agentID, endpoint string) (dir string, env, argv []string, err error) {
	s, err := sessionFor(agentID)
	if err != nil {
		return "", nil, nil, err
	}
	dir, env, agentArgv, err := wsops.AgentCommand(s)
	if err != nil {
		return "", nil, nil, err
	}
	inner := []string{codexBin(agentArgv), "app-server", "--listen", endpoint}
	return dir, env, scope(dir, TabAgent, s, inner, agentRepoSources(agentID)), nil
}

// AttachCommand resolves the launch spec for a native Codex CLI attaching to the
// supervised server/thread from a pane: `codex --remote <endpoint> resume
// <threadID>`, in the agent's sandbox scope. This is the pane path for a structured
// session — it never starts a standalone Codex runtime. threadID may be empty
// (attach without a resume, e.g. a thread not yet created).
func AttachCommand(agentID, endpoint, threadID string) (dir string, env, argv []string, err error) {
	s, err := sessionFor(agentID)
	if err != nil {
		return "", nil, nil, err
	}
	dir, env, agentArgv, err := wsops.AgentCommand(s)
	if err != nil {
		return "", nil, nil, err
	}
	inner := []string{codexBin(agentArgv), "--remote", endpoint}
	if threadID != "" {
		inner = append(inner, "resume", threadID)
	}
	return dir, env, scope(dir, TabAgent, s, inner, agentRepoSources(agentID)), nil
}

// codexBin is the resolved codex executable from an agent's command argv (argv[0]),
// so the App Server and the --remote attach launch the exact binary the agent
// would. Falls back to the bare name if the argv is somehow empty.
func codexBin(agentArgv []string) string {
	if len(agentArgv) > 0 && agentArgv[0] != "" {
		return agentArgv[0]
	}
	return "codex"
}

// agentRepoSources returns the bare-clone git dirs backing an agent's worktrees.
// They live under the read-only amux tree but must be writable so git can commit
// (it writes objects/refs/index there), so the scope re-binds them read-write.
func agentRepoSources(agentID string) []string {
	db, err := store.Open()
	if err != nil {
		return nil
	}
	defer db.Close()
	s, ok, _ := db.GetSession(agentID)
	if !ok || s.IsRoot() {
		// The console has no store row; a coordinator or repo home is a root with
		// no worktree of its own (a repo home reads its bare clone, never commits
		// to it), so none of them gets a writable clone.
		return nil
	}
	var out []string
	for _, name := range store.SplitRepos(s.Repo) {
		if r, ok, _ := db.Repo(name); ok && r.GitDir != "" {
			out = append(out, r.GitDir)
		}
	}
	return out
}

// systemRoots are the host trees every pane sees read-only, in bind order: the
// first two (/usr, /etc) are required, the rest are bound with -try so a system
// that lacks one (non-merged /usr, no Nix, no linuxbrew) still scopes. Anything
// a pane runs — the harness, the editor, $BROWSER — has to live under one of
// these, under the amux data tree, or under the pane binary's own $HOME subtree;
// the rest of $HOME is a tmpfs inside the scope. ScopeReaches is the query side
// of this list, so doctor can tell a hidden binary apart from a missing one.
var systemRoots = []string{"/usr", "/etc", "/bin", "/sbin", "/lib", "/lib64", "/opt", "/nix", "/home/linuxbrew", "/run"}

// interopRoots are the WSL2 mounts the agent pane binds so Windows interop
// (clipboard .exe helpers, path translation) works from inside the scope. They
// are -try binds, so this is a no-op off WSL.
var interopRoots = []string{"/mnt/c", "/mnt/wsl"}

// jail resolves what scope needs to build a sandbox — the bwrap binary and the
// home dir it hides — and reports false when panes run unscoped: AMUX_JAIL=off,
// no bwrap on this host (macOS), or no resolvable $HOME.
func jail() (bwrap, home string, ok bool) {
	if envOr("AMUX_JAIL", "on") == "off" {
		return "", "", false
	}
	bw, err := exec.LookPath("bwrap")
	if err != nil {
		return "", "", false
	}
	home, err = os.UserHomeDir()
	if err != nil || home == "" {
		return "", "", false
	}
	return bw, home, true
}

// Jailed reports whether panes on this host run inside the bwrap scope. False
// means every pane sees the host filesystem as-is (AMUX_JAIL=off, or no bwrap),
// so a visibility question like ScopeReaches has no bearing.
func Jailed() bool {
	_, _, ok := jail()
	return ok
}

// ScopeReaches reports whether an absolute host path is visible from inside an
// agent pane's scope: under one of the read-only system roots, the WSL interop
// mounts, or the amux data tree (dataDir, a parameter so the rule is testable).
// Everything else under $HOME is replaced by an empty tmpfs (only the pane
// binary's own subtree and the agent's dir come back, neither of which a caller
// should rely on for a third tool), and paths outside the listed roots (/var,
// /snap, /srv, …) are not bound at all. The path is checked as given; a caller
// that cares about a symlink's target (Ubuntu's /usr/bin/firefox → /snap/…)
// should resolve it and ask about both.
func ScopeReaches(dataDir, path string) bool {
	path = filepath.Clean(path)
	under := func(root string) bool {
		root = filepath.Clean(root)
		return path == root || strings.HasPrefix(path, root+string(os.PathSeparator))
	}
	if dataDir != "" && under(dataDir) {
		return true
	}
	for _, r := range append(append([]string{}, systemRoots...), interopRoots...) {
		if under(r) {
			return true
		}
	}
	return false
}

// scope wraps a pane's command in a bubblewrap mount namespace confined to the
// worktree: the system is read-only (so tools/libraries run), only the worktree
// (and a private /tmp) is writable, and the rest of $HOME — other repos, other
// agents' worktrees, the store, your files — is replaced by an empty tmpfs. Only
// what the tool itself needs is bound back: the agent gets its own runtime and
// the one shared auth file (its harness config is a private copy already inside
// its dir — see agent.Harness.Config); the editor gets its config; the shell gets
// nothing. This is a filesystem scope, not a hardened jail (network and pids are
// shared), and it's skipped if AMUX_JAIL=off or bwrap is missing.
func scope(dir string, tab int, s store.Session, argv []string, rwSources []string) []string {
	if len(argv) == 0 {
		return argv
	}
	bw, home, ok := jail()
	if !ok {
		return argv
	}

	args := []string{bw, "--die-with-parent", "--unshare-user"}
	// Required core for a functional sandbox: binaries/libraries (/usr) and system
	// config (/etc — provides resolv.conf for DNS and passwd for user resolution).
	args = append(args, "--ro-bind", "/usr", "/usr", "--ro-bind", "/etc", "/etc")
	// Non-merged-/usr systems also need these as real dirs; on merged systems they
	// are symlinks already covered by /usr, so -try skips whatever's absent. /opt,
	// /nix, /home/linuxbrew (brew prefix), and /run cover this host's toolchain.
	for _, p := range systemRoots[2:] {
		args = append(args, "--ro-bind-try", p, p)
	}
	// Network is shared (not unshared), but DNS needs the *real* resolv.conf: on
	// WSL2 /etc/resolv.conf is a symlink to /mnt/wsl/... which the binds above
	// don't reach. Bind the symlink target at its own path so /etc/resolv.conf
	// (already present via the /etc bind) resolves through it.
	if real, err := filepath.EvalSymlinks("/etc/resolv.conf"); err == nil && real != "/etc/resolv.conf" {
		args = append(args, "--ro-bind-try", real, real)
	}
	args = append(args, "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp")
	// Empty $HOME, then add back the amux data tree read-only (the worktrees are
	// sourced from here — each worktree's .git points back to a bare clone under
	// ~/.local/share/amux/repos, so git needs to read it), and finally the agent's
	// own worktree read-write on top so it can edit its files.
	args = append(args, "--tmpfs", home)
	// The pane binary itself, if it lives under $HOME (e.g. claude under ~/.nvm),
	// would be hidden by the tmpfs — bind its subtree read-only so it still runs.
	// Mount it BEFORE the data/worktree/git mounts: a native Claude install under
	// ~/.local otherwise hides those writable mounts behind a read-only parent.
	if sub := homeSubtree(home, argv[0]); sub != "" {
		args = append(args, "--ro-bind-try", sub, sub)
	}
	args = append(args, "--ro-bind-try", core.DataDir(), core.DataDir())
	args = append(args, "--bind", dir, dir)
	// The agent's own bare clones, read-write, so git can commit to its branch.
	for _, src := range rwSources {
		args = append(args, "--bind-try", src, src)
	}
	args = append(args, "--chdir", dir)
	for _, b := range configBinds(tab, s, home) {
		args = append(args, b...)
	}
	args = append(args, "--")
	return append(args, argv...)
}

// configBinds is the minimal per-tool config/state mounted into the scope so the
// tool can run: for the agent, the harness's shared auth file (its config is a
// private copy inside the agent's dir, already writable — nothing of the user's
// ~/.claude or $CODEX_HOME is mounted); the editor's config/state for the
// editor; nothing for the shell.
func configBinds(tab int, s store.Session, home string) [][]string {
	j := filepath.Join
	switch tab {
	case TabAgent:
		// amux's hooks/gap-fill run inside the scope and must reach amux's state
		// dirs: the hook-state dir (activity) and the transcript-capture dir (a
		// durable copy of the conversation for the "restarting" diagnostic).
		// --bind-try skips missing paths, so create the capture dir first.
		_ = os.MkdirAll(core.TranscriptDir(), 0o755)
		// The harness's shared auth file, bound at its template path — the one
		// thing the agent's private config copy links back to (its OAuth credential
		// must not diverge per agent). Everything below is shared by every agent
		// pane regardless of harness.
		var binds [][]string
		if spec, ok := agent.HarnessFor(s.Agent).Config(s); ok {
			binds = cfghome.Binds(spec)
		}
		binds = append(binds,
			[]string{"--bind-try", core.HookStateDir(), core.HookStateDir()},
			[]string{"--bind-try", core.TranscriptDir(), core.TranscriptDir()},
			[]string{"--ro-bind-try", core.InstalledBinPath(), core.InstalledBinPath()},
		)
		if exe, err := os.Executable(); err == nil {
			binds = append(binds, []string{"--ro-bind-try", exe, exe})
		}
		// On WSL2, Claude reaches the Windows clipboard (e.g. pasting an image) by
		// invoking a Windows .exe via interop; those live under /mnt/c, and the
		// launcher path-translates through the DrvFs mount. Without /mnt/c in the
		// scope the .exe can't be found and the read fails ("can't find image on
		// clipboard"). Bind it read-only; /mnt/wsl backs some interop helpers too.
		// --ro-bind-try is a no-op off WSL, so this stays cross-platform.
		for _, p := range interopRoots {
			binds = append(binds, []string{"--ro-bind-try", p, p})
		}
		return append(binds, gitBinds(home)...)
	case TabEditor:
		name := filepath.Base(editorBin())
		return [][]string{
			{"--ro-bind-try", j(home, ".config", name), j(home, ".config", name)},
			{"--bind-try", j(home, ".local/share", name), j(home, ".local/share", name)},
			{"--bind-try", j(home, ".local/state", name), j(home, ".local/state", name)},
			{"--bind-try", j(home, ".cache", name), j(home, ".cache", name)},
			{"--ro-bind-try", j(home, "."+name), j(home, "."+name)},
			{"--ro-bind-try", j(home, "."+name+"rc"), j(home, "."+name+"rc")},
		}
	case TabTerminal:
		// The user's shell config (read-only) so the terminal picks up their
		// prompt theme, aliases, plugins (e.g. oh-my-zsh) — without exposing the
		// rest of $HOME. Frameworks/plugins are sourced from these or from system
		// dirs already bound read-only (e.g. /home/linuxbrew).
		var binds [][]string
		for _, p := range []string{
			".zshrc", ".zshenv", ".zprofile", ".zlogin", ".zlogout",
			".oh-my-zsh", ".p10k.zsh", ".zsh", ".config/zsh", ".fzf.zsh", ".fzf",
			".bashrc", ".bash_profile", ".bash_login", ".profile", ".bash_aliases", ".inputrc",
		} {
			binds = append(binds, []string{"--ro-bind-try", j(home, p), j(home, p)})
		}
		// History, writable so the shell can append to it.
		binds = append(binds, []string{"--bind-try", j(home, ".zsh_history"), j(home, ".zsh_history")})
		binds = append(binds, []string{"--bind-try", j(home, ".bash_history"), j(home, ".bash_history")})
		// Docker, in the terminal only (the human shell), not the agent pane. On
		// WSL2 the CLI is a symlink into /mnt/wsl (Docker Desktop); bind that so it
		// resolves. The CLI defaults to /var/run/docker.sock, but the scope has no
		// /var — the real socket is /run/docker.sock (bound), so re-expose it at
		// the default path. NB: docker reaches the host daemon, bypassing the
		// worktree scope — kept off the agent pane on purpose.
		binds = append(binds, []string{"--ro-bind-try", "/mnt/wsl", "/mnt/wsl"})
		binds = append(binds, []string{"--ro-bind-try", "/run/docker.sock", "/var/run/docker.sock"})
		return append(binds, gitBinds(home)...)
	}
	return nil
}

// gitBinds mounts the user's git + GitHub-CLI auth read-only so agents inherit
// the host's authentication instead of each one having to log in: ~/.gitconfig
// (identity + the `gh auth git-credential` helper for HTTPS) and ~/.config/gh
// (the gh token in hosts.yml). The gh binary itself is already on the read-only
// system path. NB: this hands the agent your GitHub token — it can act on GitHub
// as you (push, open PRs, etc.), which is the point.
func gitBinds(home string) [][]string {
	j := filepath.Join
	return [][]string{
		{"--ro-bind-try", j(home, ".gitconfig"), j(home, ".gitconfig")},
		{"--ro-bind-try", j(home, ".config", "git"), j(home, ".config", "git")},
		{"--ro-bind-try", j(home, ".config", "gh"), j(home, ".config", "gh")},
	}
}

// homeSubtree returns home/<first component> if p is under home, else "". Used to
// bind a pane binary's tree (e.g. ~/.nvm for claude) back through the tmpfs.
func homeSubtree(home, p string) string {
	rel, err := filepath.Rel(home, p)
	if err != nil {
		return ""
	}
	parts := strings.Split(rel, string(os.PathSeparator))
	if len(parts) == 0 || parts[0] == "" || parts[0] == ".." {
		return ""
	}
	return filepath.Join(home, parts[0])
}

// sessionFor resolves the session for an id: an agent, or one of the built-in
// default sessions — the console (synthetic, not in the store), a workgroup's
// coordinator (its root row), or a repo's home (created on first open for a repo
// tracked before default sessions). Every pane of a session — the agent process,
// the editor, the shell — derives its launch dir and argv from this one record.
func sessionFor(id string) (store.Session, error) {
	s, ok, err := wsops.ResolveSession(id)
	if err != nil {
		return store.Session{}, err
	}
	if !ok {
		return store.Session{}, fmt.Errorf("no such agent %q", id)
	}
	return s, nil
}

// EditorBin is the configured editor, defaulting to nvim.
func editorBin() string { return envOr("AMUX_EDITOR", "nvim") }

// shellBin is the user's shell, defaulting to a sane fallback.
func shellBin() string { return envOr("SHELL", "/bin/bash") }

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
