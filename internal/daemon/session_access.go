package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"amux/internal/access"
	"amux/internal/console"
	"amux/internal/core"
	"amux/internal/store"

	"golang.org/x/sys/unix"
)

// sessionAccessForLaunch is the only daemon-owned provisioning seam for a
// sandbox launch. Callers must pass the returned SessionAccess into panespec;
// panespec and filesystem helpers must never open a second authority.
func (d *Daemon) sessionAccessForLaunch(ctx context.Context, id string) (store.Session, access.SessionAccess, error) {
	if d.authority == nil {
		return store.Session{}, access.SessionAccess{}, fmt.Errorf("daemon access authority unavailable")
	}
	session, ok, err := lookupSession(id)
	if err != nil {
		return store.Session{}, access.SessionAccess{}, err
	}
	if !ok {
		return store.Session{}, access.SessionAccess{}, fmt.Errorf("session %q not found", id)
	}
	if session.Archived {
		return store.Session{}, access.SessionAccess{}, fmt.Errorf("session %q is archived", id)
	}
	expected := session.Dir
	switch session.Role() {
	case store.RoleConsole:
		expected = console.Dir()
	case store.RoleCoordinator:
		// Coordinator and repo-home layouts require their dedicated namespace
		// directories. The namespace integration supplies that role-aware
		// validator; never preserve the unsafe shared RootDir as authority.
		return store.Session{}, access.SessionAccess{}, fmt.Errorf("root session %q has no secure dedicated launch layout", id)
	case store.RoleRepo:
		expected = store.RootDir(session.ID)
	case store.RoleAgent:
		if err := validateStoredAgentDir(session.Dir); err != nil {
			return store.Session{}, access.SessionAccess{}, fmt.Errorf("session %q uses unsupported agent directory %q: %w", id, session.Dir, err)
		}
	}
	if !filepath.IsAbs(session.Dir) || filepath.Clean(session.Dir) != filepath.Clean(expected) {
		return store.Session{}, access.SessionAccess{}, fmt.Errorf("session %q uses unsupported legacy/shared directory %q", id, session.Dir)
	}
	grant, err := d.authority.EnsureSession(ctx, session.ID, session.Dir)
	if err != nil {
		return store.Session{}, access.SessionAccess{}, err
	}
	return session, grant, nil
}

// validateStoredAgentDir deliberately does not derive a path from RootID: a
// move changes the ownership relationship but leaves the existing worktree in
// place. The immutable stored directory is accepted only as an existing,
// no-symlink descendant below SessionsDir, never the root container itself or
// the reserved coordinator child.
func validateStoredAgentDir(dir string) error {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return fmt.Errorf("path is not absolute and clean")
	}
	rel, err := filepath.Rel(filepath.Clean(core.SessionsDir()), dir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path is outside the session root")
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) < 2 || parts[1] == "coordinator" {
		return fmt.Errorf("path is a container or coordinator directory")
	}
	return validateDirectoryNoSymlinks(dir)
}

func validateDirectoryNoSymlinks(dir string) error {
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	for _, part := range strings.Split(strings.TrimPrefix(dir, string(filepath.Separator)), string(filepath.Separator)) {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("invalid path component")
		}
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		_ = unix.Close(fd)
		fd = next
	}
	return nil
}
