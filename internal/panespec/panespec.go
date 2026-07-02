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

	"amux/internal/codexcfg"
	"amux/internal/console"
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
	return dir, env, scope(dir, tab, s.Agent, argv, agentRepoSources(agentID)), nil
}

// agentRepoSources returns the bare-clone git dirs backing an agent's worktrees.
// They live under the read-only amux tree but must be writable so git can commit
// (it writes objects/refs/index there), so the scope re-binds them read-write.
func agentRepoSources(agentID string) []string {
	if agentID == console.ID {
		return nil
	}
	db, err := store.Open()
	if err != nil {
		return nil
	}
	defer db.Close()
	s, ok, _ := db.GetSession(agentID)
	if !ok {
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

// scope wraps a pane's command in a bubblewrap mount namespace confined to the
// worktree: the system is read-only (so tools/libraries run), only the worktree
// (and a private /tmp) is writable, and the rest of $HOME — other repos, other
// agents' worktrees, the store, your files — is replaced by an empty tmpfs. Only
// what the tool itself needs is bound back: the agent gets its Claude config/auth
// and its own runtime; the editor gets its config; the shell gets nothing. This
// is a filesystem scope, not a hardened jail (network and pids are shared), and
// it's skipped if AMUX_JAIL=off or bwrap is missing.
func scope(dir string, tab int, agentKind string, argv []string, rwSources []string) []string {
	if len(argv) == 0 || envOr("AMUX_JAIL", "on") == "off" {
		return argv
	}
	bw, err := exec.LookPath("bwrap")
	if err != nil {
		return argv
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return argv
	}

	args := []string{bw, "--die-with-parent", "--unshare-user"}
	// Required core for a functional sandbox: binaries/libraries (/usr) and system
	// config (/etc — provides resolv.conf for DNS and passwd for user resolution).
	args = append(args, "--ro-bind", "/usr", "/usr", "--ro-bind", "/etc", "/etc")
	// Non-merged-/usr systems also need these as real dirs; on merged systems they
	// are symlinks already covered by /usr, so -try skips whatever's absent. /opt,
	// /nix, /home/linuxbrew (brew prefix), and /run cover this host's toolchain.
	for _, p := range []string{"/bin", "/sbin", "/lib", "/lib64", "/opt", "/nix", "/home/linuxbrew", "/run"} {
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
	args = append(args, "--ro-bind-try", core.DataDir(), core.DataDir())
	args = append(args, "--bind", dir, dir)
	// The agent's own bare clones, read-write, so git can commit to its branch.
	for _, src := range rwSources {
		args = append(args, "--bind-try", src, src)
	}
	args = append(args, "--chdir", dir)
	// The pane binary itself, if it lives under $HOME (e.g. claude under ~/.nvm),
	// would be hidden by the tmpfs — bind its subtree read-only so it still runs.
	if sub := homeSubtree(home, argv[0]); sub != "" {
		args = append(args, "--ro-bind-try", sub, sub)
	}
	for _, b := range configBinds(tab, agentKind, home) {
		args = append(args, b...)
	}
	args = append(args, "--")
	return append(args, argv...)
}

// configBinds is the minimal per-tool config/state mounted into the scope so the
// tool can run: the agent harness's own config/auth/state (writable — each stores
// its transcripts there) for the agent, the editor's config/state for the editor,
// nothing for the shell. The agent binds depend on which harness runs: Claude
// keeps its state in ~/.claude(.json), Codex under $CODEX_HOME.
func configBinds(tab int, agentKind string, home string) [][]string {
	j := filepath.Join
	switch tab {
	case TabAgent:
		// amux's hooks/gap-fill run inside the scope and must reach amux's state
		// dirs: the hook-state dir (activity) and the transcript-capture dir (a
		// durable copy of the conversation for the "restarting" diagnostic).
		// --bind-try skips missing paths, so create the capture dir first.
		_ = os.MkdirAll(core.TranscriptDir(), 0o755)
		var binds [][]string
		if agentKind == "codex" {
			// Codex keeps auth (auth.json), config (config.toml), and its rollout
			// transcripts under $CODEX_HOME (default ~/.codex), and writes rollouts
			// there mid-session — so bind the whole tree writable. It lives under the
			// tmpfs'd $HOME, so create it first or the writes land on ephemeral tmpfs.
			ch := codexcfg.Home()
			_ = os.MkdirAll(ch, 0o755)
			binds = append(binds, []string{"--bind-try", ch, ch})
		} else {
			// Claude's config/auth, writable — it stores transcripts under ~/.claude.
			binds = append(binds,
				[]string{"--bind-try", j(home, ".claude.json"), j(home, ".claude.json")},
				[]string{"--bind-try", j(home, ".claude"), j(home, ".claude")},
			)
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
		binds = append(binds,
			[]string{"--ro-bind-try", "/mnt/c", "/mnt/c"},
			[]string{"--ro-bind-try", "/mnt/wsl", "/mnt/wsl"},
		)
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

// sessionFor resolves the session for an agent id, handling the built-in console
// (synthetic, not in the store). Every pane of an agent — the Claude process, the
// editor, the shell — derives its launch dir and argv from this one session.
func sessionFor(id string) (store.Session, error) {
	if id == console.ID {
		if err := console.Ensure(); err != nil {
			return store.Session{}, err
		}
		return console.Session(), nil
	}
	db, err := store.Open()
	if err != nil {
		return store.Session{}, err
	}
	defer db.Close()
	s, ok, err := db.GetSession(id)
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
