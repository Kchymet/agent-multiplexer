package daemon

import (
	"context"
	"fmt"
	"path/filepath"

	"amux/internal/access"
	"amux/internal/console"
	"amux/internal/store"
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
	expected := ""
	switch {
	case session.ID == console.ID:
		expected = console.Dir()
	case session.RootID == "":
		expected = store.RootDir(session.ID)
	default:
		expected = store.AgentDir(session.RootID, session.ID)
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
