package daemon

import (
	"context"
	"log"
	"strings"

	"amux/internal/access"
	"amux/internal/agent"
	"amux/internal/claudegoal"
	"amux/internal/engine"
	"amux/internal/items"
	"amux/internal/itemsvc"
	"amux/internal/panespec"
	"amux/internal/prwatch"
	"amux/internal/store"
)

// The overseer (internal/overseer) is the coordinator of coordinators. Its
// access column lives in internal/access (overseer.go); this file serves the
// daemon-side half of its one stateful rule: it may create a workgroup only
// from an item the user acknowledged from the host.

var _ access.ItemApprover = (*daemonAccessResolver)(nil)

// HostAcknowledged reports whether itemID is acknowledged and its latest
// acknowledgment was the host's (itemsvc.HostAcknowledged). A missing or
// malformed item is simply not acknowledged.
func (r *daemonAccessResolver) HostAcknowledged(ctx context.Context, itemID string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !items.ValidID(itemID) {
		return false, nil
	}
	lookup := r.itemLookup
	if lookup == nil {
		lookup = readItem
	}
	it, ok, err := lookup(itemID)
	if err != nil || !ok {
		return false, err
	}
	return itemsvc.HostAcknowledged(it), nil
}

// deliverGlobalInboxWakes offers the global inbox owner (items.GlobalInbox,
// the overseer) its unnotified items and the coordinator updates addressed to
// it (prwatch.KindOverseer). The overseer is started lazily: a wake-up for a
// stopped overseer is a prompt that starts it. A live one takes a wake-up only
// between turns with no prompt open, like a coordinator; otherwise it is
// deferred. The items stay in the global inbox either way.
func (d *Daemon) deliverGlobalInboxWakes(ctx context.Context, wakes []prwatch.Wake) []prwatch.Delivery {
	results := make([]prwatch.Delivery, len(wakes))
	var updates []prwatch.Wake
	for i, w := range wakes {
		switch w.Kind {
		case prwatch.KindItems:
			results[i] = prwatch.Deferred
		case prwatch.KindOverseer:
			results[i] = prwatch.Deferred
			updates = append(updates, w)
		default:
			results[i] = prwatch.Dropped
		}
	}
	owner := items.GlobalInbox()
	release := d.admitSessions(owner)
	defer release()
	shown, all, err := d.loadItemWake(owner)
	if err != nil {
		log.Printf("items: global inbox wake-up: %v", err)
		return results
	}
	if len(all) == 0 {
		for i, w := range wakes {
			if w.Kind == prwatch.KindItems {
				results[i] = prwatch.Dropped
			}
		}
		if len(updates) == 0 {
			return results
		}
	}
	sess, found, err := lookupSession(owner)
	if err != nil || !found || sess.Archived {
		log.Printf("items: global inbox owner %s unavailable: found=%v err=%v", owner, found, err)
		return results
	}
	if d.sessionLive(sess.ID) {
		if ready, why := d.coordinatorTakesWake(sess); !ready {
			if why != "" {
				log.Printf("items: global inbox wake-up deferred: %s", why)
			}
			return results
		}
	} else if d.engine == nil {
		return results
	}
	lines := prwatch.WakeLines(updates)
	if len(all) > 0 {
		lines = append(lines, itemWakeLines(shown, len(all))...)
	}
	text := overseerWakeText(agent.HookGoals(sess), lines, len(all) > 0)
	outcome, err := d.beginPRWake(ctx, sess, text)
	if err != nil || outcome != prwatch.Delivered {
		if outcome == prwatch.Indeterminate {
			for i := range results {
				if results[i] == prwatch.Deferred {
					results[i] = prwatch.Indeterminate
				}
			}
		}
		log.Printf("items: global inbox wake-up not delivered: %v", err)
		return results
	}
	if len(all) > 0 {
		if err := d.items.MarkNotified(owner, all); err != nil {
			log.Printf("items: mark global inbox items notified: %v", err)
		}
	}
	for i := range results {
		if results[i] == prwatch.Deferred {
			results[i] = prwatch.Delivered
		}
	}
	log.Printf("items: woke global inbox owner %s (%d item(s), %d coordinator update(s))", owner, len(all), len(updates))
	return results
}

// sessionLive reports whether a session's runtime is up (structured or PTY).
func (d *Daemon) sessionLive(id string) bool {
	if d.codex != nil {
		if _, ok := d.codex.Get(id); ok {
			return true
		}
	}
	if d.engine == nil {
		return false
	}
	_, ok := d.engine.Lookup(engine.Key{AgentID: id, Tab: panespec.TabAgent})
	return ok
}

// overseerWakeText is the overseer's wake-up: coordinator updates and new
// global inbox items, pointing at the queue and the inbox.
func overseerWakeText(hookGoal bool, lines []string, hasItems bool) string {
	prefix := "[amux]"
	if hookGoal {
		prefix = prWakeMarker
	}
	var b strings.Builder
	b.WriteString(prefix + " Overseer update (amux wake-up, not a new task):")
	for _, l := range lines {
		b.WriteString("\n- ")
		b.WriteString(l)
	}
	b.WriteString("\nCheck `amux queue list` (what the user must answer) and act on these as your guide says.")
	if hasItems {
		b.WriteString(" " + itemsTriageHint)
	}
	return b.String()
}

// overseerGoalUpdate decides whether a coordinator's goal moving from prev to
// cur is worth telling the overseer, and how to say it. prev is "" for a
// coordinator not seen before (never a wake).
func overseerGoalUpdate(prev, cur, note string) (string, bool) {
	if prev == "" || prev == cur {
		return "", false
	}
	note = boundRunes(note, 160)
	switch cur {
	case claudegoal.StatusInput:
		return "asked the user: " + firstNonEmpty(note, "(see `amux queue list`)"), true
	case claudegoal.StatusBlocked:
		return "reported itself blocked: " + firstNonEmpty(note, "(no reason given)"), true
	case claudegoal.StatusComplete:
		return "completed its goal" + suffixNote(note), true
	}
	return "", false
}

func suffixNote(note string) string {
	if note == "" {
		return ""
	}
	return ": " + note
}

// readItem reads one item from the store, read-only.
func readItem(id string) (items.Item, bool, error) {
	db, err := store.OpenReadOnly()
	if err != nil {
		return items.Item{}, false, err
	}
	defer db.Close()
	return db.Item(id)
}
