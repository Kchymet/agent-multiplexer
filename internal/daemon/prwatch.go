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
func (d *Daemon) deliverPRWake(ctx context.Context, workgroup string, wakes []prwatch.Wake) []prwatch.Delivery {
	results := make([]prwatch.Delivery, len(wakes))
	for i := range results {
		results[i] = prwatch.Deferred
	}
	if len(wakes) == 0 {
		return results
	}

	// Membership moves and coordinator-grant changes take the effect gate
	// exclusively. Member archive/delete and repo changes take these session
	// locks; repo creation/rebinding and repo-set changes also take creationMu.
	// Hold the same ordered admission through the final store read and exact
	// prompt admission so none of those mutations can cross the disclosure.
	ids := make([]string, 1, len(wakes)+1)
	ids[0] = workgroup
	for _, wake := range wakes {
		ids = append(ids, wake.SessionID)
	}
	release := d.admitSessions(ids...)
	defer release()
	d.creationMu.Lock()
	defer d.creationMu.Unlock()
	if d.prWakeAdmission != nil {
		d.prWakeAdmission()
	}

	sess, eligible, dropped, err := resolvePRWakes(workgroup, wakes)
	if err != nil {
		log.Printf("prwatch: revalidate wake-up for %s: %v", workgroup, err)
		return results
	}
	for _, i := range dropped {
		results[i] = prwatch.Dropped
	}
	if len(eligible) == 0 {
		return results
	}
	if ready, why := d.coordinatorTakesWake(sess); !ready {
		if why != "" {
			log.Printf("prwatch: wake-up for %s deferred: %s", workgroup, why)
		}
		return results
	}
	authorized := make([]prwatch.Wake, 0, len(eligible))
	for _, i := range eligible {
		authorized = append(authorized, wakes[i])
	}
	text := prWakeText(agent.HookGoals(sess), prwatch.WakeLines(authorized))
	if err := d.beginPRWake(ctx, sess, text); err != nil {
		log.Printf("prwatch: wake-up for %s failed: %v", workgroup, err)
		return results
	}
	for _, i := range eligible {
		results[i] = prwatch.Delivered
	}
	log.Printf("prwatch: woke coordinator %s (%d PR update(s))", workgroup, len(eligible))
	return results
}

// resolvePRWakes re-reads every authority-bearing identity from the store.
// Archived members remain eligible while their current RootID and repository
// binding remain in scope; missing, moved, deleted, rebranched, ungranted, or
// rebound entries are definitive drops and are never rerouted.
func resolvePRWakes(workgroup string, wakes []prwatch.Wake) (root store.Session, eligible, dropped []int, err error) {
	db, err := store.OpenReadOnly()
	if err != nil {
		return store.Session{}, nil, nil, err
	}
	defer db.Close()
	root, ok, err := db.GetSession(workgroup)
	if err != nil {
		return store.Session{}, nil, nil, err
	}
	if !ok || root.Archived || root.Role() != store.RoleCoordinator {
		for i := range wakes {
			dropped = append(dropped, i)
		}
		return root, nil, dropped, nil
	}
	grants, initialized, err := db.CoordinatorRepoGrants(workgroup)
	if err != nil {
		return store.Session{}, nil, nil, err
	}
	granted := make(map[string]bool, len(grants))
	for _, repo := range grants {
		granted[repo] = true
	}
	for i, wake := range wakes {
		if wake.Workgroup != workgroup || wake.SessionID == "" || wake.Repo == "" ||
			wake.Branch == "" || !prwatch.Owns(wake.Branch, wake.PRBranch) {
			dropped = append(dropped, i)
			continue
		}
		member, found, err := db.GetSession(wake.SessionID)
		if err != nil {
			return store.Session{}, nil, nil, err
		}
		if !found || member.IsRoot() || member.RootID != workgroup || member.Branch != wake.Branch {
			dropped = append(dropped, i)
			continue
		}
		bound := false
		for _, name := range store.SplitRepos(member.Repo) {
			if !initialized || !granted[name] {
				continue
			}
			repo, found, err := db.Repo(name)
			if err != nil {
				return store.Session{}, nil, nil, err
			}
			if found && prwatch.RepoSlug(repo.Source) == wake.Repo {
				bound = true
				break
			}
		}
		if !bound {
			dropped = append(dropped, i)
			continue
		}
		eligible = append(eligible, i)
	}
	return root, eligible, dropped, nil
}

// beginPRWake admits the prompt while the caller holds the coordinator/member
// delivery admission. A live structured coordinator splits turn/start from the
// long model turn: BeginPrompt is the bounded effect protected here, while its
// waiter runs under daemon lifecycle ownership after these locks are released.
func (d *Daemon) beginPRWake(ctx context.Context, sess store.Session, text string) error {
	if d.codex != nil {
		if sup, ok := d.codex.Get(sess.ID); ok {
			return d.beginStructuredPRWake(ctx, sess.ID, sup, text)
		}
	}
	res := d.handle(withEffectAdmission(ctx), core.Action{Action: core.ActionSteer, ID: sess.ID, Fields: map[string]string{
		core.SteerVerb: core.SteerPrompt, core.SteerText: text,
	}})
	if !res.OK {
		return fmt.Errorf("%s", res.Error)
	}
	return nil
}

func (d *Daemon) beginStructuredPRWake(ctx context.Context, id string, starter structuredPromptStarter, text string) error {
	waiter := make(chan func(context.Context) error, 1)
	if !d.startDeferredContext(ctx, func(workCtx context.Context) {
		wait := <-waiter
		if wait == nil {
			return
		}
		if err := wait(workCtx); err != nil {
			structuredJournal(id, core.JournalError, fmt.Sprintf("prompt: %v", err))
		}
	}) {
		return context.Canceled
	}
	admitCtx, cancel := context.WithTimeout(ctx, steerEffectAdmissionTimeout)
	defer cancel()
	wait, err := starter.BeginPrompt(admitCtx, text)
	if err != nil || wait == nil {
		waiter <- nil
		if err != nil {
			return err
		}
		return fmt.Errorf("prompt runtime returned no turn waiter")
	}
	waiter <- wait
	return nil
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
	status, goalKnown := d.coordinatorGoalStatusKnown(sess)
	if !goalKnown {
		return false, "goal state is unknown"
	}
	if status != "" {
		switch status {
		case claudegoal.StatusActive, claudegoal.StatusWaiting, "none":
		default:
			return false, "goal is " + status
		}
	}
	if d.coordinatorHasOpenPermission(sess) {
		return false, "permission is open"
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
	case engine.ActivityUnknown:
		return false, "readiness is unknown"
	}
	return false, ""
}

func (d *Daemon) coordinatorHasOpenPermission(sess store.Session) bool {
	if d.codex != nil {
		if sup, ok := d.codex.Get(sess.ID); ok && len(sup.OpenApprovals()) > 0 {
			return true
		}
	}
	// Legacy/diagnostic observations are not authority to answer a permission,
	// but an unresolved one is sufficient reason not to inject automatic input.
	if len(core.PendingPermissions(sess.ID)) > 0 {
		return true
	}
	rec := permissionRecord(agent.HarnessFor(sess.Agent), sess)
	return rec.Path != "" && len(runtimeevents.OpenPermissions(rec)) > 0
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
	status, _ := d.coordinatorGoalStatusKnown(sess)
	return status
}

// coordinatorGoalStatusKnown distinguishes a coordinator with no goal from a
// goal coordinator whose state could not be read. Status surfaces retain the
// empty-string fallback, but automatic wake-up fails closed on known=false.
func (d *Daemon) coordinatorGoalStatusKnown(sess store.Session) (string, bool) {
	switch {
	case agent.HookGoals(sess):
		st, err := readHookGoal(sess)
		if err != nil {
			return "", false
		}
		if st.None() {
			return "none", true
		}
		// A coordinator that ran `goal wait` and ended its turn has its goal
		// flipped back to active by the Stop hook, keeping the wait note: that
		// is the idle "waiting on a wake-up" state, not active work.
		if st.Status == claudegoal.StatusActive && st.Note != "" {
			return claudegoal.StatusWaiting, true
		}
		return st.Status, true
	case agent.NativeGoals(sess):
		if d.codex == nil {
			return "", false
		}
		sup, ok := d.codex.Get(sess.ID)
		if !ok {
			return "", false
		}
		g, ok := sup.Goal()
		if !ok {
			return "none", true
		}
		return g.Status, true
	}
	return "", true
}
