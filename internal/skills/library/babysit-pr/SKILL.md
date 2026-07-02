---
name: babysit-pr
description: Watch an open pull request and drive it to a mergeable state — poll CI and fix failures, read review feedback and critically decide what to act on, address accepted feedback, and resolve merge conflicts as they arise. Use when the user asks to "babysit the PR", "watch the PR", "keep the PR green", "address review feedback", "resolve the merge conflicts", or after opening a PR (the create-pr flow ends here). Emphasizes judging reviewer feedback on its merits rather than blindly complying.
---

# babysit-pr — shepherd a PR to merge

A PR is not done when it's opened. Between "opened" and "merged" it needs
tending: CI has to go green, review comments arrive, and the base branch moves
underneath you. This skill is that tending loop. It runs after **create-pr**, but
also stands alone — invoke it any time you need to nurse an existing PR (yours or,
if asked, someone else's).

Sandbox rules still apply: stay on your `amux/…` branch, keep every edit inside
your worktree, and never push to the default branch.

## The loop

Identify the PR (`gh pr view --json number,url,headRefName,state`), then repeat
until the exit condition below:

1. **Refresh state** — checks + reviews + mergeability in one pass:

   ```sh
   gh pr view <n> --json state,mergeable,mergeStateStatus,statusCheckRollup,reviewDecision,reviews
   ```

2. **Get CI green** (step 1 below).
3. **Read and triage review feedback** (step 2 — the important one).
4. **Address what you accepted; reply to the rest** (step 3).
5. **Resolve conflicts if the base moved** (step 4).
6. Re-evaluate. Stop when the **exit condition** is met.

Between iterations you don't need to hammer the API. Watch actively while CI is
running; when you're waiting on a human reviewer, poll on a sane cadence (the
`/loop` skill can re-run this check every few minutes) rather than spinning.

### 1. Get CI green

```sh
gh pr checks <n> --watch        # block until checks settle
```

If a check fails, read the actual failure — don't guess:

```sh
gh run view <run-id> --log-failed
```

Fix the cause in your worktree, re-run the relevant checks locally, commit
(same conventions as create-pr: lowercase-area subject, `Co-Authored-By: Claude
Opus 4.8 <noreply@anthropic.com>` trailer), and push. Repeat until green. A
flaky/unrelated infra failure is worth one re-run before you spend time on it —
say so if you re-run.

### 2. Read and **critically** triage review feedback

Collect everything — the top-level review verdicts and the inline thread
comments:

```sh
gh pr view <n> --comments
gh api repos/{owner}/{repo}/pulls/<n>/comments        # inline review comments
```

This project's reviews may come from an automated reviewer (e.g. a code-review
bot / ultrareview) as well as humans. **Treat every comment as a claim to be
verified, not an order to be executed.** Automated and human reviewers are both
often right and sometimes wrong; your job is to tell the difference. For each
distinct piece of feedback, decide **implement / partially implement / decline**,
using this rubric:

- **Is it correct?** Go look at the code. Reproduce the concern. A reviewer can
  misread control flow, miss a guard that already exists, or flag a non-bug.
  Don't "fix" something that isn't broken — you'll add risk for nothing.
- **Is it in scope?** A suggestion that's real but belongs to other code, or
  expands this PR's purpose, should be acknowledged and deferred (note it / file
  a follow-up), not smuggled in. Scope creep is how a clean PR rots.
- **Does it fit the project?** Weigh it against existing conventions, the
  surrounding code, and the PR's intent. "Rewrite this the way I'd do it" is not
  automatically an improvement.
- **Blocker or nit?** Correctness, security, data-loss, and API-contract issues
  are blockers — address them. Style nits and preferences are optional; batch or
  skip them with a word of explanation.
- **Cost vs. benefit.** A large, risky refactor to satisfy a minor point may not
  be worth it. Say so.

Bias: **apply well-founded correctness and security feedback**; **push back, with
reasoning, on feedback that is wrong, out of scope, or not worth the cost.**
Silently ignoring a comment is not an option — either act on it or reply saying
why you won't.

### 3. Address accepted feedback; reply to the rest

- Make the accepted changes in your worktree, commit (conventions above), push.
- Reply on each thread so the reviewer sees the disposition:
  - Accepted: reply pointing at the commit that addresses it.
  - Declined/deferred: reply with a brief, respectful rationale (why it's not
    correct / out of scope / deferred to a follow-up), so it's a documented
    decision, not a dropped ball.

  ```sh
  gh pr comment <n> --body "Addressed in <sha>: <what changed>."
  # inline reply:
  gh api repos/{owner}/{repo}/pulls/<n>/comments -f body="…" -F in_reply_to=<comment-id>
  ```

### 4. Resolve merge conflicts / stale base

If `mergeable` is `CONFLICTING` or the base has advanced, rebase onto the latest
remote inside the repo worktree:

```sh
git fetch origin && git rebase origin/HEAD
# resolve conflicts, keeping BOTH the intent of your change and incoming changes
git rebase --continue
```

Re-run the project's checks after resolving (a clean merge can still break the
build). Then update the remote branch. Because it's your own amux branch, a
lease-guarded force-push is correct:

```sh
git push --force-with-lease
```

Never resolve a conflict by blindly discarding one side — understand both and
preserve the combined intent.

## Exit condition — when babysitting is done

Stop the loop when **all** hold:

- CI is green.
- Every review thread is resolved — accepted-and-addressed or declined-with-a-reason.
- The PR is mergeable (no conflicts) and, where the repo requires it, approved.

Then:

- **Merge only if you're authorized to.** Merging is an outward, hard-to-reverse
  action. Merge autonomously **only** when the task explicitly told you to (e.g.
  an autonomous PR loop whose policy is "merge approved PRs"). Otherwise, report
  that the PR is green and ready and let the human merge.
- When you do merge, use the repo's convention (this project squash-merges):

  ```sh
  gh pr merge <n> --squash --delete-branch
  ```

- If you're **blocked** — feedback you can't resolve alone, a failing check you
  can't fix, a design disagreement — stop and report it clearly with the specific
  blocker and what you tried. Don't churn.

Report the outcome plainly: merged (with URL), or ready-for-human (with what's
left), or blocked (with why). Don't claim done if it isn't.
