package daemon

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"amux/internal/agent"
	"amux/internal/claudegoal"
	"amux/internal/core"
	"amux/internal/prwatch"
	"amux/internal/store"
	"amux/internal/vterm"
)

// fakePane simulates a Claude Code pane for typed delivery: a composer that
// typed keys edit (Claude's newline escape, backslash+Enter, adds a line; Enter
// submits; Esc closes an open question selector), painted the way the real TUI
// paints it so the daemon reads it through the production parser.
type fakePane struct {
	mu        sync.Mutex
	composer  string
	question  bool // an AskUserQuestion selector replaces the composer
	garbled   bool // a screen with no recognizable composer
	human     time.Time
	submitted []string
	// onTyped runs after amux's text lands (never after the submit key), as if
	// a person typed at that instant.
	onTyped func(*fakePane)
}

func (p *fakePane) apply(b []byte) {
	p.mu.Lock()
	typed := false
	for i := 0; i < len(b); i++ {
		switch {
		case b[i] == '\\' && i+1 < len(b) && b[i+1] == '\r':
			p.composer += "\n"
			i++
			typed = true
		case b[i] == '\r':
			if !p.question {
				p.submitted = append(p.submitted, p.composer)
				p.composer = ""
			}
		case b[i] == 0x1b:
			p.question = false
		default:
			p.composer = string(append([]byte(p.composer), b[i]))
			typed = true
		}
	}
	hook := p.onTyped
	p.mu.Unlock()
	if typed && hook != nil {
		hook(p)
	}
}

// userTypes is a person typing into the pane.
func (p *fakePane) userTypes(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.composer += s
	p.human = time.Now()
}

func (p *fakePane) state() (composer string, submitted []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.composer, append([]string(nil), p.submitted...)
}

func (p *fakePane) screen() vterm.Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	const rule = "\x1b[90m────────────────────────────────────────────────────────────\x1b[0m"
	var b strings.Builder
	b.WriteString("\x1b[2J\x1b[H⏺ Finished the last task.\r\n\r\n")
	switch {
	case p.garbled:
		b.WriteString("\x1b[5;10H▒▒▒ partial repaint ▒▒▒")
	case p.question:
		b.WriteString("Which approach?\r\n❯ 1. Polling\r\n  2. Webhooks\r\n\x1b[2mEnter to select · Esc to cancel\x1b[0m")
	default:
		b.WriteString(rule + "\r\n> ")
		if p.composer == "" {
			b.WriteString("\x1b[7mT\x1b[0m\x1b[2mry \"fix lint errors\"\x1b[0m")
		} else {
			b.WriteString(strings.ReplaceAll(p.composer, "\n", "\r\n  "))
		}
		b.WriteString("\r\n" + rule + "\r\n\x1b[2m  ? for shortcuts\x1b[0m")
	}
	return vterm.Capture([]byte(b.String()), 100, 30)
}

func (f *fakeInstance) Screen() (vterm.Snapshot, bool) {
	if f.fx != nil {
		return f.fx.screen(), true
	}
	if f.pane == nil {
		return vterm.Snapshot{}, false
	}
	return f.pane.screen(), true
}

func (f *fakeInstance) LastHumanInput() time.Time {
	if f.fx != nil {
		f.fx.mu.Lock()
		defer f.fx.mu.Unlock()
		return f.fx.human
	}
	if f.pane == nil {
		return time.Time{}
	}
	f.pane.mu.Lock()
	defer f.pane.mu.Unlock()
	return f.pane.human
}

func (f *fakeInstance) Started() time.Time { return time.Now().Add(-time.Hour) }

func fastTyped(d *Daemon) {
	d.typedTiming = typedTiming{composer: 100 * time.Millisecond, echo: 150 * time.Millisecond, poll: 2 * time.Millisecond}
}

const userDraft = "...Ideally we can support status and polling on those"

// The reported failure: a PR wake-up arriving mid-draft was appended to the
// draft and submitted with it. Now the draft is left alone and the wake-up
// deferred; once the draft is sent, the wake-up goes out on its own, once.
func TestPRWakeNeverMixesWithADraftAndIsDeliveredLaterOnce(t *testing.T) {
	d, in, root, _, wake := prWakeFixture(t)
	fastTyped(d)
	in.pane = &fakePane{}
	in.pane.userTypes(userDraft)
	in.pane.human = time.Now().Add(-time.Minute) // paused, but the draft is unsent

	if got := d.deliverPRWake(context.Background(), root.ID, []prwatch.Wake{wake}); got[0] != prwatch.Deferred {
		t.Fatalf("wake into a draft = %v, want Deferred", got)
	}
	composerText, submitted := in.pane.state()
	if composerText != userDraft || len(submitted) != 0 {
		t.Fatalf("draft touched: composer=%q submitted=%q", composerText, submitted)
	}

	// The user sends their draft; the retried wake-up then lands alone.
	in.pane.apply([]byte("\r"))
	if got := d.deliverPRWake(context.Background(), root.ID, []prwatch.Wake{wake}); got[0] != prwatch.Delivered {
		t.Fatalf("retried wake = %v, want Delivered", got)
	}
	_, submitted = in.pane.state()
	if len(submitted) != 2 || submitted[0] != userDraft {
		t.Fatalf("submitted = %q, want the draft then the wake-up", submitted)
	}
	wakePrompt := submitted[1]
	if !strings.HasPrefix(wakePrompt, prWakeMarker+"\n") || !strings.Contains(wakePrompt, wake.Text) || strings.Contains(wakePrompt, userDraft) {
		t.Fatalf("wake prompt = %q: want the marker on its own first line and no draft text", wakePrompt)
	}
	// Nothing was left behind to be delivered a second time.
	if composerText, _ := in.pane.state(); composerText != "" {
		t.Fatalf("composer after delivery = %q", composerText)
	}
}

func TestPRWakeWaitsForTheHumanToGoQuiet(t *testing.T) {
	d, in, root, _, wake := prWakeFixture(t)
	fastTyped(d)
	in.pane = &fakePane{human: time.Now().Add(-2 * time.Second)} // typed and cleared, 2s ago
	if got := d.deliverPRWake(context.Background(), root.ID, []prwatch.Wake{wake}); got[0] != prwatch.Deferred {
		t.Fatalf("wake while the user is active = %v, want Deferred", got)
	}
	if _, submitted := in.pane.state(); len(submitted) != 0 || in.written() != "" {
		t.Fatalf("typed into an active pane: submitted=%q written=%q", submitted, in.written())
	}
	in.pane.mu.Lock()
	in.pane.human = time.Now().Add(-humanQuietPeriod - time.Second)
	in.pane.mu.Unlock()
	if got := d.deliverPRWake(context.Background(), root.ID, []prwatch.Wake{wake}); got[0] != prwatch.Delivered {
		t.Fatalf("wake after the quiet period = %v, want Delivered", got)
	}
	if _, submitted := in.pane.state(); len(submitted) != 1 {
		t.Fatalf("submitted = %q, want exactly the wake-up", submitted)
	}
}

// The race: the user starts typing after the composer was checked but before
// amux pressed Enter. amux must not submit, must not delete what the user
// typed, and the wake-up stays deferred.
func TestTypedDeliveryAbortsWhenTheUserTypesMidDelivery(t *testing.T) {
	d, in, root, _, wake := prWakeFixture(t)
	fastTyped(d)
	in.pane = &fakePane{onTyped: func(p *fakePane) {
		p.onTyped = nil
		p.composer += " and my words"
		p.human = time.Now()
	}}
	if got := d.deliverPRWake(context.Background(), root.ID, []prwatch.Wake{wake}); got[0] != prwatch.Deferred {
		t.Fatalf("wake raced by the user = %v, want Deferred", got)
	}
	composerText, submitted := in.pane.state()
	if len(submitted) != 0 {
		t.Fatalf("submitted %q despite the user typing", submitted)
	}
	if !strings.HasSuffix(composerText, " and my words") {
		t.Fatalf("user's text was removed: composer=%q", composerText)
	}
}

// A person's own steer goes through the same check: into a draft it is
// refused with a clear, stable code rather than mixed.
func TestUserSteerIntoADraftIsRefusedWithComposerBusy(t *testing.T) {
	d, eng := steerDaemon(t)
	fastTyped(d)
	putSession(t, "a1", "claude")
	in := eng.running("a1")
	in.pane = &fakePane{}
	in.pane.userTypes(userDraft)

	res := d.handle(context.Background(), core.Action{Action: core.ActionSteer, ID: "a1", Fields: map[string]string{
		core.SteerVerb: core.SteerPrompt, core.SteerText: "look at PR 7",
	}})
	if res.OK || res.Code != codeComposerBusy || !strings.Contains(res.Error, "draft") {
		t.Fatalf("steer into a draft = %+v, want composer_busy", res)
	}
	if composerText, submitted := in.pane.state(); composerText != userDraft || len(submitted) != 0 {
		t.Fatalf("draft touched: composer=%q submitted=%q", composerText, submitted)
	}

	// A person's steer is not held by the quiet period (they chose to send
	// it), only by the draft.
	in.pane.mu.Lock()
	in.pane.composer = ""
	in.pane.mu.Unlock()
	res = d.handle(context.Background(), core.Action{Action: core.ActionSteer, ID: "a1", Fields: map[string]string{
		core.SteerVerb: core.SteerPrompt, core.SteerText: "/look at PR 7",
	}})
	if !res.OK {
		t.Fatalf("steer into an empty composer: %s", res.Error)
	}
	if _, submitted := in.pane.state(); len(submitted) != 1 || strings.TrimSpace(submitted[0]) != "/look at PR 7" {
		t.Fatalf("submitted = %q", submitted)
	}
}

// Replying to an open question: Esc closes the selector, the composer comes
// back, and the reply is checked and typed into it like any other text.
func TestQuestionReplyDismissesThenDeliversSafely(t *testing.T) {
	d, eng := steerDaemon(t)
	fastTyped(d)
	d.questionOpenFn = func(agent.Harness, store.Session) bool { return true }
	putSession(t, "a1", "claude")
	in := eng.running("a1")
	in.pane = &fakePane{question: true}
	if err := d.steer(context.Background(), core.Action{ID: "a1", Fields: map[string]string{
		core.SteerVerb: core.SteerPrompt, core.SteerText: "webhooks, please",
	}}); err != nil {
		t.Fatalf("reply: %v", err)
	}
	if _, submitted := in.pane.state(); len(submitted) != 1 || submitted[0] != "webhooks, please" {
		t.Fatalf("submitted = %q", submitted)
	}
}

// A screen amux cannot read (booting, a repaint, an unknown layout): nothing
// is typed — automatic text waits for its own retry, and a person's steer is
// refused with a clear error and queued for automatic delivery, since typed
// blind it could land in a draft or answer whatever holds the keyboard.
func TestUnreadableComposerTypesNothing(t *testing.T) {
	d, eng := steerDaemon(t)
	fastTyped(d)
	putSession(t, "a1", "claude")
	in := eng.running("a1")
	in.pane = &fakePane{garbled: true}
	err := d.steer(context.Background(), core.Action{ID: "a1", Fields: map[string]string{
		core.SteerVerb: core.SteerPrompt, core.SteerText: "wake", steerOriginField: steerOriginAmux,
	}})
	if err == nil || !strings.Contains(err.Error(), "cannot see") {
		t.Fatalf("automatic text to an unreadable pane: err=%v", err)
	}
	if in.written() != "" {
		t.Fatalf("typed %q into an unreadable pane", in.written())
	}
	err = d.steer(context.Background(), core.Action{ID: "a1", Fields: map[string]string{
		core.SteerVerb: core.SteerPrompt, core.SteerText: "user text",
	}})
	if err == nil || !strings.Contains(err.Error(), "cannot see") || !strings.Contains(err.Error(), "queued") {
		t.Fatalf("user steer to an unreadable pane: err=%v, want refused and queued", err)
	}
	if in.written() != "" {
		t.Fatalf("typed %q into an unreadable pane", in.written())
	}
}

// Resuming a goal while the user is drafting resumes it, but leaves the draft
// alone: the nudge is not typed, and the user's next prompt continues the goal.
func TestGoalResumeNudgeNeverJoinsADraft(t *testing.T) {
	d, in, root, _, _ := prWakeFixture(t)
	fastTyped(d)
	writeGoalFile(t, root, claudegoal.State{Objective: "ship", Status: claudegoal.StatusPaused, Created: 1})
	in.pane = &fakePane{}
	in.pane.userTypes(userDraft)
	if err := d.steer(context.Background(), core.Action{ID: root.ID, Fields: map[string]string{
		core.SteerVerb: core.SteerGoal, core.SteerGoalStatus: "active",
	}}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := readGoalFile(t, root); got.Status != claudegoal.StatusActive {
		t.Fatalf("goal = %+v, want resumed", got)
	}
	if composerText, submitted := in.pane.state(); composerText != userDraft || len(submitted) != 0 {
		t.Fatalf("draft touched: composer=%q submitted=%q", composerText, submitted)
	}

	// With the composer free (and the user idle) the nudge lands on its own.
	writeGoalFile(t, root, claudegoal.State{Objective: "ship", Status: claudegoal.StatusPaused, Created: 1})
	in.pane.mu.Lock()
	in.pane.composer, in.pane.human = "", time.Now().Add(-time.Minute)
	in.pane.mu.Unlock()
	if err := d.steer(context.Background(), core.Action{ID: root.ID, Fields: map[string]string{
		core.SteerVerb: core.SteerGoal, core.SteerGoalStatus: "active",
	}}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if _, submitted := in.pane.state(); len(submitted) != 1 || submitted[0] != claudegoal.ResumePrompt {
		t.Fatalf("submitted = %q, want the resume nudge alone", submitted)
	}
}
