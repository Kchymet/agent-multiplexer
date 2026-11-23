// Package directadmin is the host control API for direct client access: the
// one way host-side tools (the amux CLI, the native TUI, the local admin
// console) administer enrolled direct clients. Every call proves the host
// credential through the daemon's challenge before anything is read or
// written (internal/direct/admin.go); session, provider and direct-client
// credentials are refused.
//
// A call goes to the running direct server's admin socket when one is
// listening (so live streams are affected at once), and otherwise runs against
// the store directly under its exclusive lock.
package directadmin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"

	"amux/internal/access"
	"amux/internal/daemon"
	"amux/internal/direct"
)

// HostCredential loads the host credential from the protected directory, which
// HOME and XDG cannot redirect.
func HostCredential() (access.Credential, error) {
	dir, err := access.ProtectedHostCredentialDir()
	if err != nil {
		return access.Credential{}, err
	}
	cred, err := access.LoadCredential(dir)
	if err != nil {
		return access.Credential{}, fmt.Errorf("load the host credential (start the daemon with `amux daemon start`): %w", err)
	}
	if cred.Kind != access.SubjectHost || cred.SubjectID != access.LocalHostSubject {
		return access.Credential{}, access.ErrInvalidCredential
	}
	return cred, nil
}

// RelayHostProof verifies a host proof through the running daemon.
func RelayHostProof(ctx context.Context, prove direct.ProveFunc) error {
	return daemon.RelayHostProof(ctx, prove)
}

// Run sends req to the running direct server, or, when none owns the store,
// runs it offline under the store's exclusive lock. Both require host proof.
func Run(ctx context.Context, req direct.AdminRequest) (direct.AdminResponse, error) {
	cred, err := HostCredential()
	if err != nil {
		return direct.AdminResponse{}, err
	}
	root, err := direct.DefaultRoot()
	if err != nil {
		return direct.AdminResponse{}, err
	}
	resp, err := direct.AdminCall(ctx, root, cred, req)
	if err == nil {
		return resp, nil
	}
	var nerr *net.OpError
	if !errors.As(err, &nerr) && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ECONNREFUSED) {
		return resp, err
	}
	// No server is listening: administer the store directly. If a server owns
	// the lock after all, this refuses rather than writing beside it.
	resp, err = direct.OfflineAdminister(ctx, root, cred, RelayHostProof, req)
	if errors.Is(err, direct.ErrStoreBusy) {
		return resp, errors.New("the direct store is owned by a running server whose admin socket is unreachable")
	}
	return resp, err
}

// Clients lists enrolled clients (all of them, or only the stale ones with
// stale set; staleDays 0 is direct.DefaultStaleDays).
func Clients(ctx context.Context, stale bool, staleDays int) ([]direct.ClientInfo, error) {
	resp, err := Run(ctx, direct.AdminRequest{Op: direct.OpList, Stale: stale, StaleDays: staleDays})
	if err != nil {
		return nil, err
	}
	return resp.Clients, nil
}

// UpdateClient changes a client's grant in place (direct.Authority.UpdateGrant).
// actor names the caller in the grant's record ("cli", "tui", "console").
// The result is returned with a non-nil error when a narrowing is in effect
// but could not be confirmed (persisted, drained or audited).
func UpdateClient(ctx context.Context, u direct.GrantUpdate, actor string) (direct.GrantUpdateResult, error) {
	resp, err := Run(ctx, direct.AdminRequest{Op: direct.OpUpdate, Update: &u, Actor: actor})
	if resp.Update != nil {
		return *resp.Update, err
	}
	if err != nil {
		if resp.Error != "" && resp.Error == "unknown admin operation \""+direct.OpUpdate+"\"" {
			return direct.GrantUpdateResult{}, errors.New("the running direct server predates grant updates; restart it (amux device disable && amux device enable) and retry")
		}
		return direct.GrantUpdateResult{}, err
	}
	return direct.GrantUpdateResult{}, errors.New("the direct server returned no update result")
}

// RevokeClient revokes a client now.
func RevokeClient(ctx context.Context, clientID string) error {
	_, err := Run(ctx, direct.AdminRequest{Op: direct.OpRevoke, ClientID: clientID})
	return err
}
