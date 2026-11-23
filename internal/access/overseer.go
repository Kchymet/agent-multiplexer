package access

import (
	"context"
	"strings"

	"amux/internal/core"
)

// The overseer (role "overseer") is the coordinator of coordinators: it reads
// every workgroup, steers and starts coordinators, archives finished
// workgroups, creates workgroups from items the user acknowledged, and triages
// the global item inbox it owns. It holds no admin powers — no accounts or
// auth, direct clients, updates, repo tracking or grants, delete, move, or
// host operations — and it can never reach the console. See
// docs/direct-console.md §2 and docs/workgroup-permissions.md.
//
// Its column of the access matrix is exactly the rules marked Overseer in
// scope.go, narrowed per verb by allowOverseer below. Every unmarked rule is
// denied to it, whatever the rule's scope: unlike the console it has no
// blanket shortcut. To adjust the matrix, change the Overseer marks and this
// one predicate (TestOverseerAccessMatrix pins every cell).

// RoleOverseer is the overseer's role string (store.RoleOverseer).
const RoleOverseer = "overseer"

// FromItemField names the item a workgroup is created from. The overseer may
// create a workgroup only from an item the user (the host) acknowledged.
const FromItemField = "from_item"

// ItemApprover is implemented by a Resolver that can tell whether the user
// acknowledged an item from the host. Without it, the overseer's
// create-from-item is denied.
type ItemApprover interface {
	// HostAcknowledged reports whether itemID exists, is active, and was last
	// acknowledged by the host (the user), not by any session.
	HostAcknowledged(ctx context.Context, itemID string) (bool, error)
}

// allowOverseer narrows an Overseer-marked rule for the overseer once the
// target is known to be in its scope (InWorkgroupScope). self reports whether
// the target is the overseer itself.
func allowOverseer(req Request, self bool, target Resource) bool {
	switch req.Verb {
	case core.ActionStart:
		// Starting a workgroup root starts its members; never single members.
		return target.Role == "coordinator"
	case core.ActionSteer:
		// Coordinators only: members are steered through their coordinator. It
		// never answers a permission request — only the user does (the queue).
		if target.Role != "coordinator" {
			return false
		}
		switch req.Fields[core.SteerVerb] {
		case core.SteerPrompt, core.SteerInterject, core.SteerStop:
			return true
		}
		return false
	case core.ActionSetArchived:
		// Explicit archival only, never forced past the open-PR guard: a
		// workgroup root (cascading to its members) or a member.
		if req.Fields["archived"] != "true" || req.Fields["force"] == "true" {
			return false
		}
		return target.Role == "coordinator" || (target.Role == "" && target.RootID != "")
	case core.ActionRaiseItem, core.ActionItemState, core.QueryItems:
		return self
	case core.QueryRuntimeEvents, core.QueryWait:
		return true
	}
	return false
}

// allowOverseerHost narrows an Overseer-marked host-scope rule.
func allowOverseerHost(req Request) bool {
	switch req.Verb {
	case core.ActionNewWorkgroup:
		// Never as a member of an existing workgroup; always from an item (the
		// policy then asks the ItemApprover whether the user acknowledged it).
		return strings.TrimSpace(req.ID) == "" && strings.TrimSpace(req.Fields[FromItemField]) != ""
	case core.ActionRefresh, core.QueryQueue:
		return true
	}
	return false
}

// authorizeFromItem admits the overseer's create-from-item only when the
// named item was acknowledged by the user (the host), never by a session — a
// prompt-injected overseer cannot spawn work on its own say-so.
func (p Policy) authorizeFromItem(ctx context.Context, req Request) error {
	approver, ok := p.Resolver.(ItemApprover)
	if !ok {
		return ErrDenied
	}
	acked, err := approver.HostAcknowledged(ctx, strings.TrimSpace(req.Fields[FromItemField]))
	if err != nil {
		return err
	}
	if !acked {
		return ErrDenied
	}
	return nil
}
