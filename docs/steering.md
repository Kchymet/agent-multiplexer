# Steering: how amux types into an agent's composer

amux has exactly one way to put text into an interactive agent (Claude Code,
Codex): it types keys into the agent's terminal pane. Every sender goes through
that path: steering `prompt` and `interject` (console, CLI, direct and Android
clients), PR-watcher and member wake-ups, the Claude goal resume and restart
nudges, and question replies. The single implementation is
`internal/daemon/typed_delivery.go` (`deliverTyped`). The per-harness key maps
are in `internal/agent/keys.go`. **Read both before changing steering.** Most
of the rules below were learned by breaking something.

## Rules (each was verified against a real CLI; don't relitigate them without a new PTY test)

1. **Type, never paste (Claude Code).** A bracketed paste is collapsed into a
   `[Pasted text #1 +N lines]` placeholder and given to the model inside
   `<pasted_content>` tags, which the model is told to treat as material the
   *user did not write*. So an orchestrator's directive arrives as untrusted
   content. Verified against CLI 2.1.310 and fixed in #218. amux types steering
   text in `TypeChunk` = 64-byte pieces, `TypeDelay` = 100ms apart, and sends
   each newline as **backslash + Enter** (`TypedNewline = "\\\r"`), Claude's
   newline escape. Proposals that "use bracketed paste for atomicity" are wrong
   for Claude Code. **Typing is paced** because Claude's paste detection also
   fires on fast bursts of typed input: a long multi-line steer typed in
   256-byte pieces 5ms apart was seen collapsed into a `[Pasted text #N +M
   lines]` placeholder (session 923958). `TestNativeClaudeTypedDelivery` types
   a long multi-line steer into the pinned CLI in CI and requires it verbatim.
2. **Leading `/` or `!` changes mode.** Typed text reaches the composer's own key
   handling: a first character `/` runs a slash command, and `!` enters bash mode
   and runs the rest as a shell command. Text starting with one of these
   (`TypedTriggers`) is sent with a leading space. `/` or `!` on a later line,
   `#`, and `@` behave as plain text.
3. **Enter while busy is how interject works.** Claude Code queues a message
   submitted mid-turn and delivers it at the next boundary. Codex steers the
   running turn with it. So the composer can legitimately be used while the agent
   is busy, and a busy pane is the main interject case, not an error.
4. **Interrupt keys differ, and Claude's is dangerous when idle.** Claude:
   Ctrl+C, not Esc. With `"editorMode": "vim"` (the maintainer uses it) the
   composer swallows Esc to enter vim normal mode. Ctrl+C at an idle prompt
   clears the composer, and a second press exits the CLI, so it's sent only when
   the harness reports the agent mid-turn (`InterruptOnlyWhileBusy`). Codex: Esc
   (inert when idle).
5. **Permission prompts.** Claude: Enter accepts the focused first option, Esc
   declines. Codex: `y` / `n` (not Esc, which edits the previous message on an
   empty composer).
6. **An open question replaces the composer.** While Claude's AskUserQuestion
   selector is up, printable keys drive the selector and Enter picks the focused
   option. A typed reply would be misread. Send `DismissQuestion` (Esc), wait
   `DismissDelay` for the composer to come back, then type.
7. **The composer is shared with the human.** Since #334, `deliverTyped` types only
   into a composer it can see is empty, waits `humanQuietPeriod` after the human
   last typed (automatic text only), and submits only once the input region
   shows amux's text as typing renders it. See "How `deliverTyped` decides".
8. **Never leave amux's text behind.** If amux typed text and then can't confirm
   or submit it, it must take back exactly the characters it typed (Erase) or keep
   the message queued for automatic retry. It must never leave half-delivered text
   for a human to find. (#334 initially left text behind on busy panes:
   "composer busy … amux's text was typed but not submitted and is left in the
   input box".)
9. **Verifying "exactly amux's text" means comparing against what typing renders,
   in the input region only, not the whole pane.** Account for:
   - the backslash-newline escapes from rule 1, rendered at line ends, and soft
     wraps;
   - the leading space from rule 2;
   - vim mode (`-- INSERT --`) and the mode/status lines under the composer
     (`▸▸ auto mode on (shift+tab to cycle)`);
   - the busy-turn UI (spinner, queued-message display), which sits above or
     below the input box;
   - modal dialogs that replace the composer entirely, e.g. AskUserQuestion,
     permission prompts, and Claude's auto-mode onboarding ("Teach auto mode
     about your environment? 1. Yes / 2. Not now / 3. Don't show again"). A
     composer you can't recognize is Unknown: defer, don't type.
   Test against captured fixtures of real busy and idle Claude Code and Codex
   panes (with and without vim mode), not hand-written strings.
10. **Mark provenance.** amux-originated prompts start with a marker such as
    `[amux goal]` on its own line, never appended to other text.
11. **Prefer channels that don't type, where they exist.** The Codex app-server
    (`internal/codexapp`) and Claude hooks can carry some information without
    the composer. Typing is the fallback for interactive panes, and every new
    sender still goes through `deliverTyped`.

## How `deliverTyped` decides (since #344)

**1. Classify the input region** (`composer.Classify`). Only the composer block
is read: Claude's rule-framed `❯` block (the frame may carry a short label) or
Codex's `›` block, plus a numbered selector below it or in its place.
Transcript, tool output, spinner and status rows, queued-message previews, the
vim mode and auto-mode lines, and footers are never composer text.

| State | Meaning | Action |
|---|---|---|
| `free` | composer recognized and empty | type |
| `draft` | composer holds text | refuse, nothing typed (unless it is amux's remembered leftover; see 4) |
| `dialog` | a numbered selector owns the keyboard (permission prompt, AskUserQuestion, auto-mode onboarding, Codex approval) | refuse, nothing typed |
| `unknown` | no composer recognized (booting, repaint, unknown layout) | refuse, nothing typed |

`busy` ("esc to interrupt" on screen) is reported alongside the state and is
informational only. A free composer mid-turn takes the text (rule 3). An open
AskUserQuestion is still dismissed first for a reply (rule 6); if it is still
up, the pane classifies as `dialog`.

**2. Verify what typing renders** (`composer.MatchTyped`, `composer.Same`).
After typing, the region must show amux's text. The comparison ignores:
- whitespace (soft wraps, continuation indents, the leading space of rule 2);
- the backslash Claude leaves at the end of each line typed as backslash+Enter;
- spinner and ellipsis glyphs repainted into the region.

A collapsed-paste placeholder (`[Pasted text #N( +M lines)]`, Codex
`[Pasted Content N chars]`) counts as amux's text when every literal around it
is amux's, in order. Acting on that requires that amux saw the box empty and
nobody else typed since. Such a submit is journaled. The echo wait is 8 s plus
the paced typing time.

**3. Submit only on a match with no human input since amux looked.** Otherwise:

**4. Never leave amux's text behind (take-back).**
- **Nobody touched the pane** since amux saw it empty: the box holds only amux's
  text, so amux presses `Keys.Erase` (Backspace) once per character it typed (a
  typed newline counts as one) and checks that the box is free again.
- **Someone typed meanwhile:** amux erases only if the box still shows exactly
  amux's words. Otherwise it leaves the box alone and **remembers** the text per
  session and runtime instance (`typedLeftover`).
- **Leftover memory:** the next delivery recognizes a box holding exactly that
  remembered text as amux's, erases it, and delivers. It is never treated as a
  user draft. A placeholder stands for a leftover only if amux left it with
  nobody typing and nobody has typed since, because a placeholder could hide
  the user's own paste.
- **A dialog or unreadable screen after typing:** no keys are sent, since they
  would reach whatever holds focus. The text is remembered instead.

**5. Queue refused person steers** (`steer_queue.go`). A person's
`prompt`/`interject` refused as `composer_busy` is queued as a
`prwatch.KindSteer` wake on the session's own queue (`steer:<id>`), on the PR
watcher's deferral machinery (Notify / Flush every 20 s / Kick /
MaxPendingAge). The error says it is queued and must not be resent.
- Queued steers replay in order through the ordinary steer path. One refused
  again stays queued, along with everything after it. A stopped agent drops it,
  with a journal entry.
- An identical resend while queued collapses into the one queued copy.
- A resend delivered directly cancels the queued copy, so text is never
  delivered twice.
- Automatic text (wakes, nudges) keeps its own retry and is not queued here.

Fixtures for all of this live in `internal/composer/testdata/panes/`; their
README gives the format and marks each fixture REAL or RECONSTRUCTED. The
regression tests are `TestPaneFixtures`, `TestTypedDeliveryAgainstPaneFixtures`,
`TestBusySteerSequenceDeliversOnceWithNoResidue`,
`TestFailedLongSteerThenSecondSteerDeliveredOnce` and
`TestLeftoverTextNeverBlocksTheNextSteer`.

## When you change steering

- Add or extend a PTY-verified note in `internal/agent/keys.go`, with the CLI
  version you verified against.
- Add pane fixtures for the cases in rule 9, and a regression test for the bug
  you fixed.
- Update this file.
