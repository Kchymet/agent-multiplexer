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
	"amux/internal/items"
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
const prWakeMarker = claudegoal.Marker

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
	interval, enabled := prPollInterval()
	return prwatch.New(prwatch.Config{
		Run:      prwatch.ExecRunner,
		Members:  prWatchMembers,
		Load:     loadPRStates,
		Save:     savePRState,
		Emit:     d.emitPRState,
		Deliver:  d.deliverPRWake,
		Interval: interval,
		NoPoll:   !enabled,
	})
}

// prWatchLoop runs the PR watcher until ctx ends. With PR polling disabled it
// still runs, without polling GitHub, to deliver member wake-ups.
func (d *Daemon) prWatchLoop(ctx context.Context) {
	if d.prs == nil {
		return
	}
	if _, ok := prPollInterval(); !ok {
		log.Printf("prwatch: PR polling disabled by %s; member wake-ups stay on", prPollIntervalEnv)
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
	if items.IsGlobalInbox(workgroup) {
		// Only item notices are addressed to the global inbox owner (items.go).
		return d.deliverGlobalInboxWakes(ctx, wakes)
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

	sess, eligible, dropped, err := resolvePRWakes(workgroup, wakes, d.sessionStateNow)
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
	// A member's activity change is not worth starting a turn for a goal
	// coordinator with no goal to continue (memberWakesWanted). A PR update
	// keeps its own rule.
	memberWakes := memberWakesWanted(d.coordinatorGoalStatus(sess))
	kept := eligible[:0]
	for _, i := range eligible {
		if wakes[i].Kind == prwatch.KindMember && !memberWakes {
			results[i] = prwatch.Dropped
			continue
		}
		kept = append(kept, i)
	}
	eligible = kept
	if len(eligible) == 0 {
		return results
	}
	// An items wake says nothing itself: it stands for the coordinator's
	// unnotified items, read now. None left (all triaged meanwhile) drops it.
	var itemsShown, itemsAll []items.Item
	kept = eligible[:0]
	for _, i := range eligible {
		if wakes[i].Kind == prwatch.KindItems {
			if itemsAll == nil {
				shown, all, err := d.loadItemWake(workgroup)
				if err != nil {
					log.Printf("items: wake-up for %s: %v", workgroup, err)
					return results
				}
				itemsShown, itemsAll = shown, all
			}
			if len(itemsAll) == 0 {
				results[i] = prwatch.Dropped
				continue
			}
		}
		kept = append(kept, i)
	}
	eligible = kept
	if len(eligible) == 0 {
		return results
	}
	authorized := make([]prwatch.Wake, 0, len(eligible))
	kinds := wakeKinds{}
	for _, i := range eligible {
		switch wakes[i].Kind {
		case prwatch.KindItems:
			kinds.items = true
			continue
		case prwatch.KindMember:
			kinds.members = true
		}
		authorized = append(authorized, wakes[i])
	}
	lines := prwatch.WakeLines(authorized)
	if kinds.items {
		lines = append(lines, itemWakeLines(itemsShown, len(itemsAll))...)
	}
	text := prWakeText(agent.HookGoals(sess), lines, kinds)
	outcome, err := d.beginPRWake(ctx, sess, text)
	if err != nil {
		if outcome == prwatch.Indeterminate {
			for _, i := range eligible {
				results[i] = prwatch.Indeterminate
			}
			log.Printf("prwatch: wake-up for %s indeterminate; automatic retry suppressed: %v", workgroup, err)
			return results
		}
		log.Printf("prwatch: wake-up for %s deferred: %v", workgroup, err)
		return results
	}
	if outcome != prwatch.Delivered {
		log.Printf("prwatch: wake-up for %s returned unexpected admission outcome %d", workgroup, outcome)
		return results
	}
	for _, i := range eligible {
		results[i] = prwatch.Delivered
	}
	if kinds.items {
		if err := d.items.MarkNotified(workgroup, itemsAll); err != nil {
			log.Printf("items: mark %s items notified: %v", workgroup, err)
		}
	}
	log.Printf("prwatch: woke coordinator %s (%d update(s))", workgroup, len(eligible))
	return results
}

// memberWakesWanted reports whether a coordinator with this goal status takes
// member activity wake-ups: a goal being worked (active) or waiting on a
// wake-up, or a coordinator outside goal mode (""), which takes PR wake-ups on
// the same terms. A goal coordinator with no goal ("none") has nothing to
// continue and is not started on a turn for a member.
func memberWakesWanted(goal string) bool {
	return goal == "" || goal == claudegoal.StatusActive || goal == claudegoal.StatusWaiting
}

// wakeKinds records which non-PR kinds a wake-up batch carries; it picks the
// prompt's wording.
type wakeKinds struct {
	members bool // a member's activity changed
	items   bool // items were raised, escalated or carried over to the recipient
}

// resolvePRWakes re-reads every authority-bearing identity from the store.
// Archived members remain eligible while their current RootID and repository
// binding remain in scope; missing, moved, deleted, rebranched, ungranted, or
// rebound entries are definitive drops and are never rerouted.
//
// A member wake (prwatch.KindMember) carries no repository: it is eligible
// while the member is still an active member of this workgroup and is still in
// the state it reports (stateNow) — a member that moved on since (steered
// again, restarted) is a definitive drop, not stale news for the coordinator.
func resolvePRWakes(workgroup string, wakes []prwatch.Wake, stateNow func(id string) string) (root store.Session, eligible, dropped []int, err error) {
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
		if wake.Kind == prwatch.KindItems {
			// Addressed to the coordinator itself; what it says is read from the
			// item store at delivery, under this admission.
			if wake.Workgroup != workgroup || wake.SessionID != workgroup {
				dropped = append(dropped, i)
				continue
			}
			eligible = append(eligible, i)
			continue
		}
		if wake.Kind == prwatch.KindMember {
			if wake.Workgroup != workgroup || wake.SessionID == "" || stateNow == nil || stateNow(wake.SessionID) != wake.State {
				dropped = append(dropped, i)
				continue
			}
			member, found, err := db.GetSession(wake.SessionID)
			if err != nil {
				return store.Session{}, nil, nil, err
			}
			if !found || member.IsRoot() || member.Archived || member.RootID != workgroup {
				dropped = append(dropped, i)
				continue
			}
			eligible = append(eligible, i)
			continue
		}
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
func (d *Daemon) beginPRWake(ctx context.Context, sess store.Session, text string) (prwatch.Delivery, error) {
	if d.codex != nil {
		if sup, ok := d.codex.Get(sess.ID); ok {
			return d.beginStructuredPRWake(ctx, sess.ID, sup, text)
		}
	}
	res := d.handle(withEffectAdmission(ctx), core.Action{Action: core.ActionSteer, ID: sess.ID, Fields: map[string]string{
		core.SteerVerb: core.SteerPrompt, core.SteerText: text,
	}})
	if !res.OK {
		return prwatch.Deferred, fmt.Errorf("%s", res.Error)
	}
	return prwatch.Delivered, nil
}

func (d *Daemon) beginStructuredPRWake(ctx context.Context, id string, starter structuredPromptStarter, text string) (prwatch.Delivery, error) {
	d.noteSteer(id)
	if err := ctx.Err(); err != nil {
		return prwatch.Deferred, err
	}
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
		return prwatch.Deferred, context.Canceled
	}
	admitCtx, cancel := context.WithTimeout(ctx, steerEffectAdmissionTimeout)
	defer cancel()
	if err := admitCtx.Err(); err != nil {
		waiter <- nil
		return prwatch.Deferred, err
	}
	wait, err := starter.BeginPrompt(admitCtx, text)
	if err != nil || wait == nil {
		waiter <- nil
		if err == nil {
			err = fmt.Errorf("prompt runtime returned no turn waiter")
		}
		structuredJournal(id, core.JournalError, fmt.Sprintf("PR watcher turn-start outcome is unknown; automatic retry suppressed; reconcile PR state manually: %v", err))
		return prwatch.Indeterminate, err
	}
	waiter <- wait
	return prwatch.Delivered, nil
}

// prWakeText renders the wake-up prompt. A batch with a member activity
// change is a workgroup update; one of PR transitions only keeps the PR
// watcher's wording.
func prWakeText(hookGoal bool, lines []string, kinds wakeKinds) string {
	head, what, check := "PR update", "amux PR watcher wake-up", "these PRs"
	switch {
	case kinds.members:
		head, what, check = "Workgroup update", "amux wake-up", "these members and PRs"
	case kinds.items:
		head, what, check = "Items raised", "amux wake-up", "these items"
	}
	prefix := "[amux]"
	if hookGoal {
		prefix = prWakeMarker
	}
	var b strings.Builder
	b.WriteString(prefix + " " + head + " (" + what + ", not a new task):")
	for _, l := range lines {
		b.WriteString("\n- ")
		b.WriteString(l)
	}
	b.WriteString("\nCheck " + check + " and act on them as your task requires.")
	if kinds.items {
		b.WriteString(" " + itemsTriageHint)
	}
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

// transcriptTailBytes bounds how much of a transcript lastTurnEnded and
// claudeQuestionOpenIn read.
const transcriptTailBytes = 256 << 10

// transcriptTail reads the last transcriptTailBytes of a transcript as lines,
// dropping a first line the cut probably split. ok is false when the file
// cannot be read.
func transcriptTail(path string) (lines []string, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.Size() > transcriptTailBytes {
		if _, err := f.Seek(info.Size()-transcriptTailBytes, io.SeekStart); err != nil {
			return nil, false
		}
	}
	b, err := io.ReadAll(io.LimitReader(f, transcriptTailBytes))
	if err != nil {
		return nil, false
	}
	lines = strings.Split(string(b), "\n")
	if len(lines) > 0 && len(b) == transcriptTailBytes {
		lines = lines[1:] // the first line is probably partial
	}
	return lines, true
}

// lastTurnEnded reports whether the last substantive event in a Claude
// transcript's tail is a turn_end.
func lastTurnEnded(path string) bool {
	lines, ok := transcriptTail(path)
	if !ok {
		return false
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

// coordinatorGoalInfo is coordinatorGoalStatus plus the goal's note (what a
// hook-goal coordinator is waiting on, the question it asked, why it is
// blocked) for the coordinator row's supervision detail. A native (Codex) goal
// carries no such note.
func (d *Daemon) coordinatorGoalInfo(sess store.Session) (status, note string) {
	status, _ = d.coordinatorGoalStatusKnown(sess)
	if status == "" || status == "none" || !agent.HookGoals(sess) {
		return status, ""
	}
	if st, err := readHookGoal(sess); err == nil {
		note = st.Note
	}
	return status, note
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
		g, present, known := sup.ObservedGoal()
		if !known {
			return "", false
		}
		if !present {
			return "none", true
		}
		return g.Status, true
	}
	return "", true
}
