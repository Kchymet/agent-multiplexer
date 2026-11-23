package itemsvc

import (
	"context"
	"testing"

	"amux/internal/access"
	"amux/internal/items"
)

// TestHostAcknowledgmentIsRecorded: only an acknowledgment made on the host
// path (the user) counts for the overseer's create-from-item; the overseer
// acknowledging the item itself does not, and a later session ack revokes it.
func TestHostAcknowledgmentIsRecorded(t *testing.T) {
	svc, res := newService(t)
	res[items.GlobalInbox()] = access.Resource{ID: items.GlobalInbox(), Role: access.RoleOverseer}
	ctx := context.Background()
	it, _, err := svc.Raise(ctx, "wg1", items.Raise{Kind: items.KindDecisionNeeded, Title: "Start the billing rewrite?"})
	if err != nil || it.Recipient != items.GlobalInbox() {
		t.Fatalf("raise = %+v, %v", it, err)
	}
	if HostAcknowledged(it) {
		t.Fatal("an open item counts as acknowledged")
	}

	// The overseer acknowledging its own inbox item is not the user's say-so.
	it, err = svc.Transition(ctx, items.GlobalInbox(), it.ID, items.VerbAck, "")
	if err != nil {
		t.Fatal(err)
	}
	if HostAcknowledged(it) {
		t.Fatal("a session acknowledgment counts as the user's")
	}

	// Reopen, then acknowledge on the host path.
	if it, err = svc.Transition(ctx, items.GlobalInbox(), it.ID, items.VerbReopen, ""); err != nil {
		t.Fatal(err)
	}
	it, err = svc.Transition(WithHost(ctx), items.GlobalInbox(), it.ID, items.VerbAck, "go ahead")
	if err != nil {
		t.Fatal(err)
	}
	if !HostAcknowledged(it) || !it.History[len(it.History)-1].Host {
		t.Fatalf("host acknowledgment not recorded: %+v", it.History)
	}

	// Done is no longer an acknowledgment to act on.
	it, err = svc.Transition(ctx, items.GlobalInbox(), it.ID, items.VerbDone, "workgroup abc123")
	if err != nil {
		t.Fatal(err)
	}
	if HostAcknowledged(it) {
		t.Fatal("a done item still counts as acknowledged")
	}
}

func TestWithHost(t *testing.T) {
	if IsHost(context.Background()) {
		t.Fatal("a bare context is the host")
	}
	if !IsHost(WithHost(context.Background())) {
		t.Fatal("WithHost not observed")
	}
}
