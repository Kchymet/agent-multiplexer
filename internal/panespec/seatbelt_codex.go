package panespec

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"amux/internal/codexcfg"
	"amux/internal/store"
)

// Codex 0.157 publishes the requested Unix endpoint as a symlink to a physical
// socket under /tmp/codex-daemon-<uid>. Its basename is SHA256(canonical parent +
// requested basename). TMPDIR and --no-daemon do not change this location.
// Admit only this session's two possible endpoints and their startup locks;
// granting the shared directory would expose other sessions and the host daemon.
// Protocol: codex-rs/app-server-transport/src/transport/unix_socket.rs.
func (p *seatbeltPolicy) codexSockets(s store.Session) error {
	tmp, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		return err
	}
	root := filepath.Join(tmp, fmt.Sprintf("codex-daemon-%d", os.Geteuid()))
	if err := os.Mkdir(root, 0700); err != nil && !os.IsExist(err) {
		return err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm() != 0700 || stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("Codex socket directory %s must be a real, user-owned directory with mode 0700", root)
	}
	for _, dir := range []string{tmp, root} {
		fmt.Fprintf(&p.Builder, "(allow file-read-metadata (literal %s))\n", strconv.Quote(dir))
	}
	endpoint := appServerSocketPath(s.ID)
	if err := os.MkdirAll(filepath.Dir(endpoint), 0700); err != nil {
		return err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(endpoint))
	if err != nil {
		return err
	}
	workdir, err := filepath.EvalSymlinks(s.Dir)
	if err != nil {
		return err
	}
	// Host-owned XDG path aliases are allowed. Session-writable config
	// symlinks cannot redirect a grant: hash the fixed path under the trusted
	// workdir, never a symlink target beneath it. Codex creates that parent
	// inside its sandbox if it needs the standalone listener.
	if err := validateExistingDestinationParents(workdir, ".amux/codex/app-server-control"); err != nil {
		return err
	}
	for _, rendezvous := range []string{
		filepath.Join(parent, filepath.Base(endpoint)),
		filepath.Join(codexcfg.AgentHome(workdir), "app-server-control", "app-server-control.sock"),
	} {
		sum := sha256.Sum256([]byte(rendezvous))
		physical := filepath.Join(root, fmt.Sprintf("%x", sum))
		for _, path := range []string{physical, physical + ".lock"} {
			fmt.Fprintf(&p.Builder, "(allow file-read* file-write* (literal %s))\n", strconv.Quote(path))
		}
		fmt.Fprintf(&p.Builder, "(allow network-bind (local unix-socket (literal %s)))\n", strconv.Quote(physical))
		fmt.Fprintf(&p.Builder, "(allow network-outbound (remote unix-socket (literal %s)))\n", strconv.Quote(physical))
	}
	return nil
}
