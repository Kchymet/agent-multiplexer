package access

import "amux/internal/items"

// Items (internal/items) are acted on as the caller itself: every item verb
// names the caller as its target (Request.ID), so the route rules in scope.go
// admit it only for self (allowSelfItem). Which items the caller may then see
// or move is decided here, from daemon-owned state: the caller's Resource and
// the stored item's parties. Clients never supply either.
//
// The routing rule — a member raises to its own coordinator, everyone else to
// the console — and the triage rules are:
//
//   - a session sees what it raised and what it receives; a coordinator also
//     sees every item raised in its workgroup (also after escalating it); the
//     console sees all;
//   - the current recipient (and the console, always) may acknowledge,
//     dismiss, complete or reopen an item; its source may withdraw it
//     (dismiss) or mark it done while it is active;
//   - only a coordinator that currently holds an item may escalate it, and
//     only to the console.

// ItemParties is the authorization-relevant projection of a stored item.
type ItemParties struct {
	Source, Workgroup, Recipient string
}

// PartiesOf projects an item.
func PartiesOf(it items.Item) ItemParties {
	return ItemParties{Source: it.Source, Workgroup: it.Workgroup, Recipient: it.Recipient}
}

// ItemRecipient routes a raise from subject. root is subject's workgroup root
// (rootFound false when subject has none or it is unknown). A member of an
// active workgroup raises to its coordinator; a coordinator, a repo home, an
// agent outside a coordinated workgroup and the console raise to the console.
// workgroup is the coordinator the item belongs to ("" for none).
func ItemRecipient(subject, root Resource, rootFound bool) (recipient, workgroup string) {
	if subject.Role == "coordinator" {
		return items.ConsoleRecipient, subject.ID
	}
	if subject.Role == "" && subject.RootID != "" && rootFound && root.ID == subject.RootID &&
		root.Role == "coordinator" && !root.Archived {
		return root.ID, root.ID
	}
	return items.ConsoleRecipient, ""
}

// ItemVisible reports whether subject may read an item.
func ItemVisible(subject Resource, it ItemParties) bool {
	switch {
	case subject.Role == "console":
		return true
	case subject.ID == "":
		return false
	case it.Source == subject.ID, it.Recipient == subject.ID:
		return true
	case subject.Role == "coordinator" && it.Workgroup == subject.ID:
		return true
	}
	return false
}

// ItemTransitionAllowed reports whether subject may move an item to state.
// The state machine itself (items.CanTransition) is checked separately.
func ItemTransitionAllowed(subject Resource, it ItemParties, state string) bool {
	if !ItemVisible(subject, it) {
		return false
	}
	if subject.Role == "console" || it.Recipient == subject.ID {
		return true
	}
	if it.Source == subject.ID {
		return state == items.StateDismissed || state == items.StateDone
	}
	return false
}

// ItemEscalationAllowed reports whether subject may escalate an item to the
// console: it must be a coordinator holding the item.
func ItemEscalationAllowed(subject Resource, it ItemParties) bool {
	return subject.Role == "coordinator" && subject.ID != "" && it.Recipient == subject.ID
}

// allowSelfItem admits an item verb only for the caller itself.
func allowSelfItem(_ Request, self bool, _ Resource) bool { return self }

// allowEscalateItem admits escalation only for a coordinator acting as itself.
func allowEscalateItem(_ Request, self bool, target Resource) bool {
	return self && target.Role == "coordinator"
}
