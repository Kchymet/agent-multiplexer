---
name: create-pr
description: End-to-end pull-request flow for an amux agent — commit the work, push the branch, open the PR, then babysit it to merge. Use when the user asks to "open a PR", "create a pull request", "put this up for review", "raise a PR", "ship this", or when a task is code-complete and the next step is getting it reviewed and merged. Encodes this project's commit/PR conventions and the sandbox rules (stay on the amux/… branch, never push to master).
---

# create-pr — take a change from working tree to merged PR

This is the standard way an amux agent ships a change. It is **end-to-end**:
preflight → commit → push → open the PR → see it through → (for a task session)
mark the session done. Opening the PR is the middle of the job, not the end.

Keep two lifecycles distinct:

- **The PR** is done when it's merged (or you've hit something only a human can
  decide, and said so). The **babysit-pr** skill drives that.
- **The amux session** — if it's a single-task session — is done once its PR is
  open and green, because producing that PR was the whole point. Phase 5 marks it
  done; a separate PR-review loop can carry the PR to merge.

Run the phases in order. Don't skip preflight because the change "looks small."

## Phase 0 — Sandbox & preflight (do not skip)

You are in an amux worktree. The rules from your workspace guide (`CLAUDE.md`, or
`AGENTS.md` for non-Claude agents) are hard constraints:

- **Stay on your branch.** Run `git branch --show-current` — it must be your
  `amux/…` branch. Never commit to, or push to, `master`/`main`.
- **Rebase on the remote first**, inside the repo worktree (not the workspace
  root):

  ```sh
  git fetch origin && git rebase origin/HEAD
  ```

  Resolve any conflicts now, on your branch, before you build a PR on a stale
  base.
- **Green before you open.** Build and run the project's checks. Read the repo's
  `Makefile`/CI config and run what it runs (e.g. `make check` / `go build ./...`
  / `go test ./...`). Do not open a PR on a red tree — a reviewer's first signal
  should be green, not your CI failure.
- **Read your own diff** before committing: `git status` then
  `git diff origin/HEAD...HEAD` (already-committed work) and `git diff` (unstaged).
  Confirm every change is intentional and in scope. If you spot debug prints,
  stray files, or unrelated edits, clean them up.

## Phase 1 — Commit

- Stage only intentional changes. Never stage `.claude/` — amux git-excludes its
  own files (`settings.local.json`, `skills/`), so they should not appear; if they
  do, do not add them.
- Match the repo's commit style. In this project subjects are a **lowercase area
  prefix + imperative summary**, e.g.:

  ```
  wsops: launch Claude in the workspace root so .claude config is consistent
  tui: scroll the rail to follow the selection when it overflows
  agent: let agents read every Claude session, and tell them they can
  ```

- Write a body that explains **why**, not just what, when the change isn't
  self-evident. Wrap ~72 cols.
- End every commit message with a Claude co-author trailer. **Match the trailer
  this repo already uses** — check with `git log -8 --format='%b'` and copy the
  form you see (in this project it names the running model, e.g.):

  ```
  Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
  ```

  Use the model **you** are running as, not a hardcoded name.

Use a HEREDOC so the body and trailer are preserved:

```sh
git commit -m "$(cat <<'EOF'
area: imperative summary of the change

Why this change is needed and any context a reviewer needs. Wrap at ~72
columns.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
)"
```

## Phase 2 — Push

```sh
git push -u origin HEAD
```

Pushes your current `amux/…` branch and sets upstream. Never push to the default
branch.

## Phase 3 — Open the PR

First check whether one already exists for this branch — if so, skip to Phase 4:

```sh
gh pr view --json number,url,state 2>/dev/null
```

Determine the base branch from the repo rather than assuming (this repo's default
is `master`, but don't hardcode it):

```sh
gh repo view --json defaultBranchRef -q .defaultBranchRef.name
```

Create the PR against that base, head = your branch, with a HEREDOC body. This
project's PRs use **What / Why / Change / Test** sections and a Claude Code
footer:

```sh
gh pr create --base "$BASE" --head "$(git branch --show-current)" \
  --title "area: imperative summary" --body "$(cat <<'EOF'
## What
1–3 sentences on what this PR does.

## Why
The motivation / problem it solves. A short code snippet is fine if it clarifies.

## Change
- key change one
- key change two

## Test
- exact commands you ran to verify, with results (e.g. `go test ./internal/foo/`),
  or why the change can't be exercised

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

Keep the title in the same lowercase-area style as the commit subject. Print the
PR URL for the user.

## Phase 4 — See the PR through (babysit or hand off)

The PR is open, but the *change* isn't landed. Two paths, depending on who owns
merging:

- **If a separate PR-review loop owns merging** (this project runs one — it
  reviews open PRs and merges the clean ones), your active work is done: the
  deliverable of a task session is an open, green PR. Get CI green first (see
  babysit-pr step 1), then go to Phase 5.
- **If this session is responsible for landing the PR** (you were told to see it
  through, there's no review loop, or it's a long-lived loop session), follow the
  **babysit-pr** skill fully: watch CI, critically evaluate review feedback,
  address what's worth addressing, resolve conflicts, and drive it to merge.

Either way, don't walk away from a red PR — at minimum leave CI green.

## Phase 5 — Mark the amux session done (task sessions only)

A **task-driven** amux session exists to produce one change. Once its PR is open
(and green), the session's job is finished, so mark it done to clear it from the
active rail. This is reversible — it archives, it does not delete.

**Only do this when both hold:**

- `$AMUX_MODE` is `task` — **never** archive a `loop` session (a loop is meant to
  keep running; archiving it defeats its purpose).
- The session is genuinely task-driven — it was spun up to deliver this PR. If the
  session has more work queued, or the PR is one of several deliverables, don't
  archive; just report the PR.

`amux` is on `PATH`, and your own session id is in the environment (`$AMUX_WORKSPACE`,
alias `$AMUX_WORKGROUP`). Mark done with:

```sh
if [ "$AMUX_MODE" = "task" ]; then
  amux ws archive "$AMUX_WORKSPACE"   # reversible; alias: `amux ws done`
fi
```

(`ws` also accepts `workgroup`/`session`. To undo: `amux ws unarchive <id>`.)

Then report the outcome to the user: the PR URL and that the session was marked
done. If you did **not** archive (loop session, or more work remains), say so and
why.

## Guardrails

- One logical change per PR. If mid-flow you discover unrelated work, note it for
  a follow-up rather than piling it into this branch.
- If preflight can't go green and you can't fix it, stop and report the failure
  with the output — don't open a knowingly-broken PR.
- Everything stays inside your worktree. Don't touch other agents' worktrees, the
  amux data dir, or any parent clone.
