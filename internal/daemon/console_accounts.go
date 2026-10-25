package daemon

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"amux/internal/access"
	"amux/internal/console"
	"amux/internal/core"
	"amux/internal/store"
)

func (d *Daemon) authorizeConsoleAccounts(ctx context.Context, p access.Principal) error {
	if p.Kind != access.SubjectSession || p.SubjectID != console.ID {
		return access.ErrDenied
	}
	if err := d.authority.Valid(ctx, p); err != nil {
		return err
	}
	row, ok, err := newDaemonAccessResolver().Lookup(ctx, p.SubjectID)
	if err != nil || !ok || row.Archived || row.Role != string(store.RoleConsole) {
		return access.ErrDenied
	}
	return nil
}

// The socket inode lives in the daemon-owned, read-only-mounted mailbox root.
// Only the transport is shared: no request, response, token or URL is a file.
func (d *Daemon) listenConsoleAccounts(ctx context.Context, mailbox string) (func(), error) {
	stable := filepath.Join(mailbox, access.ConsoleAccountSocketName)
	alias, removeAlias, err := consoleSocketAlias(stable)
	if err != nil {
		return nil, err
	}
	// Unlike the shared host endpoint, this path is inside a daemon-owned
	// directory that the console only sees read-only. Recreate the exact name
	// so an already-running Seatbelt console can reconnect after daemon restart.
	if info, statErr := os.Lstat(stable); statErr == nil {
		if info.Mode()&os.ModeSocket == 0 {
			removeAlias()
			return nil, fmt.Errorf("unexpected console account endpoint")
		}
		if err := os.Remove(stable); err != nil {
			removeAlias()
			return nil, err
		}
	} else if !os.IsNotExist(statErr) {
		removeAlias()
		return nil, statErr
	}
	listener, err := net.Listen("unix", alias)
	if err != nil {
		removeAlias()
		return nil, err
	}
	cleanup := func() { _ = listener.Close() }
	lifetime, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	slots := make(chan struct{}, 16)
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			select {
			case slots <- struct{}{}:
			default:
				_ = conn.Close()
				continue
			}
			d.startServingHandler(lifetime, conn, func(ctx context.Context, c net.Conn) {
				defer func() { <-slots }()
				d.serveAuthenticated(ctx, c, true)
			})
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { cancel(); cleanup(); <-done; removeAlias() }) }, nil
}

// A private temporary directory alias keeps AF_UNIX names short even when the
// daemon-owned mailbox has a long HOME path. It grants no authority; TLS pins
// and signed credentials authenticate both ends. The socket remains in mailbox.
func consoleSocketAlias(path string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "amux-ca-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	if err = os.Symlink(filepath.Dir(path), filepath.Join(dir, "d")); err != nil {
		cleanup()
		return "", nil, err
	}
	alias := filepath.Join(dir, "d", filepath.Base(path))
	if cwd, err := os.Getwd(); err == nil {
		if relative, err := filepath.Rel(cwd, alias); err == nil && len(relative) < len(alias) {
			alias = relative
		}
	}
	return alias, cleanup, nil
}

// DialConsoleAccounts derives identity and endpoint exclusively from the fixed
// trusted session context. Ordinary sessions never fall through to host Dial.
func DialConsoleAccounts() (*Client, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second+authDeadline)
	defer cancel()
	session, err := access.LoadSessionContext()
	if err != nil || session.SubjectID != console.ID {
		return nil, fmt.Errorf("machine account management requires a host terminal or trusted amux console")
	}
	credential, err := access.LoadCredential(core.SessionAccessDir())
	if err != nil || credential.Kind != access.SubjectSession || credential.SubjectID != session.SubjectID {
		return nil, access.ErrDenied
	}
	return dialConsoleAccounts(ctx, session.MailboxDir, credential)
}

func dialConsoleAccounts(ctx context.Context, mailbox string, credential access.Credential) (*Client, error) {
	path := filepath.Join(mailbox, access.ConsoleAccountSocketName)
	// Prefer a relative path from the console directory: its fixed mount is near
	// that directory and may have a much longer absolute host pathname.
	if cwd, err := os.Getwd(); err == nil {
		if relative, err := filepath.Rel(cwd, path); err == nil && len(relative) < len(path) {
			path = relative
		}
	}
	if len(path) >= 100 {
		alias, cleanup, err := consoleSocketAlias(filepath.Join(mailbox, access.ConsoleAccountSocketName))
		if err != nil {
			return nil, err
		}
		defer cleanup()
		path = alias
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("console account service unavailable: %w", err)
	}
	return authenticateClientContext(ctx, conn, credential)
}
