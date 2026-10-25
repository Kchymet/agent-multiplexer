# Pane-local scrollback

amux mirrors each agent pane through a VT emulator (`internal/vterm`), so the
outer terminal's scrollback never sees the agent's output. The pane keeps its
own bounded history and a viewport over it. This page is the policy; the
package documentation in `internal/vterm/history.go` is the contract.

## What becomes history

| Screen | History | Who scrolls |
| --- | --- | --- |
| Primary | Rows that physically scroll off the top: LF/IND/NEL at the bottom margin, SU, DL, inside a full-width scrolling region. Bounded per pane (`DefaultHistoryLines`, `SetHistoryLimit`). | amux: `ScrollLines`, `ScrollPages`, `ScrollToTop`, `ReturnLive`. |
| Alternate | None. A real terminal keeps none either, and the program draws its own. | The program: raw mouse events when it tracks the mouse (`MouseTracked`), alternate-scroll cursor keys otherwise (`MouseEvent`). `ScrollLines` reports no movement. |

Repaints never enter history. Cursor-positioned redraws (every ratatui or Ink
frame) touch cells, not the scroll position. Erase-display 2 (`ESC[2J`) is
overridden to erase without saving the screen, as xterm does, so a
clear-and-repaint cycle cannot duplicate rows. `ESC[3J` still clears history.

History rows are physical rows captured at the width they had. They carry no
soft-wrap metadata and are not reflowed on resize: a narrower pane truncates
them, a wider one pads.

## Codex

Codex 0.157.1 has two transcript modes (`codex-rs/tui/src/transcript_mode.rs`):

- **Owned** (`tui.alternate_screen = "always"`, which PR #132 injects): Codex
  enters the alternate screen, enables mouse modes 1000/1002/1003/1006 and
  redraws an in-app transcript inside synchronized-output frames. A probe of
  a 100-line response shows zero LF/IND/RI/NEL/SU/SD/DL operations, so there
  are no scrolled-off rows to capture. amux forwards the wheel to Codex, which
  scrolls itself. amux does not reconstruct history from frames: frames skip
  rows when streaming outpaces the frame rate (the probe showed rows 26-41 and
  63-79 never drawn), and identical rows (blank lines, separators, repeated
  tool output) make duplicate suppression unsound.
- **Terminal** (inline): Codex commits each transcript line above its viewport
  with `DECSTBM 1..top` plus `\r\n` (`insert_history.rs`, Standard strategy on
  Linux and macOS), does not capture the mouse, and clears with `ESC[2J` when
  its viewport moves. Every committed row scrolls off row 0 and lands in pane
  history exactly once. This is the mode that gives amux-owned scrolling of
  Codex output.

Switching Codex to inline mode is a launch-configuration change (the override
PR #132 added), not a vterm change.

## Viewport lifecycle

The viewport returns to live when the screen it was showing stops being the
right one:

- new output scrolls rows into history (a cursor-only repaint keeps the offset);
- input from the user (`Write`; embedders call `ReturnLive` for anything else);
- a resize;
- the program enters the alternate screen;
- RIS (`Reset`), which also clears history: the daemon sends it before replaying
  a tail of output the history already absorbed.

Leaving the alternate screen keeps primary history intact.

## Rendering and snapshots

`Render` is unchanged while live. Scrolled back, it composites the newest
`Offset` history rows above the live screen's top rows into the same exact
cols×rows block and overlays ` ↑ offset/history ` in reverse video on the
top-right of row 0 (omitted when the pane is too narrow; `Indicator` returns
the text either way).

`Snapshot` returns a detached copy of the same cells without the indicator:
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
