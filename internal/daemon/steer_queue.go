package daemon

import (
	"context"
	"errors"
	"strings"

	"amux/internal/core"
	"amux/internal/prwatch"
)

// A person's typed steer (prompt/interject) that the agent cannot take right
// now — its input box holds a draft, a dialog is open, or amux could not
// confirm the text — is not lost and need not be resent: it is queued on the
// PR watcher's deferral queue (the same machinery that holds PR and member
// wake-ups until a coordinator can take them) and delivered automatically
// through the ordinary steer path once the input box is free. The sender gets
// a composer_busy error saying so.

// steerQueuePrefix namespaces a session's own steer queue among the watcher's
// per-workgroup queues, so queued steers never coalesce with wake-ups.
const steerQueuePrefix = "steer:"

// steerQueuedField marks a steer replayed from the queue: refused again, it
// stays queued rather than being queued a second time.
const steerQueuedField = "queued"

func steerQueueGroup(id string) string { return steerQueuePrefix + id }

// queueSteer settles a person's typed steer against the queue. Refused as busy,
// it is queued for automatic delivery and the error says so; an identical
// resend while it waits collapses into the one queued copy. Delivered directly
// (a resend that found the box free), any queued copy of the same text is
// cancelled, so a retry can never deliver the text twice. Automatic text has
// its own retry (prwatch, the goal hook), and a replayed steer is already
// queued.
func (d *Daemon) queueSteer(a core.Action, verb string, err error) error {
	if a.Fields[steerOriginField] == steerOriginAmux || a.Fields[steerQueuedField] != "" ||
		(verb != core.SteerPrompt && verb != core.SteerInterject) {
		return err
	}
	key := verb + "\x00" + a.Fields[core.SteerText]
	if err == nil {
		d.unmarkQueuedSteer(a.ID, key)
		return nil
	}
	var busy *composerBusyError
	if d.prs == nil || !errors.As(err, &busy) {
		return err
	}
	d.markQueuedSteer(a.ID, key)
	d.prs.Notify(prwatch.Wake{Kind: prwatch.KindSteer, Workgroup: steerQueueGroup(a.ID), SessionID: a.ID, State: verb, Text: a.Fields[core.SteerText]})
	busy.queued = true
	journal(a.ID, core.JournalInfo, "steer queued for automatic delivery: "+busy.reason)
	return busy
}

// deliverQueuedSteers replays a session's queued steers in order through the
// ordinary steer path. The first one the agent still cannot take, and every one
// after it, stays queued; one that fails for another reason (the agent stopped,
// was archived) is dropped and journaled.
func (d *Daemon) deliverQueuedSteers(ctx context.Context, id string, wakes []prwatch.Wake) []prwatch.Delivery {
	results := make([]prwatch.Delivery, len(wakes))
	for i := range results {
		results[i] = prwatch.Deferred
	}
	for i, w := range wakes {
		if w.Kind != prwatch.KindSteer || w.SessionID != id {
			results[i] = prwatch.Dropped
			continue
		}
		key := w.State + "\x00" + w.Text
		if !d.queuedSteerMarked(id, key) {
			// A resend already delivered this text directly (or an identical
			// queued copy went out first): never deliver it twice.
			results[i] = prwatch.Dropped
			continue
		}
		if !d.sessionLive(id) {
			// A queued steer never cold-starts an agent someone stopped.
			results[i] = prwatch.Dropped
			d.unmarkQueuedSteer(id, key)
			journal(id, core.JournalError, "queued steer dropped: the agent is no longer running")
			continue
		}
		res := d.handle(ctx, core.Action{Action: core.ActionSteer, ID: id, Fields: map[string]string{
			core.SteerVerb: w.State, core.SteerText: w.Text, steerQueuedField: "1",
		}})
		switch {
		case res.OK:
			results[i] = prwatch.Delivered
			d.unmarkQueuedSteer(id, key)
			journal(id, core.JournalInfo, "queued steer delivered")
		case res.Code == codeComposerBusy:
			return results // still busy: this and the rest wait, in order
		default:
			results[i] = prwatch.Dropped
			d.unmarkQueuedSteer(id, key)
			journal(id, core.JournalError, "queued steer dropped: "+res.Error)
		}
	}
	return results
}

// markQueuedSteer records that a steer (verb + NUL + text) waits in a session's
// queue; queuedSteerMarked/unmarkQueuedSteer are its reads and its clearing
// when the text is delivered (from the queue or by a direct resend) or dropped.
func (d *Daemon) markQueuedSteer(id, key string) {
	d.steerQueuedMu.Lock()
	defer d.steerQueuedMu.Unlock()
	if d.steerQueued == nil {
		d.steerQueued = map[string]map[string]bool{}
	}
	if d.steerQueued[id] == nil {
		d.steerQueued[id] = map[string]bool{}
	}
	d.steerQueued[id][key] = true
}

func (d *Daemon) queuedSteerMarked(id, key string) bool {
	d.steerQueuedMu.Lock()
	defer d.steerQueuedMu.Unlock()
	return d.steerQueued[id][key]
}

func (d *Daemon) unmarkQueuedSteer(id, key string) {
	d.steerQueuedMu.Lock()
	defer d.steerQueuedMu.Unlock()
	delete(d.steerQueued[id], key)
	if len(d.steerQueued[id]) == 0 {
		delete(d.steerQueued, id)
	}
}

// steerQueueTarget reports the session a watcher queue key addresses, if it is
// a steer queue.
func steerQueueTarget(workgroup string) (string, bool) {
	id, ok := strings.CutPrefix(workgroup, steerQueuePrefix)
	return id, ok && id != ""
}
