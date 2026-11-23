package daemon

import (
	"context"
	"fmt"
	"time"

	"amux/internal/agent"
	"amux/internal/composer"
	"amux/internal/engine"
	"amux/internal/store"
	"amux/internal/vterm"
)

// This file is the one path by which amux types text into an interactive
// agent's composer: PR watcher and member wake-ups, the Claude goal resume
// nudge, and steering `prompt`/`interject` from the console, a direct client
// or the Android app (including a reply that first dismisses an open
// AskUserQuestion selector). A new sender must come through deliverTyped too.
//
// Typing is the only input an interactive TUI has, and the composer it types
// into is shared with the human at the keyboard. Before this, amux typed
// blind: a wake-up arriving while the user was mid-draft was appended to the
// draft, and the trailing Enter submitted both as one prompt. deliverTyped
// therefore types only into a composer it can see is empty, holds automatic
// text while a human is at the keyboard, and submits only once the composer
// shows exactly amux's text — never touching text it did not type.

// humanQuietPeriod is how long after a human last typed into an agent's pane
// automatic text (wake-ups, nudges) waits before it is typed: a user who
// paused mid-sentence has not necessarily finished, and amux must not take
// the composer from them.
const humanQuietPeriod = 15 * time.Second

// composerWait bounds how long delivery waits for a recognizable composer on
// screen — after dismissing a question selector, or while the runtime paints
// it — before giving up on reading it.
const composerWait = 1500 * time.Millisecond

// typedEchoWait bounds how long delivery waits for typed text to show up in
// the composer before it refuses to submit.
const typedEchoWait = 3 * time.Second

// typedPollInterval is how often the screen is re-read while waiting.
const typedPollInterval = 40 * time.Millisecond

// steerOriginField marks a steer action whose text amux itself generated (a
// wake-up, a goal nudge) rather than text a person sent. Such text is held
// while a human is typing in the pane, and is never typed into a composer amux
// cannot read. It only ever makes delivery stricter, so honoring it from any
// caller is safe.
const steerOriginField = "origin"

// steerOriginAmux is steerOriginField's value for amux-generated text.
const steerOriginAmux = "amux"

// codeComposerBusy is the Result.Code of a steer refused because the agent's
// composer was not free (an unsent draft, a human typing, or text that did not
// land as typed). The text was not submitted; the caller may retry.
const codeComposerBusy = "composer_busy"

// paneObserver is what an engine instance offers for reading the pane before
// typing into it (engine/local implements it). An instance without it, or one
// whose Screen reports !ok, is typed into as before this check existed.
type paneObserver interface {
	Screen() (vterm.Snapshot, bool)
	LastHumanInput() time.Time
	Started() time.Time
}

// composerBusyError reports typed text that was not submitted.
type composerBusyError struct {
	reason string
	// typed is set when amux's text was typed but left unsubmitted in the
	// composer (the user typed into it meanwhile): amux never deletes from a
	// composer someone else has touched.
	typed bool
}

func (e *composerBusyError) Error() string {
	if e.typed {
		return "composer busy: " + e.reason + "; amux's text was typed but not submitted and is left in the input box (amux never deletes text someone else touched)"
	}
	return "composer busy: " + e.reason + "; nothing was typed, retry later"
}

// typedRequest is one piece of text for an agent's composer.
type typedRequest struct {
	harness  agent.Harness
	session  store.Session
	instance engine.Instance
	keys     agent.Keys
	// payload is steerPayload's rendering of text: the text's keystrokes, then
	// the submit key as its final step.
	payload []engine.InputStep
	text    string
	// automated is amux-generated text (steerOriginAmux): it waits out a human
	// typing in the pane, and is not typed into a composer amux cannot read.
	automated bool
}

// typedTiming overrides deliverTyped's waits (tests); zero fields use the
// package defaults.
type typedTiming struct {
	quiet, composer, echo, poll time.Duration
}

func (t typedTiming) or() typedTiming {
	if t.quiet == 0 {
		t.quiet = humanQuietPeriod
	}
	if t.composer == 0 {
		t.composer = composerWait
	}
	if t.echo == 0 {
		t.echo = typedEchoWait
	}
	if t.poll == 0 {
		t.poll = typedPollInterval
	}
	return t
}

// deliverTyped types req's text into the agent's composer and submits it, or
// returns a *composerBusyError without submitting anything:
//
//  1. Automatic text waits while a human typed into the pane in the last
//     humanQuietPeriod.
//  2. An open question selector is dismissed first (its composer comes back).
//  3. The composer must be recognized and empty. A draft — anything a person
//     typed and has not sent — refuses the delivery. An unrecognized screen
//     refuses automatic text; a person's own steer is typed as before, since
//     the person chose to send it.
//  4. The text is typed as one uninterruptible input sequence, then the
//     screen is re-read: only if the composer shows exactly amux's text, and
//     no human input arrived meanwhile, is the submit key sent. Otherwise amux
//     stops, leaving whatever is there.
//
// The caller holds the session's admission, so deliveries to one session do
// not interleave. The bounded waits (composerWait, typedEchoWait) are why
// delivery is synchronous: its outcome is the caller's to report or retry.
func (d *Daemon) deliverTyped(ctx context.Context, req typedRequest) error {
	if len(req.payload) == 0 {
		return fmt.Errorf("nothing to type")
	}
	dismiss := len(req.keys.DismissQuestion) > 0 && d.questionOpen(req.harness, req.session)
	obs, observable := req.instance.(paneObserver)
	if observable {
		_, observable = obs.Screen()
	}
	if !observable {
		// The engine cannot show amux this pane, so there is nothing to check.
		d.typeBlind(req, dismiss)
		return nil
	}
	timing := d.typedTiming.or()
	runtime := agent.Canonical(req.session.Agent)

	mark := obs.LastHumanInput()
	if req.automated && !mark.IsZero() {
		if idle := time.Since(mark); idle < timing.quiet {
			return &composerBusyError{reason: fmt.Sprintf("someone typed into this agent's pane %s ago (amux waits until it has been idle for %s)",
				idle.Round(time.Second), timing.quiet)}
		}
	}
	// A runtime younger than the boot settle has not painted its composer yet;
	// typed text is held until then anyway (holdUntilBooted).
	boot := d.bootRemaining(obs)

	if dismiss {
		req.instance.InputSequence([]engine.InputStep{{Bytes: append([]byte(nil), req.keys.DismissQuestion...)}})
	}
	reading := d.awaitComposer(ctx, obs, runtime, boot+timing.composer, timing.poll, func(r composer.Reading) bool { return r.Found })
	if !reading.Found {
		if req.automated {
			return &composerBusyError{reason: "amux cannot see this agent's input box on screen, so it cannot tell whether it holds a draft"}
		}
		// A person's own steer, to a screen amux cannot read: type it, as
		// amux always did. (A dismissal was already sent above.)
		d.typeBlind(req, false)
		return nil
	}
	if !reading.Empty() {
		return &composerBusyError{reason: "this agent's input box holds an unsent draft"}
	}

	n := len(req.payload)
	text := append([]engine.InputStep(nil), req.payload[:n-1]...)
	submit := req.payload[n-1]
	d.holdUntilBooted(text)
	req.instance.InputSequence(text)

	// The text is in the composer now, so the check that decides whether to
	// submit it runs to completion even if the requesting connection goes away.
	touched := func() bool { return !obs.LastHumanInput().Equal(mark) }
	reading = d.awaitComposer(context.WithoutCancel(ctx), obs, runtime, boot+timing.echo, timing.poll, func(r composer.Reading) bool {
		return touched() || (r.Found && composer.Same(r.Text, req.text))
	})
	if touched() {
		return &composerBusyError{reason: "someone typed into this agent's pane while amux was typing", typed: true}
	}
	if !reading.Found || !composer.Same(reading.Text, req.text) {
		return &composerBusyError{reason: "this agent's input box did not show exactly amux's text", typed: true}
	}
	submit.DelayBefore, submit.NotBeforeAge = 0, 0
	req.instance.InputSequence([]engine.InputStep{submit})
	return nil
}

// typeBlind types the whole payload without reading the pane, as amux did
// before deliverTyped: for an engine that cannot show the pane, and for a
// person's own steer to a composer amux cannot recognize.
func (d *Daemon) typeBlind(req typedRequest, dismiss bool) {
	payload := req.payload
	if dismiss {
		payload = dismissingQuestion(req.keys, payload)
	}
	d.holdUntilBooted(payload)
	req.instance.InputSequence(payload)
}

// bootRemaining is how much of the boot settle the runtime still has ahead.
func (d *Daemon) bootRemaining(obs paneObserver) time.Duration {
	settle := d.steerSettle
	if settle == 0 {
		settle = steerStartSettle
	}
	started := obs.Started()
	if started.IsZero() {
		return 0
	}
	return max(0, settle-time.Since(started))
}

// awaitComposer re-reads the composer until done accepts the reading or wait
// elapses (or ctx ends), returning the last reading.
func (d *Daemon) awaitComposer(ctx context.Context, obs paneObserver, runtime string, wait, poll time.Duration, done func(composer.Reading) bool) composer.Reading {
	deadline := time.Now().Add(wait)
	for {
		var reading composer.Reading
		if snap, ok := obs.Screen(); ok {
			reading = composer.Read(runtime, snap)
		}
		if done(reading) || !time.Now().Before(deadline) {
			return reading
		}
		timer := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return reading
		case <-timer.C:
		}
	}
}
