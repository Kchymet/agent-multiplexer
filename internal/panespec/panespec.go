// Package panespec resolves what to run for one tab of an agent: the Claude agent
// process, an editor, or a shell jailed to the agent's worktree. It is shared by
// the native TUI (legacy direct-spawn) and the multiplexer server (which hands the
// spec to a harness), so pane launch behavior stays identical everywhere.
package panespec

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"amux/internal/access"
	"amux/internal/agent"
	"amux/internal/cfghome"
	"amux/internal/codexcfg"
	"amux/internal/core"
	"amux/internal/store"
	"amux/internal/wsops"
)

var (
	ErrAccessRequired       = errors.New("daemon-provisioned session access is required")
	ErrIsolationUnsupported = errors.New("protected filesystem isolation is unsupported")
)

// LaunchSpec is the complete daemon-authorized input to a pane launch. The
// session row and access grant are captured together so panespec never reopens
// the store or invents authority from an id, environment variable, or cwd.
type LaunchSpec struct {
	Session store.Session
	Access  access.SessionAccess
}

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
func Resolve(spec LaunchSpec, tab int) (dir string, env, argv []string, err error) {
	s, err := validateLaunchSpec(spec)
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
	sources := agentRepoSources(s.ID)
	if tab == TabAgent && agent.Canonical(s.Agent) == "codex" {
		argv = codexWritableRepos(argv, sources)
	}
	argv, err = scope(dir, tab, s, spec.Access, argv, sources)
	return dir, env, argv, err
}

// AppServerCommand resolves the launch spec for a Codex App Server supervising an
// agent (AGE-181): the same working dir, env, and — crucially — the same sandbox
// scope and config binds as the agent's own pane, but running `codex app-server
// --listen <endpoint>` instead of the interactive TUI. Running it through scope()
// (not a bare exec) is what preserves the session's mount/config/identity grants;
// cwd alone does not. The codex binary is taken from the agent's resolved command
// so AMUX_CODEX_BIN and PATH resolution stay identical.
//
// The returned Unix-WebSocket endpoint lives in a dedicated socket tree. Every
// pane hides that tree, then mounts only its own session's socket directory. A
// read-only mount of a sibling's socket would still permit connecting to it.
func AppServerCommand(spec LaunchSpec) (dir string, env, argv []string, endpoint string, err error) {
	s, err := validateLaunchSpec(spec)
	if err != nil {
		return "", nil, nil, "", err
	}
	dir, env, agentArgv, err := wsops.AgentCommand(s)
	if err != nil {
		return "", nil, nil, "", err
	}
	sock := appServerSocketPath(s.ID)
	if err := os.MkdirAll(filepath.Dir(sock), 0700); err != nil {
		return "", nil, nil, "", fmt.Errorf("create App Server socket directory: %w", err)
	}
	// Supervisor.Start removes stale sockets under Manager.Ensure's startup lock.
	// Resolving argv here may race with an existing launch; never unlink its socket.
	endpoint = "unix://" + sock
	inner := []string{codexBin(agentArgv), "app-server", "--listen", endpoint}
	sources := agentRepoSources(s.ID)
	inner = codexWritableRepos(inner, sources)
	argv, err = scope(dir, TabAgent, s, spec.Access, inner, sources)
	return dir, env, argv, endpoint, err
}

// Codex applies its own tool sandbox inside amux's mount namespace. This helper
// remains for non-Git writable roots, but independent session repositories need
// no override: their .git directories are already beneath the session workspace.
func codexWritableRepos(argv, sources []string) []string {
	if len(argv) == 0 || len(sources) == 0 {
		return argv
	}
	// JSON string arrays are valid TOML and safely quote spaces and path escapes.
	roots, _ := json.Marshal(sources)
	out := []string{argv[0], "-c", "sandbox_workspace_write.writable_roots=" + string(roots)}
	return append(out, argv[1:]...)
}

// The socket root stays outside every session directory. It is never mounted;
// each namespace receives only its hashed own socket directory, so current and
// future sibling sockets remain absent without enumeration or masks.
func appServerSocketRoot() string { return filepath.Join(core.DataDir(), "cx") }

func appServerSocketPath(sessionID string) string {
	// Fixed-length, filesystem-safe keys also keep Unix paths short. The root is
	// already scoped by amux's data directory, including isolated daemon instances.
	key := sha256.Sum256([]byte(sessionID))
	return filepath.Join(appServerSocketRoot(), fmt.Sprintf("%x", key[:8]), "cx.sock")
}

// AppServerEndpoint returns the endpoint AppServerCommand would choose for a
// session, without launching — for a native `--remote` attach or diagnostics.
func AppServerEndpoint(agentID string) (string, error) {
	s, err := sessionFor(agentID)
	if err != nil {
		return "", err
	}
	return "unix://" + appServerSocketPath(s.ID), nil
}

// AttachCommand resolves the launch spec for a native Codex CLI attaching to the
// supervised server/thread from a pane: `codex --remote <endpoint> resume
// <threadID>`, in the agent's sandbox scope. This is the pane path for a structured
// session — it never starts a standalone Codex runtime. threadID may be empty
// (attach without a resume, e.g. a thread not yet created).
func AttachCommand(spec LaunchSpec, endpoint, threadID string) (dir string, env, argv []string, err error) {
	s, err := validateLaunchSpec(spec)
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
	inner = codexcfg.FullscreenTUI(inner)
	argv, err = scope(dir, TabAgent, s, spec.Access, inner, agentRepoSources(s.ID))
	return dir, env, argv, err
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

// agentRepoSources no longer returns shared writable Git common directories.
// Pooled worktrees require typed read-only GitObjectMounts in LaunchSpec; that
// namespace integration is deliberately separate from this legacy []string seam.
func agentRepoSources(agentID string) []string {
	return nil
}

// systemRoots are the host trees every pane sees read-only, in bind order: the
// first two (/usr, /etc) are required, the rest are bound with -try so a system
// that lacks one (non-merged /usr, no Nix, no linuxbrew) still scopes. Anything
// a pane runs — the harness, the editor, $BROWSER — has to live under one of
// these or an exact explicit runtime/config grant; the rest of $HOME is a tmpfs
// inside the scope. ScopeReaches is the query side of the system/interop list.
var systemRoots = []string{"/usr", "/etc", "/bin", "/sbin", "/lib", "/lib64", "/opt", "/nix", "/home/linuxbrew"}

// interopRoots are the WSL2 mounts the agent pane binds so Windows interop
// (clipboard .exe helpers, path translation) works from inside the scope. They
// are -try binds, so this is a no-op off WSL.
var interopRoots = []string{"/mnt/c", "/mnt/wsl"}

// jail resolves the protected namespace implementation. Secure launches never
// silently degrade to host filesystem access: Linux, a usable HOME, and
// bubblewrap >= 0.12.0 are required. Older bubblewrap releases follow attacker-
// controlled destination symlinks during setup (GHSA-pxhw-h44j-8pfx).
func jail() (bwrap, home string, disabled bool, err error) {
	if envOr("AMUX_JAIL", "on") == "off" {
		return "", "", true, nil
	}
	if runtime.GOOS != "linux" {
		return "", "", false, fmt.Errorf("%w: %s has no supported mount/PID namespace backend", ErrIsolationUnsupported, runtime.GOOS)
	}
	bw, err := exec.LookPath("bwrap")
	if err != nil {
		return "", "", false, fmt.Errorf("%w: bubblewrap not found", ErrIsolationUnsupported)
	}
	home, err = os.UserHomeDir()
	if err != nil || home == "" {
		return "", "", false, fmt.Errorf("%w: resolve home directory", ErrIsolationUnsupported)
	}
	if err := requireBubblewrapVersion(bw); err != nil {
		return "", "", false, err
	}
	return bw, home, false, nil
}

func requireBubblewrapVersion(binary string) error {
	out, err := exec.Command(binary, "--version").Output()
	if err != nil {
		return fmt.Errorf("%w: query bubblewrap version: %v", ErrIsolationUnsupported, err)
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) == 0 {
		return fmt.Errorf("%w: unrecognized bubblewrap version", ErrIsolationUnsupported)
	}
	version := fields[len(fields)-1]
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		return fmt.Errorf("%w: unrecognized bubblewrap version %q", ErrIsolationUnsupported, version)
	}
	major, majorErr := strconv.Atoi(parts[0])
	minor, minorErr := strconv.Atoi(parts[1])
	if majorErr != nil || minorErr != nil || major < 0 || minor < 0 {
		return fmt.Errorf("%w: unrecognized bubblewrap version %q", ErrIsolationUnsupported, version)
	}
	if major == 0 && minor < 12 {
		return fmt.Errorf("%w: bubblewrap %s is vulnerable to destination symlink traversal; require >= 0.12.0", ErrIsolationUnsupported, version)
	}
	return nil
}

// Jailed reports whether this host supports the protected bwrap scope. False no
// longer means a transparent fallback: typed launches return an explicit error.
func Jailed() bool {
	_, _, disabled, err := jail()
	return !disabled && err == nil
}

// IsolationSupport reports why protected launches cannot run on this host.
// AMUX_JAIL=off is a deliberate unprotected development mode, not support.
func IsolationSupport() error {
	_, _, disabled, err := jail()
	if disabled {
		return fmt.Errorf("%w: disabled by AMUX_JAIL=off", ErrIsolationUnsupported)
	}
	return err
}

// ScopeReaches reports whether an absolute host path is visible from inside an
// agent pane's scope: under one of the read-only system roots or WSL interop
// mounts. The dataDir parameter remains for API compatibility but is never a
// visibility grant: only an exact own directory is mounted by a LaunchSpec.
// Everything else under $HOME is replaced by an empty tmpfs; exact runtime,
// session and configuration grants are not inferable from this global helper.
// Paths outside the listed roots (/var, /snap, /srv, …) are not bound at all.
// The path is checked as given; a caller
// that cares about a symlink's target (Ubuntu's /usr/bin/firefox → /snap/…)
// should resolve it and ask about both.
func ScopeReaches(_ string, path string) bool {
	path = filepath.Clean(path)
	under := func(root string) bool {
		root = filepath.Clean(root)
		return path == root || strings.HasPrefix(path, root+string(os.PathSeparator))
	}
	for _, r := range append(append([]string{}, systemRoots...), interopRoots...) {
		if under(r) {
			return true
		}
	}
	return false
}

func validateLaunchSpec(spec LaunchSpec) (store.Session, error) {
	s := spec.Session
	g := spec.Access
	if s.ID == "" || s.Dir == "" || g.SubjectID == "" {
		return store.Session{}, ErrAccessRequired
	}
	if g.SubjectID != s.ID {
		return store.Session{}, fmt.Errorf("%w: grant subject %q does not match session %q", ErrAccessRequired, g.SubjectID, s.ID)
	}
	if err := requireRealDirectory(s.Dir); err != nil {
		return store.Session{}, fmt.Errorf("session %q own directory: %w", s.ID, err)
	}
	wantMailbox := filepath.Join(s.Dir, ".amux", access.MailboxDirName)
	wantRequests := filepath.Join(wantMailbox, "requests")
	wantCredential := filepath.Join(wantMailbox, access.CredentialDirName)
	for label, pair := range map[string][2]string{
		"mailbox":    {g.MailboxMountDir, wantMailbox},
		"requests":   {g.RequestsMountDir, wantRequests},
		"credential": {g.CredentialMountDir, wantCredential},
	} {
		if !sameCleanAbsolute(pair[0], pair[1]) {
			return store.Session{}, fmt.Errorf("%w: %s target %q is not authoritative %q", ErrAccessRequired, label, pair[0], pair[1])
		}
	}
	for label, source := range map[string]string{
		"mailbox": g.MailboxHostDir, "requests": g.RequestsHostDir, "credential": g.CredentialHostDir,
	} {
		if err := requireRealDirectory(source); err != nil {
			return store.Session{}, fmt.Errorf("%w: %s source: %v", ErrAccessRequired, label, err)
		}
		if pathWithin(s.Dir, source) {
			return store.Session{}, fmt.Errorf("%w: %s source overlaps session directory", ErrAccessRequired, label)
		}
	}
	if !sameCleanAbsolute(g.RequestsHostDir, filepath.Join(g.MailboxHostDir, "requests")) {
		return store.Session{}, fmt.Errorf("%w: requests source is not the mailbox child", ErrAccessRequired)
	}
	if filepath.Clean(g.CredentialHostDir) == filepath.Clean(g.MailboxHostDir) || pathWithin(g.MailboxHostDir, g.CredentialHostDir) {
		return store.Session{}, fmt.Errorf("%w: credential source must be independent from mailbox", ErrAccessRequired)
	}
	if err := validateExistingDestinationParents(s.Dir,
		".amux",
		filepath.Join(".amux", access.MailboxDirName),
		filepath.Join(".amux", access.MailboxDirName, "requests"),
		filepath.Join(".amux", access.MailboxDirName, access.CredentialDirName),
	); err != nil {
		return store.Session{}, fmt.Errorf("session %q access destination: %w", s.ID, err)
	}
	if err := requireIndependentGit(s); err != nil {
		return store.Session{}, err
	}
	if err := IsolationSupport(); err != nil {
		return store.Session{}, err
	}
	return s, nil
}

func sameCleanAbsolute(got, want string) bool {
	return filepath.IsAbs(got) && got == filepath.Clean(got) && filepath.Clean(got) == filepath.Clean(want)
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func requireRealDirectory(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("path %q is not absolute and clean", path)
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if canonical != path {
		return fmt.Errorf("path %q contains a symlink", path)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("path %q is not a real directory", path)
	}
	return nil
}

// validateExistingDestinationParents rejects attacker-planted aliases without
// creating or rewriting anything on the host. Missing components are safe:
// bubblewrap >= 0.12 creates mount destinations relative to the fresh namespace
// root without following an attacker symlink through /oldroot.
func validateExistingDestinationParents(root string, paths ...string) error {
	for _, rel := range paths {
		path := root
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			path = filepath.Join(path, part)
			info, err := os.Lstat(path)
			if errors.Is(err, os.ErrNotExist) {
				break
			}
			if err != nil {
				return err
			}
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("%q is not a real directory", path)
			}
		}
	}
	return nil
}

// scope wraps a pane command in a fresh user, mount, and PID namespace. Only
// the authoritative own directory, exact daemon-issued access directories, the
// own App Server socket directory, and explicit runtime/account capabilities
// enter the namespace. Network remains shared for provider and Git access.
func scope(dir string, tab int, s store.Session, grant access.SessionAccess, argv []string, rwSources []string) ([]string, error) {
	if len(argv) == 0 {
		return argv, nil
	}
	if len(rwSources) != 0 {
		return nil, fmt.Errorf("shared writable repository mounts are unsupported")
	}
	bw, home, disabled, err := jail()
	if err != nil {
		return nil, err
	}
	if disabled {
		return nil, fmt.Errorf("%w: AMUX_JAIL=off cannot receive session credentials", ErrIsolationUnsupported)
	}

	args := []string{bw, "--die-with-parent", "--unshare-user", "--unshare-pid"}
	for _, name := range hostOnlyEnvironmentNames(os.Environ()) {
		args = append(args, "--unsetenv", name)
	}
	// Required core for a functional sandbox: binaries/libraries (/usr) and system
	// config (/etc — provides resolv.conf for DNS and passwd for user resolution).
	args = append(args, "--ro-bind", "/usr", "/usr", "--ro-bind", "/etc", "/etc")
	// Non-merged-/usr systems also need these as real dirs; on merged systems they
	// are symlinks already covered by /usr, so -try skips whatever's absent. /opt,
	// /nix, and /home/linuxbrew cover additional host toolchains without exposing
	// the host's runtime-service namespace under /run.
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
	// Empty $HOME, then restore only the exact runtime and authoritative own dir.
	args = append(args, "--tmpfs", home)
	// A launcher may resolve into a different home subtree (for example Codex's
	// ~/.local/bin launcher into ~/.codex/packages). Bind the resolved package or
	// executable and run it directly, without exposing the launcher subtree too.
	// Otherwise retain the normal runtime subtree bind, e.g. Claude under ~/.nvm.
	// These mounts precede the writable data/worktree/git mounts.
	launchArgv := argv
	if real, root := resolvedInstallRoot(home, argv[0]); root != "" {
		args = append(args, "--ro-bind-try", root, root)
		launchArgv = append([]string{real}, argv[1:]...)
	} else if root := directInstallRoot(home, argv[0]); root != "" {
		args = append(args, "--ro-bind-try", root, root)
	}
	args = append(args, "--bind", s.Dir, s.Dir)
	for _, b := range configBinds(tab, s, home) {
		args = append(args, b...)
	}
	// A Claude invoked from a terminal/editor inherits the same auth environment
	// as the agent. It needs the directory writable for refresh and lock creation.
	if tab != TabAgent {
		if spec, ok := agent.HarnessFor(s.Agent).Config(s); ok && spec.AuthDir != "" {
			args = append(args, "--bind", spec.AuthDir, spec.AuthDir)
		}
	}
	if s.ID != "" {
		ownSockets := filepath.Dir(appServerSocketPath(s.ID))
		// Mount the directory even before the server exists, so a terminal opened
		// first can attach when its session starts the server later.
		if err := os.MkdirAll(ownSockets, 0700); err != nil {
			return nil, fmt.Errorf("create own App Server socket directory: %w", err)
		}
		ownSocketsSource, err := filepath.EvalSymlinks(ownSockets)
		if err != nil {
			return nil, fmt.Errorf("resolve own App Server socket directory: %w", err)
		}
		if err := requireRealDirectory(ownSocketsSource); err != nil {
			return nil, fmt.Errorf("validate own App Server socket directory: %w", err)
		}
		args = append(args, "--bind", ownSocketsSource, ownSockets)
	}
	// The daemon-private parent is never mounted. Mailbox is read-only, requests
	// is its only writable overlay, and credentials are a read-only sibling source
	// mounted both at the immutable immediate-root context path and at the legacy
	// own-directory compatibility path.
	args = append(args,
		"--ro-bind", grant.CredentialHostDir, core.SessionAccessDir(),
		"--ro-bind", grant.MailboxHostDir, grant.MailboxMountDir,
		"--bind", grant.RequestsHostDir, grant.RequestsMountDir,
		"--ro-bind", grant.CredentialHostDir, grant.CredentialMountDir,
		"--chdir", dir,
	)
	args = append(args, "--")
	return append(args, launchArgv...), nil
}

var childAMUXEnvironment = map[string]bool{
	"AMUX_AGENT": true, "AMUX_MODE": true, "AMUX_ROLE": true,
	"AMUX_ROOT": true, "AMUX_SCOPE": true, "AMUX_SESSION_ID": true,
	"AMUX_WORKGROUP": true, "AMUX_WORKSPACE": true,
}

// hostOnlyEnvironmentNames strips daemon/operator authority inherited by both
// engine/local and codexapp before the child command runs. Session identity and
// harness runtime variables are supplied explicitly; host management, provider,
// TLS, alternate-routing, and ambient credential variables are never forwarded.
func hostOnlyEnvironmentNames(environ []string) []string {
	seen := map[string]bool{}
	for _, entry := range environ {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || name == "" {
			continue
		}
		hostOnly := strings.HasPrefix(name, "AMUX_") && !childAMUXEnvironment[name]
		switch name {
		case "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN",
			"CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR", "CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR",
			"OPENAI_API_KEY", "CODEX_API_KEY", "GH_TOKEN", "GITHUB_TOKEN", "SSH_AUTH_SOCK":
			hostOnly = true
		}
		if hostOnly {
			seen[name] = true
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// resolvedInstallRoot handles a launcher symlink. A nested package's bin
// directory gets its package root (sibling resources may be required); other
// layouts get only the resolved executable. The target is run directly, so no
// writable or attacker-controlled launcher ancestor is needed in the namespace.
func resolvedInstallRoot(home, p string) (real, root string) {
	r, err := filepath.EvalSymlinks(p)
	if err != nil || r == p {
		return "", ""
	}
	if homeSubtree(home, r) == "" {
		return "", ""
	}
	dir := filepath.Dir(r)
	if filepath.Base(dir) == "bin" {
		pkg := filepath.Dir(dir)
		if pkg != filepath.Clean(home) && pkg != homeSubtree(home, r) {
			return r, pkg
		}
	}
	return r, r
}

// directInstallRoot returns the narrowest useful mount for a non-symlinked
// executable under HOME. Node installed by nvm needs its selected version tree;
// ordinary user binaries are mounted as one file. In particular, ~/.local is
// never restored wholesale because it commonly contains amux data and state.
func directInstallRoot(home, p string) string {
	clean := filepath.Clean(p)
	if homeSubtree(home, clean) == "" {
		return ""
	}
	rel, err := filepath.Rel(home, clean)
	if err == nil {
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) >= 5 && parts[0] == ".nvm" && parts[1] == "versions" && parts[2] == "node" {
			return filepath.Join(home, parts[0], parts[1], parts[2], parts[3])
		}
	}
	return clean
}

// configBinds is the minimal per-tool config/state mounted into the scope so the
// tool can run: for the agent, the harness's shared auth paths (its config is a
// private copy inside the agent's dir, already writable — nothing of the user's
// ~/.claude or $CODEX_HOME is mounted); the editor's config/state for the
// editor; nothing for the shell.
func configBinds(tab int, s store.Session, home string) [][]string {
	j := filepath.Join
	switch tab {
	case TabAgent:
		// Shared auth files or a dedicated credential directory. Credentials and
		// refresh locks must not diverge per agent. Everything below is shared by
		// every agent pane regardless of harness.
		var binds [][]string
		if spec, ok := agent.HarnessFor(s.Agent).Config(s); ok {
			binds = cfghome.Binds(spec)
		}
		binds = append(binds,
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
		// /var — expose exactly the real /run/docker.sock at
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
	if err := wsops.ValidateAgentGit(s); err != nil {
		return store.Session{}, err
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
