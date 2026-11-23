package itemsvc

import (
	"context"

	"amux/internal/items"
)

type hostKey struct{}

// WithHost marks ctx as the host (the user) acting, so item history records
// the change as the user's (items.Event.Host). Only the daemon's host control
// path sets it; a session's RPC never does.
func WithHost(ctx context.Context) context.Context {
	return context.WithValue(ctx, hostKey{}, true)
}

// IsHost reports whether ctx was marked by WithHost.
func IsHost(ctx context.Context) bool {
	v, _ := ctx.Value(hostKey{}).(bool)
	return v
}

// HostAcknowledged reports whether it is acknowledged, and its latest
// acknowledgment was the host's (the user's) rather than any session's. This
// is the gate on the overseer creating a workgroup from an item.
func HostAcknowledged(it items.Item) bool {
	if it.State != items.StateAcknowledged {
		return false
	}
	for i := len(it.History) - 1; i >= 0; i-- {
		if it.History[i].Action == items.EventAcknowledged {
			return it.History[i].Host
		}
	}
	return false
}
