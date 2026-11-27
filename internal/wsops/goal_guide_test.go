package wsops

import (
	"strings"
	"testing"

	"amux/internal/agent"
	"amux/internal/claudegoal"
	"amux/internal/store"
)

// A coordinator's guide must describe the lifecycle it actually has, and teach
// only the operations its mailbox grant authorizes: a guide that promises goal
// mode on a harness without one, or advertises a host-only operation, sends the
// model into failures the user then has to unpick.
func TestCoordinatorGuideDescribesItsRealLifecycle(t *testing.T) {
	root := store.Session{ID: "wg1", Name: "payments", Agent: agent.GoalRuntime, Dir: "/sandbox/wg1"}
	if !agent.NativeGoals(root) {
		t.Fatal("fixture is not a goal coordinator")
	}
	guide := coordinatorGuide(root)
	for _, want := range []string{
		"native", "update_goal", "complete",
		"request_user_input",             // ask only for genuine input
		"amux do steer wg1 -f verb=goal", // the user's pause/resume/clear
		"amux add wg1",
		"amux archive <member>",
		"choose the sensible default", // decide and keep going
	} {
		if !strings.Contains(guide, want) {
			t.Errorf("goal coordinator guide missing %q", want)
		}
	}
	// Restoring an archived member is the user's operation, not the
	// coordinator's: its grant refuses restoration.
	if strings.Contains(guide, "amux do unarchive") {
		t.Error("coordinator guide advertises the host-only amux do unarchive")
	}
	assertCoordinatorWrapsUp(t, "goal coordinator", guide, "wg1")

	// A Claude coordinator runs in goal mode by default, through amux's hooks:
	// its guide teaches the amux goal commands and how to get missing input
	// from the user, never Codex's update_goal.
	claude := store.Session{ID: "wg3", Name: "payments", Agent: agent.HookGoalRuntime, Dir: "/sandbox/wg3"}
	if !agent.HookGoals(claude) || !agent.GoalMode(claude) {
		t.Fatal("claude coordinator fixture is not in goal mode")
	}
	hooked := coordinatorGuide(claude)
	flatHooked := strings.Join(strings.Fields(hooked), " ")
	for _, want := range []string{
		"Goal mode (amux goal hooks)",
		"goal mode by default",
		"amux agent goal complete",
		"amux agent goal ask",
		"amux agent goal blocked",
		"amux agent goal wait",                            // idle until a wake-up
		"Never busy-poll",                                 // no polling across continuations
		"amux wakes you",                                  // event-driven, no polling loop
		"Workgroup update",                                // the wake-up prompt it will see
		"AskUserQuestion",                                 // concrete choices
		"ask instead of guessing",                         // user-owned decisions
		"choose the sensible default",                     // optional details
		"amux do steer wg3 -f verb=goal -f status=paused", // the user's controls
	} {
		if !strings.Contains(flatHooked, want) {
			t.Errorf("claude goal coordinator guide missing %q", want)
		}
	}
	for _, forbidden := range []string{"update_goal", "no native goal engine", "request_user_input"} {
		if strings.Contains(hooked, forbidden) {
			t.Errorf("claude goal coordinator guide mentions %q", forbidden)
		}
	}
	assertCoordinatorWrapsUp(t, "claude goal coordinator", hooked, "wg3")

	// A coordinator on a harness with neither goal mechanism is told the truth.
	plain := store.Session{ID: "wg2", Name: "payments", Agent: "hermes", Dir: "/sandbox/wg2"}
	if agent.GoalMode(plain) {
		t.Fatal("fixture unexpectedly has goal mode")
	}
	other := coordinatorGuide(plain)
	if !strings.Contains(other, "no native goal engine") {
		t.Error("non-goal coordinator guide claims or omits its lifecycle")
	}
	if strings.Contains(other, "update_goal") || strings.Contains(other, "amux agent goal") {
		t.Error("non-goal coordinator guide teaches a tool it does not have")
	}
	if !strings.Contains(other, agent.GoalRuntime) {
		t.Error("non-goal coordinator guide does not say which runtime does run goals")
	}
	assertCoordinatorWrapsUp(t, "non-goal coordinator", other, "wg2")
}

// Users ended up with dozens of finished workgroups idling on the dashboard. On
// every harness a finished coordinator wraps up its own workgroup: it archives
// itself only when "done" is proven (every PR verified MERGED on GitHub, nothing
// pending), waits for the user's merge without asking while that is all that
// remains, asks only when unsure the work is done — closed-unmerged PRs and any
// doubt included — never forces, and archives as the last step after its final
// summary.
func assertCoordinatorWrapsUp(t *testing.T, kind, guide, id string) {
	t.Helper()
	// The guide is hard-wrapped prose; match phrases against its unwrapped form.
	flat := strings.Join(strings.Fields(guide), " ")
	for _, want := range []string{
		"does not archive the workgroup by itself",
		"archive every finished member",
		"## Wrapping up this workgroup",
		"wait for the user's merge, archive yourself, or — only when you are not sure the work is done — ask the user; never just stop",
		// Waiting on the user's merge is the default, never a question.
		"**Waiting on the user's merge is the default, not a question.**",
		"the only remaining step is the user's merge, do not ask whether to archive",
		"once every PR is MERGED and nothing else is pending, archive (below) without asking",
		"PRs that are open only because the user has not merged them yet are not doubt",
		// The root id and where it comes from.
		"Your workgroup's root id is `" + id + "` (`$AMUX_WORKGROUP` in your environment; `$AMUX_ROOT` is set only for members and is empty for you)",
		// Auto-archive: every PR verified MERGED from GitHub itself.
		"**Auto-archive only when ALL of these hold**",
		"Every PR this workgroup produced is **MERGED**",
		"`gh pr view <n> --repo <owner/repo> --json state,mergedAt`",
		"never trust \"opened\", \"ready\", or a member's word",
		"A PR **closed without merging is not done**: ask.",
		"no follow-up pending or offered, no open decision, no member running (`amux ls --json`), no unanswered user message, and nothing left open in the PR ledger",
		// Archive-once-merged is the default; a user instruction is standing permission.
		"**\"Archive once every PR is merged\" is the default.**",
		"archive without asking for confirmation",
		"is **standing permission** for exactly that",
		"Standing permission never overrides the rules below",
		// The command must match the user's allow rule.
		"Run it as a **standalone** command: exactly `amux agent done`, never chained with `&&`, `;`, `|` or anything else",
		"`Bash(amux agent done)`",
		"do not retry, rephrase or work around it",
		// Ask on any doubt.
		"**Ask only when you are genuinely unsure the work is done**",
		"trailing questions, offered follow-ups, or open decisions",
		"a PR closed without merging, or the set of PRs is unclear",
		"any uncertainty at all about being done",
		"failing CI that was merged anyway, work handed to another workgroup",
		"When in doubt, ask; never guess.",
		"**Never archive** while a member is still running, or while the user's last message is unanswered",
		"Never pass `--force`",
		// The command and the safe order.
		"`amux agent done` is the command: it archives this workgroup (" + id + ") and its direct members",
		"the host's `amux archive " + id + "`",
		"Write the final summary to `COORDINATION.md`",
		"run `amux agent done` as the **last** tool call of the turn",
		"stops your runtime only once the turn has ended",
		"If it fails (for example an open PR), the workgroup stays open: report the error and ask the user instead.",
		"Restoration is host-only",
		"Previously completed members stay archived",
		// The question, asked only when unsure, and its two answers.
		"### Asking whether to archive",
		"Ask only in the \"unsure\" cases above, never merely because a PR awaits the user's merge.",
		"**Archive now**",
		"**Keep open: I have more work**",
		"If the answer leaves member PRs open only for the user's merge, wait for it as above, without asking again.",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("%s guide missing %q", kind, want)
		}
	}
	for _, forbidden := range []string{
		"Never archive this workgroup yourself",
		"Wait for explicit user confirmation before archiving",
		"goal completion is not confirmation",
		"automatically marks the workgroup done",
		"stops its coordinator and members",
		"archive yourself with --force",
		// Waiting on a merge is the default, not an option the user must pick.
		"Keep open: waiting on my PR merge",
		"several PRs and any one is not merged",
		"otherwise ask)",
	} {
		if strings.Contains(flat, forbidden) {
			t.Errorf("%s guide still carries stale archive wording: %q", kind, forbidden)
		}
	}
}

// The wrap-up question uses each harness's mechanism that actually reaches the
// user on every client, and waiting on the user's merge keeps a wake-up path: a
// completed Claude goal takes no PR wake-ups, so it must wait instead.
func TestCoordinatorWrapUpAskAndWaitPerHarness(t *testing.T) {
	flat := func(s store.Session) string {
		return strings.Join(strings.Fields(coordinatorGuide(s)), " ")
	}
	claude := flat(store.Session{ID: "wg3", Agent: agent.HookGoalRuntime, Dir: "/s/wg3"})
	for _, want := range []string{
		"Use the native question UI, the `AskUserQuestion` tool",
		"web dashboard and the Android app",
		"First run `amux agent goal ask \"Archive this workgroup?\"`",
		"do not complete the goal yet (a completed goal gets no PR wake-ups)",
		"`amux agent goal wait \"awaiting merge of <repo#PR, …>\"`",
		"While the only remaining step is the user's merge, do not ask and do not complete the goal yet",
	} {
		if !strings.Contains(claude, want) {
			t.Errorf("claude wrap-up missing %q", want)
		}
	}
	codex := flat(store.Session{ID: "wg1", Agent: agent.GoalRuntime, Dir: "/s/wg1"})
	plain := flat(store.Session{ID: "wg2", Agent: "hermes", Dir: "/s/wg2"})
	for name, guide := range map[string]string{"codex": codex, "plain": plain} {
		for _, want := range []string{
			"Ask in plain text in your final message",
			"Do not use `request_user_input` for this question",
			"`amux agent wait <member>... --timeout 30m`",
		} {
			if !strings.Contains(guide, want) {
				t.Errorf("%s wrap-up missing %q", name, want)
			}
		}
		if strings.Contains(guide, "AskUserQuestion") && name == "codex" {
			t.Errorf("%s wrap-up teaches Claude's AskUserQuestion", name)
		}
	}
	if !strings.Contains(codex, "Mark the goal complete with `update_goal` first") {
		t.Error("codex wrap-up lets the goal continue while it waits for the answer")
	}
	if strings.Contains(plain, "update_goal") {
		t.Error("plain wrap-up teaches update_goal")
	}
}

// The member guide is unchanged by goal mode: an ordinary agent is dispatched by
// its coordinator and has no goal of its own to reason about.
func TestMemberGuideMentionsNoGoals(t *testing.T) {
	guide := memberGuide(store.Session{ID: "m1", RootID: "wg1", Agent: "claude", Branch: "amux/wg1-m1"})
	for _, forbidden := range []string{"update_goal", "goal mode", "thread goal"} {
		if strings.Contains(guide, forbidden) {
			t.Errorf("member guide mentions %q", forbidden)
		}
	}
}

// Agents read "the user merges" as "keep the PR draft" and left finished PRs
// in draft; the member guide must say to mark the PR ready once green, keep
// babysitting, leave the final merge to the user, and that not merging never
// implies draft.
func TestMemberGuideEndsPRLifecycleReadyForReview(t *testing.T) {
	guide := strings.Join(strings.Fields(memberGuide(store.Session{ID: "m1", RootID: "wg1", Agent: "claude", Branch: "amux/wg1-m1"})), " ")
	for _, want := range []string{
		"mark it ready with `gh pr ready <n>` and report it ready for review",
		"keep babysitting (CI, review feedback, conflicts)",
		"merge, the final step, which the user does",
		"Marking ready is the hand-off to review, not the end.",
		"do not enable auto-merge",
		"it never means \"keep the PR draft\"",
		"A finished PR left in draft is an incomplete task.",
	} {
		if !strings.Contains(guide, want) {
			t.Errorf("member guide missing %q", want)
		}
	}
}

// A coordinator verifies member PRs; it must treat a finished PR left in draft
// as incomplete and never equate "do not merge" with draft, in every runtime.
func TestCoordinatorGuideRequiresReadyMemberPRs(t *testing.T) {
	for _, kind := range []string{agent.GoalRuntime, agent.HookGoalRuntime, "claude"} {
		root := store.Session{ID: "wg1", Name: "payments", Agent: kind, Dir: "/sandbox/wg1"}
		guide := strings.Join(strings.Fields(coordinatorGuide(root)), " ")
		for _, want := range []string{
			"marks its PR ready for review (`gh pr ready <n>`) once the code is complete and CI is green",
			"a finished PR left in draft is incomplete work",
			"it never means \"keep the PR draft\"",
			"Before you hand a PR off, check that it is green and marked ready for review",
		} {
			if !strings.Contains(guide, want) {
				t.Errorf("%s coordinator guide missing %q", kind, want)
			}
		}
		if agent.GoalMode(root) && !strings.Contains(guide, "\"do not merge\" never means draft") {
			t.Errorf("%s goal section does not say do-not-merge never means draft", kind)
		}
	}
}

// The console dispatches creations for the user, so its guide must name the
// creation fields the daemon actually accepts — a guide that teaches a field
// the allowlist refuses sends the model into an error it cannot diagnose.
func TestConsoleGuideTeachesCoordinatorFields(t *testing.T) {
	guide := consoleGuide(store.Session{ID: "console", Agent: "claude", Dir: "/sandbox/console", Mode: store.ModeConsole})
	for _, want := range []string{"--coordinator ", "--coordinator-model", agent.GoalRuntime} {
		if !strings.Contains(guide, want) {
			t.Errorf("console guide does not teach %q", want)
		}
	}
}

// Only a Claude coordinator's environment names its goal file: the hooks and
// `amux agent goal` find their state through it, and its absence is how they
// know a session is not in Claude goal mode.
func TestGoalFileEnvOnlyForClaudeCoordinator(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    store.Session
		want bool
	}{
		{"claude coordinator", store.Session{ID: "wg1", Agent: "claude", Dir: "/s/wg1"}, true},
		{"codex coordinator", store.Session{ID: "wg2", Agent: agent.GoalRuntime, Dir: "/s/wg2"}, false},
		{"claude member", store.Session{ID: "m1", RootID: "wg1", Agent: "claude", Dir: "/s/m1"}, false},
	} {
		got := envOf(AgentEnv(tc.s), claudegoal.EnvFile)
		if (got != "") != tc.want {
			t.Errorf("%s: %s=%q, want set=%v", tc.name, claudegoal.EnvFile, got, tc.want)
		}
		if tc.want && got != claudegoal.StatePath(tc.s.Dir) {
			t.Errorf("%s: goal file %q, want %q", tc.name, got, claudegoal.StatePath(tc.s.Dir))
		}
	}
}

// Coordinators sat "waiting" with ready member PRs nobody babysat, and archived
// members whose PRs were still open. Every coordinator guide must make merged or
// closed the finish line, keep a PR ledger, react by re-prompting members, and
// refuse to archive a member that still owns an open PR.
func TestCoordinatorGuideTracksMemberPRsToMergedOrClosed(t *testing.T) {
	for _, kind := range []string{agent.GoalRuntime, agent.HookGoalRuntime, "hermes"} {
		root := store.Session{ID: "wg1", Name: "payments", Agent: kind, Dir: "/sandbox/wg1"}
		guide := strings.Join(strings.Fields(coordinatorGuide(root)), " ")
		for _, want := range []string{
			"## Track every member PR to merged or closed",
			"A member PR marked ready is handed to review, not finished.",
			"Keep a **PR ledger** in `COORDINATION.md`: one row per member PR — member, repo#PR, base branch (and the PR it is stacked on, if any), state",
			"React to what you see by re-prompting the member",
			"red CI, a merge conflict, new review comments",
			"Never archive a member that still owns an open PR",
			"A member is finished when its PR is merged or closed",
			"Never end a turn with open member PRs and nothing that will bring you back",
			"A task is finished only when every member PR it produced is **merged or closed**, or explicitly handed to the user.",
			"exactly what needs a human (e.g. merge approval",
			"Archiving finished members (PR merged or closed) with `amux archive <member>` needs no confirmation; a member whose PR is still open keeps babysitting it.",
		} {
			if !strings.Contains(guide, want) {
				t.Errorf("%s coordinator guide missing %q", kind, want)
			}
		}
		if agent.GoalMode(root) && !strings.Contains(guide, "The goal is complete only when every member PR is merged or closed, or explicitly handed to the user with what needs a human.") {
			t.Errorf("%s goal section does not make merged/closed the goal's finish line", kind)
		}
		if strings.Contains(guide, "A member PR is done only when it is green and marked ready") {
			t.Errorf("%s coordinator guide still calls a ready PR done", kind)
		}
	}
	claude := store.Session{ID: "wg3", Name: "payments", Agent: agent.HookGoalRuntime, Dir: "/sandbox/wg3"}
	hooked := strings.Join(strings.Fields(coordinatorGuide(claude)), " ")
	for _, want := range []string{
		"Do not arm a `Monitor` or `run_in_background` loop that polls `amux agent events` or `gh pr view`.",
		"The note names what is awaited (member, repo#PR)",
		"the moment a member finishes its turn, waits on a permission prompt or question, or stops",
		"Only for something amux does not watch",
		"The fallback, when one step inside a turn genuinely has to block on a specific member",
		"`amux agent wait <member> --until ready,idle`",
		"Never write a sleep or until-loop for members or PRs.",
		"amux's wake-ups count",
	} {
		if !strings.Contains(hooked, want) {
			t.Errorf("claude goal coordinator guide missing %q", want)
		}
	}
}

// A member must own its PR to merged or closed: never archive itself while the
// PR is open, arm a follow-up before ending a turn, and report the PR's state
// every turn so its coordinator can track it.
func TestMemberGuideOwnsPRUntilMergedOrClosed(t *testing.T) {
	guide := strings.Join(strings.Fields(memberGuide(store.Session{ID: "m1", RootID: "wg1", Agent: "claude", Branch: "amux/wg1-m1"})), " ")
	for _, want := range []string{
		"You own your PR until it is **merged or closed**.",
		"Do not run `amux agent done` while it is open",
		"unless your task explicitly says someone else owns the merge; then name who",
		"arm a follow-up that re-checks it",
		"say your coordinator will re-prompt you",
		"End every turn's final message with your PR's URL and state: draft or ready, CI",
		"mergeable or conflicting, and review status",
		"(only once your PR is merged or closed",
	} {
		if !strings.Contains(guide, want) {
			t.Errorf("member guide missing %q", want)
		}
	}
}
