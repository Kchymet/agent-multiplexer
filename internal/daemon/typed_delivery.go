package daemon

import (
	"bytes"
	"context"
	"fmt"
	"time"
	"unicode/utf8"

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
// into is shared with the human at the keyboard. deliverTyped therefore:
//
//   - reads only the pane's input region (composer.Classify) and types only
//     into a composer it can see is free — never into a draft, a dialog, or a
//     screen it cannot read;
//   - holds automatic text while a human is at the keyboard;
//   - submits once the input region shows amux's text (plus rendering noise);
//   - never leaves its own unsent text behind: text it typed but could not
//     confirm is taken back character by character when nobody else touched
//     the pane, and text it had to leave (someone typed meanwhile) is
//     remembered so the next delivery recognizes it as amux's and clears it
//     instead of mistaking it for the user's draft;
//   - never deletes or submits text someone else typed.
//
// Steering text is typed rather than bracketed-pasted on Claude Code (see
// agent.claudeKeys): Claude collapses a paste into a "[Pasted text]" token and
// hands it to the model as untrusted pasted content, so a paste would neither
// echo checkably in the composer nor arrive as the user's words.

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
// the composer before it gives up on submitting it. A busy runtime (a long
// transcript re-rendering under a spinner) can lag well behind its input.
const typedEchoWait = 8 * time.Second

// typedPollInterval is how often the screen is re-read while waiting.
const typedPollInterval = 40 * time.Millisecond

// steerOriginField marks a steer action whose text amux itself generated (a
// wake-up, a goal nudge) rather than text a person sent. Such text is held
// while a human is typing in the pane. It only ever makes delivery stricter,
// so honoring it from any caller is safe.
const steerOriginField = "origin"

// steerOriginAmux is steerOriginField's value for amux-generated text.
const steerOriginAmux = "amux"

// codeComposerBusy is the Result.Code of a steer refused because the agent's
// composer was not free (an unsent draft, an open dialog, a human typing, or
// text that could not be confirmed). The text was not submitted.
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
	// typed is set when amux's text was typed and is still in the composer:
	// someone else touched the pane meanwhile, and amux never deletes from a
	// composer someone else has touched. amux remembers the text and clears it
	// before its next delivery once the box holds exactly that.
	typed bool
	// queued is set when the steer was queued for automatic delivery.
	queued bool
}

func (e *composerBusyError) Error() string {
	msg := "composer busy: " + e.reason
	if e.typed {
		msg += "; amux's text is left in the input box (someone else touched it; amux clears it before its next delivery if the box still holds only amux's text)"
	} else {
		msg += "; nothing of amux's is left in the input box"
	}
	if e.queued {
		return msg + "; the message is queued and amux delivers it automatically when the agent can take it (do not resend)"
	}
	return msg + "; retry later"
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
	// typing in the pane.
	automated bool
}

// typedLeftover is amux text left unsent in a session's composer, so a later
// delivery recognizes it as amux's own rather than as the user's draft.
type typedLeftover struct {
	instance engine.Instance // the runtime it was typed into
	text     string          // the steer text, as compared with composer.Same
	runes    int             // characters it occupies in the composer
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
//  3. The input region must classify as a free composer. amux's own text left
//     from an earlier delivery is recognized and cleared first. A draft, an
//     open dialog, or a screen amux cannot read refuses the delivery before
//     anything is typed.
//  4. The text is typed as one input sequence, then the input region is
//     re-read until it shows amux's text (composer.MatchTyped: whitespace and
//     spinner/status glyphs are rendering noise). Only then, and only if no
//     human input arrived meanwhile, is the submit key sent.
//  5. Otherwise amux takes its text back: when nobody else touched the pane
//     since amux saw the composer empty, the composer holds nothing but amux's
//     text, so exactly that many characters are erased. When someone did type,
//     the text is erased only if the box still shows exactly amux's text;
//     otherwise it is left, remembered, and reported.
//
// The caller holds the session's admission, so deliveries to one session do
// not interleave. The bounded waits are why delivery is synchronous: its
// outcome is the caller's to report or queue.
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
	id := req.session.ID

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
	readable := func(p composer.Pane) bool { return p.State == composer.Free || p.State == composer.Draft }
	pane := d.awaitPane(ctx, obs, runtime, boot+timing.composer, timing.poll, readable)

	// amux's own text from an earlier delivery is not the user's draft.
	if pane.State == composer.Draft {
		if left, ok := d.leftover(id, req.instance); ok && left.holds(pane.Text) {
			if !d.eraseTyped(ctx, obs, req, runtime, left.runes, timing) {
				return &composerBusyError{reason: "this agent's input box holds text amux typed earlier and amux could not clear it yet", typed: true}
			}
			d.forgetLeftover(id)
			pane = d.awaitPane(ctx, obs, runtime, timing.composer, timing.poll, readable)
			mark = obs.LastHumanInput()
		}
	}
	switch pane.State {
	case composer.Free:
	case composer.Draft:
		return &composerBusyError{reason: "this agent's input box holds an unsent draft"}
	case composer.Dialog:
		return &composerBusyError{reason: "a dialog (a permission or selection prompt) is open in place of this agent's input box"}
	default:
		return &composerBusyError{reason: "amux cannot see this agent's input box on screen, so it cannot tell whether it holds a draft"}
	}

	n := len(req.payload)
	text := append([]engine.InputStep(nil), req.payload[:n-1]...)
	submit := req.payload[n-1]
	runes := typedRunes(req.keys, text)
	d.holdUntilBooted(text)
	req.instance.InputSequence(text)

	// The text is in the composer now, so the check that decides its fate runs
	// to completion even if the requesting connection goes away.
	wctx := context.WithoutCancel(ctx)
	touched := func() bool { return !obs.LastHumanInput().Equal(mark) }
	echo := composer.EchoNone
	d.awaitPane(wctx, obs, runtime, boot+timing.echo, timing.poll, func(p composer.Pane) bool {
		echo = composer.EchoNone
		if readable(p) {
			echo = composer.MatchTyped(p.Text, req.text)
		}
		return touched() || echo == composer.EchoExact
	})
	if !touched() && echo == composer.EchoExact {
		submit.DelayBefore, submit.NotBeforeAge = 0, 0
		req.instance.InputSequence([]engine.InputStep{submit})
		d.forgetLeftover(id)
		return nil
	}

	// Not submitted: take amux's text back, never anyone else's.
	now := d.readPane(obs, runtime)
	left := typedLeftover{instance: req.instance, text: req.text, runes: runes}
	switch {
	case touched():
		// Someone typed: erase only if the box still holds exactly amux's text.
		if now.State == composer.Draft && composer.Same(now.Text, req.text) && d.eraseTyped(wctx, obs, req, runtime, runes, timing) {
			return &composerBusyError{reason: "someone typed into this agent's pane while amux was typing, so amux took its text back unsent"}
		}
		d.rememberLeftover(id, left)
		return &composerBusyError{reason: "someone typed into this agent's pane while amux was typing", typed: true}
	case readable(now) && composer.MatchTyped(now.Text, req.text) == composer.EchoForeign:
		// The box shows something amux did not type: do not guess.
		d.rememberLeftover(id, left)
		return &composerBusyError{reason: "this agent's input box showed text amux did not type", typed: true}
	case readable(now):
		// Nobody else touched the pane since amux saw the composer empty, so
		// what is in it is only amux's: erase exactly that.
		if d.eraseTyped(wctx, obs, req, runtime, runes, timing) {
			return &composerBusyError{reason: "this agent's input box did not show amux's text in time, so amux took it back unsent"}
		}
	}
	// A dialog or an unreadable screen now covers the composer: sending keys
	// would reach whatever holds focus. Remember the text for later instead.
	d.rememberLeftover(id, left)
	return &composerBusyError{reason: "this agent's input box could not be read after amux typed", typed: true}
}

// eraseTyped presses the runtime's Erase key once per character amux typed and
// reports whether the composer then reads free with no human input meanwhile.
// It is only ever called for text amux knows is solely its own.
func (d *Daemon) eraseTyped(ctx context.Context, obs paneObserver, req typedRequest, runtime string, runes int, timing typedTiming) bool {
	if len(req.keys.Erase) == 0 || runes <= 0 {
		return false
	}
	mark := obs.LastHumanInput()
	req.instance.InputSequence([]engine.InputStep{{Bytes: bytes.Repeat(req.keys.Erase, runes)}})
	p := d.awaitPane(ctx, obs, runtime, timing.echo, timing.poll, func(p composer.Pane) bool { return p.State == composer.Free })
	return p.State == composer.Free && obs.LastHumanInput().Equal(mark)
}

// typedRunes counts the characters the text steps leave in the composer: paste
// framing is not text, and a typed newline key is one character.
func typedRunes(keys agent.Keys, steps []engine.InputStep) int {
	var all []byte
	for _, s := range steps {
		all = append(all, s.Bytes...)
	}
	if len(keys.PasteStart) > 0 {
		all = bytes.ReplaceAll(all, keys.PasteStart, nil)
	}
	if len(keys.PasteEnd) > 0 {
		all = bytes.ReplaceAll(all, keys.PasteEnd, nil)
	}
	if len(keys.TypedNewline) > 0 {
		all = bytes.ReplaceAll(all, keys.TypedNewline, []byte("\n"))
	}
	return utf8.RuneCount(all)
}

// holds reports whether a composer showing text still holds exactly this
// leftover (its words as typing renders them).
func (l typedLeftover) holds(text string) bool { return composer.Same(text, l.text) }

// leftover returns amux text left unsent in this session's current runtime.
func (d *Daemon) leftover(id string, in engine.Instance) (typedLeftover, bool) {
	d.typedLeftMu.Lock()
	defer d.typedLeftMu.Unlock()
	left, ok := d.typedLeft[id]
	if ok && left.instance != in {
		delete(d.typedLeft, id) // a restarted runtime has a fresh composer
		return typedLeftover{}, false
	}
	return left, ok
}

func (d *Daemon) rememberLeftover(id string, left typedLeftover) {
	d.typedLeftMu.Lock()
	defer d.typedLeftMu.Unlock()
	if d.typedLeft == nil {
		d.typedLeft = map[string]typedLeftover{}
	}
	d.typedLeft[id] = left
}

func (d *Daemon) forgetLeftover(id string) {
	d.typedLeftMu.Lock()
	defer d.typedLeftMu.Unlock()
	delete(d.typedLeft, id)
}

// typeBlind types the whole payload without reading the pane, as amux did
// before deliverTyped: for an engine that cannot show the pane.
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

// readPane classifies the pane's current screen.
func (d *Daemon) readPane(obs paneObserver, runtime string) composer.Pane {
	if snap, ok := obs.Screen(); ok {
		return composer.Classify(runtime, snap)
	}
	return composer.Pane{}
}

// awaitPane re-reads the pane until done accepts it or wait elapses (or ctx
// ends), returning the last classification.
func (d *Daemon) awaitPane(ctx context.Context, obs paneObserver, runtime string, wait, poll time.Duration, done func(composer.Pane) bool) composer.Pane {
	deadline := time.Now().Add(wait)
	for {
		p := d.readPane(obs, runtime)
		if done(p) || !time.Now().Before(deadline) {
			return p
		}
		timer := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return p
		case <-timer.C:
		}
	}
}
