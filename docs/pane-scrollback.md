# Pane-local scrollback

amux mirrors each agent pane through a VT emulator (`internal/vterm`), so the
outer terminal's scrollback never sees the agent's output. The pane keeps its
own bounded history and a viewport over it. This page is the policy; the
package documentation in `internal/vterm/history.go` is the contract.

## What becomes history

The two screens have separate history buffers with separate rules. The
viewport scrolls whichever screen the program has active.

| Screen | History | Bound |
| --- | --- | --- |
| Primary | Rows that physically scroll off the top: LF/IND/NEL at the bottom margin, SU, DL, inside a full-width scrolling region. | `DefaultHistoryLines` rows (`SetHistoryLimit`), kept by the emulator's scrollback. |
| Alternate | Completed, distinct rendered frames: each time the program begins a synchronized-output frame (`DECSET 2026`), the frame currently on display is committed, unless it is identical to the last committed frame or entirely blank. | The same row limit, dropping whole frames oldest first; the newest frame always survives, so at most the limit plus one screen. |

amux scrolls both with `ScrollLines`, `ScrollPages`, `ScrollHalfPages`, `ScrollToTop` and
`ReturnLive`. Wheel events go to the program when it enables mouse reporting;
Shift forces local history instead. Without reporting, wheel stays local even
when history is empty or at its boundary: `ScrollLines` reports no movement,
and no cursor keys are synthesized. The configurable `keys.scroll-up`,
`keys.scroll-down`, and `keys.scroll-live` actions default to Alt+PageUp,
Alt+PageDown, and Alt+End. The history indicator lives in the TUI header and
never covers pane cells.

`keys.scroll-half-up` and `keys.scroll-half-down` default to Ctrl+U and Ctrl+D.
They move toward older/newer history by `max(1, rows/2)` rows (integer division),
clamping at the oldest row and live output. Full-page navigation remains
`max(1, rows-1)` rows. Half-page actions respect focus: pane focus targets pane
history, while rail focus preserves existing rail half-page navigation. Rebinding
these actions changes both uses. Full-page Alt chords remain global pane actions;
an open modal takes precedence. Pane focus without an attached pane does nothing.
The actions clear local selection,
keep local scrolling out of the child input stream, and leave mouse routing
unchanged. Bracketed pasted text never becomes a history command.

The defaults reserve the child's Ctrl+U line-kill and Ctrl+D EOF. To restore
those child keys, run the following and reopen the dashboard:

```sh
amux config set keys.scroll-half-up f5
amux config set keys.scroll-half-down f6
```

Upgrade compatibility: if an existing explicit setting already uses Ctrl+U or
Ctrl+D (for example `keys.scroll-up=ctrl+u`), it keeps the chord. The corresponding
new half-page default is suppressed and appears as `unbound` in `amux config`.
No config file is rewritten. Moving the older binding away restores the new
default, and two explicit bindings cannot be set to the same chord.

### Primary screen

Repaints never enter history. Cursor-positioned redraws (every ratatui or Ink
frame) touch cells, not the scroll position. Erase-display 2 (`ESC[2J`) is
overridden to erase without saving the screen, as xterm does, so a
clear-and-repaint cycle cannot duplicate rows. `ESC[3J` still clears history.

History rows are physical rows captured at the width they had. They carry no
soft-wrap metadata and are not reflowed on resize: a narrower pane truncates
them, a wider one pads.

### Alternate screen

A real terminal keeps no alternate-screen history, but the agents amux runs
keep their transcript there (see Codex below), so amux keeps the sequence of
frames the user saw:

- **Previously painted only.** `DECSET 2026` commits the cells on display
  before the new frame paints over them; `DECRST 2026` only closes the frame.
  The live frame is never in history, and a frame left half-painted by a
  nested `DECSET 2026` is not committed. The capture happens at the sequence,
  so chunking of the output cannot change what is captured.
- **Duplicate suppression.** A frame identical cell for cell (text, width,
  style, link) to the last committed frame is dropped, so a cursor-only or
  no-op repaint adds nothing. An all-blank frame (the cleared screen on entry)
  is dropped. Frames that differ only by a spinner glyph are distinct and are
  kept; the row bound is what limits them.
- **No synchronized output, no history.** A program that never uses
  `DECSET/DECRST 2026` has no frame boundary that is independent of chunking
  and timing, so no frame completes and `HistoryLen` stays 0. It keeps its own
  scrolling exactly as before.
- **One program session.** Entering or leaving the alternate screen
  (`DECSET/DECRST 1047/1049`) clears the frame history, as does RIS. Frames
  keep the size they were painted at and are shown cut or padded after a
  resize, like primary rows.

Frame rows are stored with trailing blanks trimmed, so the two buffers have
the same per-row cost.

## Codex

Codex 0.157.1 has two transcript modes (`codex-rs/tui/src/transcript_mode.rs`):

- **Owned** (`tui.alternate_screen = "always"`, which PR #132 injects): Codex
  enters the alternate screen, enables mouse modes 1000/1002/1003/1006 and
  redraws an in-app transcript inside synchronized-output frames. A probe of
  a 100-line response shows zero LF/IND/RI/NEL/SU/SD/DL operations, so there
  are no scrolled-off rows to capture. The alternate-screen frame history
  above is what amux keeps for it: each distinct completed frame, in order.
  Frames skip rows when streaming outpaces the frame rate (the probe showed
  rows 26-41 and 63-79 never drawn), so this history is the frames the user
  could have seen, not a lossless transcript; Codex's own transcript
  scrolling remains available through the forwarded mouse.
- **Terminal** (inline): Codex commits each transcript line above its viewport
  with `DECSTBM 1..top` plus `\r\n` (`insert_history.rs`, Standard strategy on
  Linux and macOS), does not capture the mouse, and clears with `ESC[2J` when
  its viewport moves. Every committed row scrolls off row 0 and lands in
  primary history exactly once, losslessly.

### Reopening a long-running pane

The engine replays only the last 256 KiB of a pane's output to a client that
opens it. Codex sets the alternate screen and mouse reporting exactly once at
startup, so once its repaints outgrow that tail a mirror rebuilt from the tail
alone sat on the primary screen without mouse reporting: the wheel stayed
local with no history to scroll, while Page Up/Down still reached Codex. The
engine therefore follows each pane's DEC private modes (`internal/termmodes`,
on the emulator's own parser) and, when the ring has been trimmed, restates
them ahead of the replay. Modes the program turned off or reset (RIS) stay off.

The same holds wherever a stream is cut down to a tail for a client that fell
more than 4 MiB behind: the daemon's and the mux server's per-connection resync
(`pane.reset`, then the last 256 KiB) and `vterm.Feed`'s own backlog trim (RIS,
then the last 256 KiB) each follow the modes of the bytes they queue and
restate them after the reset, so the cleared mirror keeps Codex's alternate
screen and mouse reporting. The remote provider's replay trim is not covered;
its consumer is the orchestrator, not an amux mirror.

The engine's tracker exists only in a daemon that has it: an upgrade does not
replace a running daemon, so panes keep the old behaviour until `amux daemon
restart`.

## Viewport lifecycle

The viewport returns to live whenever the program or the user moves on:

- any output at all, including a repaint that only changes text (a program
  that keeps repainting while idle keeps the pane live);
- input from the user (`Write`; embedders call `ReturnLive` for anything else);
- a resize;
- a switch between the primary and alternate screens;
- RIS (`Reset`), which also clears both histories: the daemon sends it before
  replaying a tail of output the history already absorbed.

Leaving the alternate screen keeps primary history intact and drops the frame
history.

## Rendering and snapshots

`Render` is unchanged while live. Scrolled back, it composites the newest
`Offset` history rows above the live screen's top rows into the same exact
cols×rows block. It never draws the scroll indicator into the pane: `Indicator`
returns ` ↑ offset/history ` (or `""` when live) for the embedder to show in
its border or status line, and `Viewport` carries the numbers for a custom
badge. This keeps every pane cell real text, so a selection copies what the
user sees, in narrow panes as well as wide ones.

`Snapshot` returns a detached copy of exactly the cells `Render` draws:
`Cells[y][x]`, physical rows, wide characters as a Width-2 cell followed by a
zero placeholder, plus the viewport state and a `Revision` that changes on any
output, resize, scroll or reset.

## Known limits

- The emulator pushes rows scrolled out of any full-width region, including one
  whose top margin is below row 0; xterm saves only from a region that starts
  at row 0. No agent amux runs scrolls such a region on the primary screen.
- The emulator gives the alternate screen its own hidden scrollback (10 000
  rows) that amux cannot reach or bound; only programs that scroll the
  alternate screen fill it, and owned-mode Codex never does.
- Rows lost when the emulator shrinks are not reflowed into history.
- Alternate-screen frames are compared only with the frame before them, so a
  screen shown, replaced and shown again is kept twice.
