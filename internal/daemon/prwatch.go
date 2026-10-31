package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"amux/internal/agent"
	"amux/internal/claudegoal"
	"amux/internal/codexapp"
	"amux/internal/core"
	"amux/internal/engine"
	"amux/internal/panespec"
	"amux/internal/prwatch"
	"amux/internal/runtimeevents"
	"amux/internal/store"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// This file wires internal/prwatch into the daemon: which sessions it watches,
// where their PR state persists, how a transition reaches the member's event
// stream, and how a wake-up reaches the coordinator.
//
// The poller runs in the daemon (host side) with the host user's own gh login,
// never inside a sandbox: agents keep no GitHub authority of their own for it,
// and one poller per machine is what keeps the gh call rate bounded.

// prPollIntervalEnv overrides the PR poll interval (a Go duration, e.g. "90s";
// "off" or "0" disables PR watching).
const prPollIntervalEnv = "AMUX_PR_POLL_INTERVAL"

// prWakeMarker opens every wake-up typed into a Claude goal coordinator. It is
// claudegoal's continuation marker, so the coordinator's UserPromptSubmit hook
// treats the wake-up as continuing the current goal rather than as a new task
// that would replace it (a test pins this against claudegoal.OnPrompt).
const prWakeMarker = "[amux goal]"

// prPollInterval resolves the configured interval; ok=false disables watching.
func prPollInterval() (time.Duration, bool) {
	v := strings.TrimSpace(os.Getenv(prPollIntervalEnv))
	switch strings.ToLower(v) {
	case "":
		return prwatch.DefaultInterval, true
	case "off", "0", "false", "no":
		return 0, false
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		log.Printf("prwatch: ignoring %s=%q (want a duration like 3m, or off)", prPollIntervalEnv, v)
		return prwatch.DefaultInterval, true
	}
	if d < 30*time.Second {
		d = 30 * time.Second // stay friendly to GitHub's rate limits
	}
	return d, true
}

func (d *Daemon) newPRWatcher() *prwatch.Watcher {
	interval, _ := prPollInterval()
	return prwatch.New(prwatch.Config{
		Run:      prwatch.ExecRunner,
		Members:  prWatchMembers,
		Load:     loadPRStates,
		Save:     savePRState,
		Emit:     d.emitPRState,
		Deliver:  d.deliverPRWake,
		Interval: interval,
	})
}

// prWatchLoop runs the PR watcher until ctx ends (unless disabled).
func (d *Daemon) prWatchLoop(ctx context.Context) {
	if d.prs == nil {
		return
	}
	if _, ok := prPollInterval(); !ok {
		log.Printf("prwatch: disabled by %s", prPollIntervalEnv)
		return
	}
	d.prs.Run(ctx)
}

// sessionPRs is the workspace source's view of the watcher's state.
func (d *Daemon) sessionPRs(id string) []harnessproto.SessionPR {
	if d.prs == nil {
		return nil
	}
	return d.prs.PRs(id)
}

// prWatchMembers lists the active members of active workgroups, with their
// branch and the GitHub repositories they work in.
func prWatchMembers() ([]prwatch.Member, error) {
	db, err := store.OpenReadOnly()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	all, err := db.AllSessions()
	if err != nil {
		return nil, err
	}
	repos, err := db.Repos()
	if err != nil {
		return nil, err
	}
	slugs := map[string]string{}
	for _, r := range repos {
		slugs[r.Name] = prwatch.RepoSlug(r.Source)
	}
	roots := map[string]store.Session{}
	for _, s := range all {
		if s.IsRoot() {
			roots[s.ID] = s
		}
	}
	var out []prwatch.Member
	for _, s := range all {
		if s.IsRoot() || s.Archived || s.Branch == "" {
			continue
		}
		root, ok := roots[s.RootID]
		if !ok || root.Archived || root.Role() != store.RoleCoordinator {
			continue
		}
		var gh []string
		for _, name := range store.SplitRepos(s.Repo) {
			if slug := slugs[name]; slug != "" {
				gh = append(gh, slug)
			}
		}
		if len(gh) == 0 {
			continue
		}
		out = append(out, prwatch.Member{SessionID: s.ID, Workgroup: s.RootID, Label: s.Name, Branch: s.Branch, Repos: gh})
	}
	return out, nil
}

func loadPRStates() (map[string]string, error) {
	db, err := store.OpenReadOnly()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	return db.SessionPRStates()
}

func savePRState(id, state string) error {
	db, err := store.Open()
	if err != nil {
		return err
	}
	defer db.Close()
	return db.PutSessionPRState(id, state, store.Now())
}

// prStatePayload is the pr_state event payload.
type prStatePayload struct {
	Session string                 `json:"session"`
	PR      harnessproto.SessionPR `json:"pr"`
	Changes []string               `json:"changes"`
	Wake    bool                   `json:"wake"`
}

// emitPRState records a PR transition on the member's own event stream (its
// amux journal, plus its structured log when that is the stream's source).
func (d *Daemon) emitPRState(c prwatch.Change) {
	payload, err := json.Marshal(prStatePayload{Session: c.SessionID, PR: c.PR, Changes: c.Changes, Wake: len(c.Wake) > 0})
	if err != nil {
		return
	}
	text := fmt.Sprintf("PR %s#%d: %s", c.PR.Repo, c.PR.Number, strings.Join(c.Changes, "; "))
	if err := core.AppendJournalRecord(c.SessionID, core.JournalRecord{
		Level: core.JournalInfo, Text: text, Type: harnessproto.TypePRState, Payload: payload,
	}); err != nil {
		log.Printf("prwatch: journal %s: %v", c.SessionID, err)
	}
	if s, ok, err := lookupSession(c.SessionID); err == nil && ok && d.structuredResolvable(s) {
		ev := harnessproto.RuntimeEvent{Type: harnessproto.TypePRState, Direction: harnessproto.DirMeta, Payload: payload}
		if err := codexapp.AppendEvent(c.SessionID, ev); err != nil {
			log.Printf("prwatch: structured event %s: %v", c.SessionID, err)
		}
	}
	d.triggerPoll() // refresh the rail's PR suffix now, not at the next tick
}

// deliverPRWake prompts a workgroup's coordinator with queued PR wake-ups.
//
// The mechanism is the ordinary steer `prompt` verb through handle — the same
// admission, routing and delivery as a host's `amux do prompt` — but only when
// it is safe for the prompt to land as a fresh turn:
//
//   - the coordinator is live (a stopped coordinator is not cold-started: it was
//     stopped deliberately, and a restart re-checks its wake-ups anyway);
//   - it is between turns: not mid-turn (where typed text would be queued
//     behind, or interleaved with, the turn's work) and no permission prompt or
//     question is open (where keystrokes would answer it);
//   - for a goal coordinator, its goal is one the wake-up may continue (active
//     or waiting, or no goal at all) — never a paused, completed, blocked goal
//     or one waiting on the user's answer, which only the user may move.
//
// Anything else is deferred and retried; prwatch bounds how long it waits.
func (d *Daemon) deliverPRWake(ctx context.Context, workgroup string, lines []string) prwatch.Delivery {
	sess, ok, err := lookupSession(workgroup)
	if err != nil {
		return prwatch.Deferred
	}
	if !ok || sess.Archived || sess.Role() != store.RoleCoordinator {
		return prwatch.Dropped
	}
	if ready, why := d.coordinatorTakesWake(sess); !ready {
		if why != "" {
			log.Printf("prwatch: wake-up for %s deferred: %s", workgroup, why)
		}
		return prwatch.Deferred
	}
	text := prWakeText(agent.HookGoals(sess), lines)
	res := d.handle(ctx, core.Action{Action: core.ActionSteer, ID: sess.ID, Fields: map[string]string{
		core.SteerVerb: core.SteerPrompt, core.SteerText: text,
	}})
	if !res.OK {
		log.Printf("prwatch: wake-up for %s failed: %s", workgroup, res.Error)
		return prwatch.Deferred
	}
	log.Printf("prwatch: woke coordinator %s (%d PR update(s))", workgroup, len(lines))
	return prwatch.Delivered
}

// prWakeText renders the wake-up prompt.
func prWakeText(hookGoal bool, lines []string) string {
	head := "[amux] PR update"
	if hookGoal {
		head = prWakeMarker + " PR update"
	}
	var b strings.Builder
	b.WriteString(head)
	b.WriteString(" (amux PR watcher wake-up, not a new task):")
	for _, l := range lines {
		b.WriteString("\n- ")
		b.WriteString(l)
	}
	b.WriteString("\nCheck these PRs and act on them as your task requires.")
	return b.String()
}

// coordinatorTakesWake decides whether a wake-up may be typed into the
// coordinator now; why explains a refusal worth logging ("" for the routine
// "busy" or "stopped").
func (d *Daemon) coordinatorTakesWake(sess store.Session) (bool, string) {
	if d.engine == nil {
		return false, ""
	}
	live := false
	if d.codex != nil {
		if _, ok := d.codex.Get(sess.ID); ok {
			live = true
		}
	}
	if !live {
		_, live = d.engine.Lookup(engine.Key{AgentID: sess.ID, Tab: panespec.TabAgent})
	}
	if !live {
		return false, ""
	}
	if status := d.coordinatorGoalStatus(sess); status != "" {
		switch status {
		case claudegoal.StatusActive, claudegoal.StatusWaiting, "none":
		default:
			return false, "goal is " + status
		}
	}
	h := agent.HarnessFor(sess.Agent)
	switch h.Activity(sess) {
	case engine.ActivitySafe:
		return true, ""
	case engine.ActivityBusy:
		// Claude reports "waiting" both for a prompt that needs the user and for
		// its idle reminder after a finished turn. Only the latter is safe.
		if h.RailState(sess) == core.StateWaiting && claudeIdleAfterTurn(h, sess) {
			return true, ""
		}
	}
	return false, ""
}

// claudeIdleAfterTurn reports whether a "waiting" Claude session is merely
// idle: no permission prompt is open and its transcript's last turn ended
// (a pending tool call such as a question to the user means it is not).
func claudeIdleAfterTurn(h agent.Harness, sess store.Session) bool {
	rec := permissionRecord(h, sess)
	if rec.Path == "" || agent.Canonical(sess.Agent) != harnessproto.RuntimeClaude {
		return false
	}
	if len(runtimeevents.OpenPermissions(rec)) > 0 {
		return false
	}
	return lastTurnEnded(rec.Path)
}

// transcriptTailBytes bounds how much of a transcript lastTurnEnded reads.
const transcriptTailBytes = 256 << 10

// lastTurnEnded reports whether the last substantive event in a Claude
// transcript's tail is a turn_end.
func lastTurnEnded(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.Size() > transcriptTailBytes {
		if _, err := f.Seek(info.Size()-transcriptTailBytes, io.SeekStart); err != nil {
			return false
		}
	}
	b, err := io.ReadAll(io.LimitReader(f, transcriptTailBytes))
	if err != nil {
		return false
	}
	lines := strings.Split(string(b), "\n")
	if len(lines) > 0 && len(b) == transcriptTailBytes {
		lines = lines[1:] // the first line is probably partial
	}
	st := &runtimeevents.ClaudeState{}
	last := ""
	for _, line := range lines {
		for _, ev := range runtimeevents.MapClaudeLine([]byte(line), st) {
			switch ev.Type {
			case harnessproto.TypeRaw, harnessproto.TypeNotice, harnessproto.TypeUsage:
			default:
				last = ev.Type
			}
		}
	}
	return last == harnessproto.TypeTurnEnd
}

// coordinatorGoalStatus reads a goal coordinator's goal status for status
// surfaces and wake gating: a claudegoal/App Server status word, "none" for a
// goal coordinator with no goal, or "" when the session is not in goal mode or
// its goal is unknown (a Codex coordinator whose runtime is not up).
func (d *Daemon) coordinatorGoalStatus(sess store.Session) string {
	switch {
	case agent.HookGoals(sess):
		st, err := readHookGoal(sess)
		if err != nil {
			return ""
		}
		if st.None() {
			return "none"
		}
		// A coordinator that ran `goal wait` and ended its turn has its goal
		// flipped back to active by the Stop hook, keeping the wait note: that
		// is the idle "waiting on a wake-up" state, not active work.
		if st.Status == claudegoal.StatusActive && st.Note != "" {
			return claudegoal.StatusWaiting
		}
		return st.Status
	case agent.NativeGoals(sess):
		if d.codex == nil {
			return ""
		}
		sup, ok := d.codex.Get(sess.ID)
		if !ok {
			return ""
		}
		g, ok := sup.Goal()
		if !ok {
			return "none"
		}
		return g.Status
	}
	return ""
}
