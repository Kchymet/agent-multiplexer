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
   text in `TypeChunk` = 256-byte pieces, `TypeDelay` = 5ms apart, and sends
   each newline as **backslash + Enter** (`TypedNewline = "\\\r"`), Claude's
   newline escape. Proposals that "use bracketed paste for atomicity" are wrong
   for Claude Code.
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
   last typed, and submits only once the composer shows exactly amux's text.
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

## When you change steering

- Add or extend a PTY-verified note in `internal/agent/keys.go`, with the CLI
  version you verified against.
- Add pane fixtures for the cases in rule 9, and a regression test for the bug
  you fixed.
- Update this file.
