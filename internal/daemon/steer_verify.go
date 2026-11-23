package daemon

import (
	"context"
	"fmt"
	"os"
	"time"

	"amux/internal/agent"
	"amux/internal/core"
	"amux/internal/engine"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// A `prompt` to a stopped agent is the one steering verb that starts a runtime.
// The start used to be fire-and-forget: the verb answered "ok" the moment the
// cold start was queued, and every later outcome (a launch error, a runtime that
// exited while booting, text typed into a TUI that never painted its composer)
// reached only the session journal. The CLI caller saw "ok", `amux status` kept
// showing the agent stopped, and the next interject was refused as "not
// running" (2026-11-23, coordinators 5a3b1a, e06ffd and 7ad904).
//
// The cold start still runs as deferred work under its own session admission
// (a Claude Code boot takes seconds, and the action's admission must not be held
// that long). But a caller that can wait now does: once the action has released
// its admission, handle waits for the deferred start to report that the runtime
// is up AND took the prompt, and turns anything else into a failed result that
// says why. "ok" means delivered.

// steerVerifyTimeout bounds how long a cold-start prompt waits for the runtime
// to acknowledge the prompt, measured from when the text was queued.
const steerVerifyTimeout = 90 * time.Second

// steerAckPoll is how often the acknowledgement is re-checked.
const steerAckPoll = 150 * time.Millisecond

// steerAckGrace is how long a runtime without a turn signal (no status hooks and
// no transcript write yet) must stay alive after its text could have been
// submitted before the prompt counts as taken.
const steerAckGrace = 2 * time.Second

type postActionKey struct{}

// postActionSlot carries a wait that must run after handle has released every
// admission lock the action took: a deferred cold start reacquires the session's
// admission itself, so waiting for it under the action's own locks would
// deadlock.
type postActionSlot struct {
	wait func(context.Context) error
}

func withPostActionSlot(ctx context.Context) (context.Context, *postActionSlot) {
	slot := &postActionSlot{}
	return context.WithValue(ctx, postActionKey{}, slot), slot
}

func postActionFrom(ctx context.Context) *postActionSlot {
	slot, _ := ctx.Value(postActionKey{}).(*postActionSlot)
	return slot
}

// coldStartReporter returns the callback a deferred cold start reports its
// outcome through. When the caller can wait (handle installed a slot), the
// result becomes the action's result. A caller that holds admission across the
// action (an amux-originated PR wake) cannot wait without deadlocking, so it
// gets nil: the old acknowledgement, with start failures still journaled and
// no acknowledgement polling that no one would read.
func coldStartReporter(ctx context.Context) func(error) {
	slot := postActionFrom(ctx)
	if slot == nil {
		return nil
	}
	done := make(chan error, 1)
	slot.wait = func(waitCtx context.Context) error {
		select {
		case err := <-done:
			return err
		case <-waitCtx.Done():
			return fmt.Errorf("prompt: stopped waiting for the agent to start: %w", waitCtx.Err())
		}
	}
	return func(err error) {
		select {
		case done <- err:
		default:
		}
	}
}

func (d *Daemon) verifyTimeout() time.Duration {
	if d.steerVerify > 0 {
		return d.steerVerify
	}
	return steerVerifyTimeout
}

// exitReporter is the optional engine.Instance surface that explains an exit.
type exitReporter interface {
	ExitMessage() string
}

func exitDetail(in engine.Instance) string {
	if r, ok := in.(exitReporter); ok {
		if msg := r.ExitMessage(); msg != "" {
			return ": " + msg
		}
	}
	return ""
}

// awaitPromptAck waits until the runtime a cold-start prompt launched has taken
// the prompt. queued is when the text entered the runtime's input FIFO; typedBy
// is the earliest moment it can have been submitted (boot settle plus the
// harness's submit delay). It fails when the runtime exits, is replaced, or
// stays silent past the verify timeout.
func (d *Daemon) awaitPromptAck(ctx context.Context, id string, in engine.Instance, queued, typedBy time.Time) error {
	key := in.Key()
	deadline := time.NewTimer(d.verifyTimeout())
	defer deadline.Stop()
	tick := time.NewTicker(steerAckPoll)
	defer tick.Stop()
	for {
		if !in.Alive() {
			return fmt.Errorf("agent %s exited while starting, before it took the prompt%s", id, exitDetail(in))
		}
		if cur, ok := d.engine.Lookup(key); !ok || cur != in {
			return fmt.Errorf("agent %s was stopped or replaced while starting, before it took the prompt", id)
		}
		if promptAcknowledged(id, queued, typedBy, time.Now()) {
			return nil
		}
		select {
		case <-deadline.C:
			return fmt.Errorf("agent %s started but did not acknowledge the prompt within %v; it is running, so retry with verb=interject or check `amux status`", id, d.verifyTimeout())
		case <-tick.C:
		case <-ctx.Done():
			return fmt.Errorf("prompt to %s: %w", id, ctx.Err())
		}
	}
}

// promptAcknowledged reports whether the session shows evidence that the
// prompt typed after queued became a turn:
//   - a status hook reported a turn (running) or a blocking prompt (waiting)
//     after the text was queued — Claude's UserPromptSubmit; or
//   - the runtime's transcript was written after the text could be submitted;
//   - for a runtime with no status hooks, it stayed alive steerAckGrace past
//     the submit, which is the most the PTY can prove.
//
// A "ready" report is not evidence: SessionStart reports ready at boot, which
// can come after the text was queued.
func promptAcknowledged(id string, queued, typedBy, now time.Time) bool {
	sess, ok, err := lookupSession(id)
	if err != nil || !ok {
		return false
	}
	if rec, ok := core.SessionHookState(sess.ID, sess.ClaudeID); ok && rec.Updated >= queued.UnixMilli() {
		switch rec.State {
		case core.StateRunning, core.StateWaiting:
			return true
		}
	}
	if now.Before(typedBy) {
		return false
	}
	if path, ok := agent.HarnessFor(sess.Agent).RuntimeTranscriptPath(sess); ok && path != "" {
		if fi, err := os.Stat(path); err == nil && fi.ModTime().After(typedBy) {
			return true
		}
	}
	return !reportsTurns(sess.Agent) && now.After(typedBy.Add(steerAckGrace))
}

// reportsTurns reports whether a runtime announces each turn through amux's
// status hooks, so its silence is meaningful.
func reportsTurns(kind string) bool {
	return agent.Canonical(kind) == harnessproto.RuntimeClaude
}

// submitBy is the earliest moment a cold-start payload can have been submitted:
// the boot settle plus every delay between its writes.
func submitBy(queued time.Time, payload []engine.InputStep) time.Time {
	var total time.Duration
	for i, step := range payload {
		if i == 0 {
			total += step.NotBeforeAge
		}
		total += step.DelayBefore
	}
	return queued.Add(total)
}
