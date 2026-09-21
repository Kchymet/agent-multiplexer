package panespec

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"amux/internal/access"
	"amux/internal/agent"
	"amux/internal/claudecfg"
	"amux/internal/core"
	"amux/internal/git"
	"amux/internal/launchenv"
	"amux/internal/store"
)

// Seatbelt grants access to the original host paths. Unlike Linux it cannot
// construct a private mount or PID namespace. Its default-deny policy covers
// files, Unix sockets, signals, and process inspection, and is inherited by
// every descendant. Network access to IP endpoints remains shared on both OSes.
const seatbeltBase = `(version 1)
(deny default)
(allow process-exec process-fork)
(allow signal (target same-sandbox))
(allow process-info* (target same-sandbox))
(allow file-read* (literal "/"))
(allow file-read-metadata (literal "/etc") (literal "/var") (literal "/tmp"))
(allow sysctl-read
  (sysctl-name-prefix "hw.")
  (sysctl-name "kern.ostype") (sysctl-name "kern.osrelease")
  (sysctl-name "kern.osversion") (sysctl-name "kern.osproductversion")
  (sysctl-name "kern.version") (sysctl-name "kern.hostname")
  (sysctl-name "kern.argmax") (sysctl-name "kern.maxfilesperproc")
  (sysctl-name "kern.usrstack64") (sysctl-name "machdep.cpu.brand_string")
  (sysctl-name "security.mac.lockdown_mode_state") (sysctl-name "kern.bootargs"))
(allow mach-lookup
  (global-name "com.apple.system.opendirectoryd.libinfo")
  (global-name "com.apple.system.opendirectoryd.membership")
  (global-name "com.apple.SystemConfiguration.configd")
  (global-name "com.apple.SystemConfiguration.DNSConfiguration")
  (global-name "com.apple.networkd")
  (global-name "com.apple.trustd.agent"))
(allow system-socket (socket-domain AF_INET) (socket-domain AF_INET6) (socket-domain AF_UNIX))
; Native getaddrinfo/DNSService clients use this system DNS broker even when
; outbound IP sockets are allowed. This does not admit other host Unix sockets.
(allow network-outbound (remote unix-socket (literal "/private/var/run/mDNSResponder")))
(allow network-outbound (remote ip))
(allow network-inbound network-bind (local ip))
(allow file-read* (literal "/dev/null") (literal "/dev/zero")
  (literal "/dev/random") (literal "/dev/urandom") (subpath "/dev/fd"))
(allow file-write-data (literal "/dev/null"))
(allow pseudo-tty)
(allow file-read* file-write* file-ioctl (literal "/dev/ptmx") (literal "/dev/tty"))
(allow file-read* file-write* file-ioctl
  (require-all (regex #"^/dev/ttys[0-9]+$") (extension "com.apple.sandbox.pty")))
(allow file-ioctl (regex #"^/dev/ttys[0-9]+$"))
`

// Native harnesses call LaunchServices directly as well as /usr/bin/open.
// lsopen cannot be restricted to HTTP URLs: it deliberately permits launching
// host applications outside this Seatbelt profile, including the user's browser.
// It does not grant Apple Events automation, browser profile files or keychains.
const seatbeltBrowser = `
(allow lsopen)
(allow mach-lookup
 (global-name "com.apple.coreservices.launchservicesd")
 (global-name "com.apple.CoreServices.coreservicesd")
 (global-name "com.apple.coreservices.quarantine-resolver")
 (global-name "com.apple.lsd.mapdb")
 (global-name "com.apple.lsd.open"))
(allow sysctl-read (sysctl-name "kern.willshutdown"))
`

var darwinSystemRoots = []string{
	"/System", "/usr", "/bin", "/sbin", "/opt", "/Library/Apple", "/Applications",
	"/private/etc", "/private/var/db/dyld", "/private/var/db/timezone",
	"/private/var/select", "/private/var/db/xcode_select_link",
	"/Library/Developer", "/Applications/Xcode.app/Contents",
	"/Library/Preferences/com.apple.dt.Xcode.plist",
	"/Library/Preferences/Logging", "/Library/Keychains/System.keychain",
}

// xcode-select may point to a versioned Xcode bundle, without an Xcode.app alias.
// Its command shims also load Info.plist and frameworks beside Developer.
func darwinReadRoots() []string {
	roots := append([]string(nil), darwinSystemRoots...)
	if developer, err := filepath.EvalSymlinks("/private/var/db/xcode_select_link"); err == nil && filepath.Base(developer) == "Developer" && filepath.Base(filepath.Dir(developer)) == "Contents" {
		roots = append(roots, filepath.Dir(developer))
	}
	return roots
}

func nativeTempDir(s store.Session) string {
	return filepath.Join(filepath.Dir(core.SessionBinPath(s.ID)), "tmp")
}

func platformLaunchEnv(spec LaunchSpec) []string {
	if isolationPlatform != "darwin" {
		return nil
	}
	env := []string{
		core.SessionAccessEnv + "=" + spec.Access.CredentialHostDir,
		"PATH=" + launchenv.PathWithTool(filepath.Dir(core.SessionBinPath(spec.Session.ID)), os.Getenv("PATH")),
		"TMPDIR=" + nativeTempDir(spec.Session),
		"CLAUDE_CODE_TMPDIR=" + nativeTempDir(spec.Session),
	}
	// Native certificate enumeration needs general Keychain IPC, which remains
	// denied. Codex's HTTPS and WebSocket clients support a PEM trust bundle.
	// Respect explicit host CA configuration; otherwise use macOS's public CAs.
	if os.Getenv("SSL_CERT_FILE") == "" && os.Getenv("CODEX_CA_CERTIFICATE") == "" {
		env = append(env, "SSL_CERT_FILE=/etc/ssl/cert.pem")
	}
	for _, key := range []string{"SSL_CERT_FILE", "CODEX_CA_CERTIFICATE"} {
		if path := os.Getenv(key); path != "" {
			if absolute, err := filepath.Abs(path); err == nil {
				path = absolute
			}
			env = append(env, key+"="+path)
		}
	}
	return env
}

// CodexSandboxForLaunch is passed only alongside a successfully constructed
// protected launch. macOS cannot apply nested Seatbelt profiles: amux owns the
// OS boundary, while Codex keeps its existing approval policy and reviewer.
func CodexSandboxForLaunch() string {
	if isolationPlatform == "darwin" {
		return "danger-full-access"
	}
	return ""
}

func seatbeltHarnessArgv(s store.Session, tab int, argv []string) []string {
	if tab != TabAgent || len(argv) == 0 {
		return argv
	}
	switch agent.HarnessFor(s.Agent).Kind() {
	case "claude":
		return append([]string{argv[0], "--settings", `{"sandbox":{"enabled":false}}`}, argv[1:]...)
	case "codex":
		// Remote attach inherits the already-running server's policy.
		for _, arg := range argv[1:] {
			if arg == "--remote" {
				return argv
			}
		}
		args := []string{argv[0], "-c", `sandbox_mode="danger-full-access"`}
		for i := 1; i < len(argv); i++ {
			if argv[i] == "--sandbox" && i+1 < len(argv) {
				i++
				continue
			}
			args = append(args, argv[i])
		}
		return args
	}
	return argv
}

// publishNativeTool copies the running executable into daemon-owned storage.
// No writable session path or user-installed sibling becomes a tool grant.
// Atomic replacement permits a daemon upgrade while earlier panes are alive.
func publishNativeTool(s store.Session) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	source, err := os.Open(self)
	if err != nil {
		return "", err
	}
	defer source.Close()
	dest := core.SessionBinPath(s.ID)
	parent := filepath.Dir(dest)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return "", err
	}
	if err := requireRealDirectory(parent); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(parent, ".amux-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, source); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(0500); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), dest); err != nil {
		return "", err
	}
	// Claude resolves its native Keychain helper through PATH. This protected
	// alias forwards only selected-account operations to the signed mailbox.
	alias := filepath.Join(parent, "security")
	if target, err := os.Readlink(alias); err != nil || target != "amux" {
		staged := tmp.Name() + "-security"
		if err := os.Symlink("amux", staged); err != nil {
			return "", err
		}
		defer os.Remove(staged)
		if err := os.Rename(staged, alias); err != nil {
			return "", err
		}
	}
	return dest, nil
}

type seatbeltPolicy struct {
	strings.Builder
}

// Paths are encoded as data, never interpolated into Scheme syntax. Canonical
// paths are required for policy matching; callers must not admit aliases of
// daemon authority or broaden a file grant into its containing directory.
func (p *seatbeltPolicy) grant(path string, writable bool, optional bool) error {
	return p.grantExcluding(path, writable, optional, nil)
}

func (p *seatbeltPolicy) grantExcluding(path string, writable, optional bool, excluded []string) error {
	real, err := filepath.EvalSymlinks(path)
	if optional && os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("resolve sandbox grant %q: %w", path, err)
	}
	info, err := os.Stat(real)
	if err != nil {
		return err
	}
	filter := "literal"
	if info.IsDir() {
		filter = "subpath"
	}
	ops := "file-read*"
	if writable {
		ops += " file-write*"
	}
	fmt.Fprintf(&p.Builder, "(allow %s (require-all (%s %s)", ops, filter, strconv.Quote(real))
	for _, hidden := range excluded {
		// System installations may contain a custom HOME or XDG data root. A
		// broad system read must never expose their host state or sibling sessions.
		if resolved, err := filepath.EvalSymlinks(hidden); err == nil {
			hidden = resolved
		}
		fmt.Fprintf(&p.Builder, " (require-not (subpath %s))", strconv.Quote(filepath.Clean(hidden)))
	}
	p.WriteString("))\n")
	// Directory traversal/stat is allowed without permitting enumeration or
	// contents of the otherwise hidden parents.
	for _, ancestor := range []string{filepath.Dir(real), filepath.Clean(path)} {
		for parent := ancestor; ; parent = filepath.Dir(parent) {
			fmt.Fprintf(&p.Builder, "(allow file-read-metadata (literal %s))\n", strconv.Quote(parent))
			if parent == "/" {
				break
			}
		}
	}
	return nil
}

func (p *seatbeltPolicy) sockets(path string) {
	q := strconv.Quote(path)
	fmt.Fprintf(&p.Builder, "(allow network-bind (local unix-socket (subpath %s)))\n", q)
	fmt.Fprintf(&p.Builder, "(allow network-outbound (remote unix-socket (subpath %s)))\n", q)
}

func scopeSeatbelt(dir string, tab int, s store.Session, grant access.SessionAccess, objects []git.GitObjectMount, argv []string, home string) ([]string, error) {
	tool, err := publishNativeTool(s)
	if err != nil {
		return nil, fmt.Errorf("prepare native amux tool: %w", err)
	}
	temp := nativeTempDir(s)
	ownSocketAlias := filepath.Dir(appServerSocketPath(s.ID))
	ownSockets := ownSocketAlias
	for _, path := range []string{temp, ownSockets} {
		if err := os.MkdirAll(path, 0700); err != nil {
			return nil, err
		}
	}
	if err := requireRealDirectory(temp); err != nil {
		return nil, err
	}
	ownSockets, err = filepath.EvalSymlinks(ownSockets)
	if err != nil {
		return nil, err
	}
	var policy seatbeltPolicy
	policy.WriteString(seatbeltBase)
	policy.WriteString(seatbeltBrowser)
	for _, key := range []string{"SSL_CERT_FILE", "CODEX_CA_CERTIFICATE"} {
		if path := os.Getenv(key); path != "" {
			info, err := os.Stat(path)
			if err != nil || !info.Mode().IsRegular() {
				return nil, fmt.Errorf("%s must name a readable CA certificate file", key)
			}
			if err := policy.grant(path, false, false); err != nil {
				return nil, err
			}
		}
	}
	if agent.Canonical(s.Agent) == "claude" {
		// Claude's host and sandbox instances must coordinate token refreshes.
		// Permit only the native lock paths, including creation when absent;
		// neither the host configuration tree nor the Keychain is exposed.
		root := claudecfg.CredentialDirectory()
		if real, err := filepath.EvalSymlinks(root); err == nil {
			root = real
		}
		for _, name := range []string{".oauth_refresh.lock", ".storage-write.lock"} {
			fmt.Fprintf(&policy.Builder, "(allow file-read* file-write* (subpath %s))\n", strconv.Quote(filepath.Join(root, name)))
		}
		fmt.Fprintf(&policy.Builder, "(allow file-read* file-write* (subpath %s))\n", strconv.Quote(root+".lock"))
		for path := root; ; path = filepath.Dir(path) {
			fmt.Fprintf(&policy.Builder, "(allow file-read-metadata (literal %s))\n", strconv.Quote(path))
			if filepath.Dir(path) == path {
				break
			}
		}
	}
	for _, path := range darwinReadRoots() {
		if err := policy.grantExcluding(path, false, true, []string{home, core.DataDir(), core.StateDir()}); err != nil {
			return nil, err
		}
	}
	for _, path := range []string{tool, filepath.Join(filepath.Dir(tool), "security"), grant.CredentialHostDir, grant.MailboxHostDir} {
		if err := policy.grant(path, false, false); err != nil {
			return nil, err
		}
	}
	readOnlyAgent := tab == TabAgent && agent.HarnessFor(s.Agent).Kind() == "codex" && os.Getenv("AMUX_CODEX_SANDBOX") == "read-only"
	if err := policy.grant(s.Dir, !readOnlyAgent, false); err != nil {
		return nil, err
	}
	if readOnlyAgent {
		if config, ok := agent.HarnessFor(s.Agent).Config(s); ok {
			if err := policy.grant(config.Dir, true, false); err != nil {
				return nil, err
			}
		}
	}
	for _, path := range []string{grant.RequestsHostDir, temp, ownSocketAlias} {
		if err := policy.grant(path, true, false); err != nil {
			return nil, err
		}
	}
	for _, mount := range objects {
		if err := policy.grant(mount.ObjectsHostDir, false, false); err != nil {
			return nil, err
		}
	}
	binds := configBinds(tab, s, home)
	// Shell/editor tabs use the same model account and private config as their
	// session. Grant its shared credential paths just as on the agent tab.
	if tab != TabAgent {
		binds = append(binds, configBinds(TabAgent, s, home)...)
	}
	for _, bind := range binds {
		if len(bind) != 3 {
			return nil, fmt.Errorf("invalid native config grant")
		}
		if err := policy.grant(bind[1], !strings.HasPrefix(bind[0], "--ro-"), strings.HasSuffix(bind[0], "-try")); err != nil {
			return nil, err
		}
	}
	launchArgv := argv
	if real, root := resolvedInstallRoot(home, argv[0]); root != "" {
		if err := policy.grant(root, false, false); err != nil {
			return nil, err
		}
		launchArgv = append([]string{real}, argv[1:]...)
	} else if root := directInstallRoot(home, argv[0]); root != "" {
		if err := policy.grant(root, false, false); err != nil {
			return nil, err
		}
	} else if filepath.IsAbs(argv[0]) {
		if err := policy.grant(argv[0], false, false); err != nil {
			return nil, err
		}
	}
	policy.sockets(ownSockets)
	policy.sockets(s.Dir)
	policy.sockets(temp)
	// env runs inside the sandbox: the marker is a trampoline selector, never
	// an authority grant. The trampoline closes inherited descriptors before
	// exec, on both platforms.
	args := []string{"/usr/bin/sandbox-exec", "-p", policy.String(), "/usr/bin/env", payloadExecEnv + "=1", tool, payloadExecArg}
	return append(args, launchArgv...), nil
}
