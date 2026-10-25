# Embedded Codex wheel and key routing

User merges this work. Do not auto-merge the routing PR.

## Reproduce

The CI `namespace-runtime` job downloads and verifies the existing Codex 0.157.1
fixture, requires the native test to execute, and uploads its screen/PTY evidence.
Run the same test locally with that executable:

```sh
AMUX_TEST_CODEX_BIN=/path/to/codex-x86_64-unknown-linux-musl \
AMUX_REQUIRE_CODEX_SCROLL=1 \
AMUX_CODEX_SCROLL_EVIDENCE=/tmp/codex-scroll-evidence \
go test ./internal/nativetui -run '^TestCodexEmbeddedScroll$' -count=1 -timeout=180s -v
```

Archive: <https://github.com/openai/codex/releases/download/rust-v0.157.1/codex-x86_64-unknown-linux-musl.tar.gz>

Archive SHA-256: `e98c1e8e028e8137fa2d2415c82ec58e7b3701a627e3554aace5b3ca31454af2`.
The test also requires the executable to report `codex-cli 0.157.1`.

The capture here was produced on 2026-10-25 UTC after merging history correction
`b8a1081d` and master `8d8ea5b2`. The native test passed both cases in 22.18 seconds.
`SHA256SUMS` identifies the exact captured files; absolute temporary launch paths
are preserved in each `argv.json` for provenance and need not be reused.

## What actually runs

The genuine Codex interactive TUI runs inside a 100×28 PTY with a fresh private
CODEX_HOME, `--no-daemon`, a read-only child sandbox, and on-request approvals.
A local Responses server supplies a deterministic 100-line assistant response,
streaming one numbered line every 80 ms. The model name is `gpt-5.6` in fixture
configuration; no authenticated model call is made. Neither credentials nor host
conversations are borrowed. This is real Codex rendering synthetic response data.

The PTY stream feeds production `vterm.NewRemote`; emulator replies go back to
that child. The native TUI's production mouse/key handlers receive Bubble Tea
events at rail/pane screen coordinates and render the resulting model view at
127×31. This exercises actual child rendering, composer behavior, mode tracking,
viewport storage, TUI routing, and header rendering together.

The `always` case calls production `codexcfg.FullscreenTUI`, forcing
`tui.alternate_screen="always"` exactly as amux does. It verifies both alternate
screen and mouse reporting are active. The comparison case uses `--no-alt-screen`
and verifies both are inactive. The test uses default history bounds.

## Observations and assertions

Both native cases passed:

1. Marker `CODEX-HISTORY-070` was visibly rendered while the response streamed
   (`older-visible.txt`), then disappeared from the live viewport.
2. Typing `DRAFT-WHEEL-MUST-STAY` left that exact draft in Codex's composer
   (`live-draft.txt`).
3. Local wheel routing recovered marker 070 (`local-history.txt`). Forced-always
   uses **Shift-wheel** despite child mouse reporting; no-alt uses ordinary
   wheel. Neither sent any additional child input. The header showed the history
   position without covering pane cells.
4. Alt+End restored live output and the original draft, clearing the indicator.
   Alt+PageUp scrolled back again; typing `x` returned live and produced the exact
   draft `DRAFT-WHEEL-MUST-STAYx` (`input-live.txt`).
5. Ordinary wheel in reporting-enabled forced-always mode emitted the expected
   SGR report at pane-local coordinates (3,3), scrolled inside Codex, and preserved
   the draft (`always-app-wheel.txt`).
6. After submitting the draft to the synthetic server, Alt+PageUp scrolled during
   the second response. Further child output returned live without another input
   event (`output-live.txt`).

`pty.raw` is the genuine child terminal stream. `input.raw` contains terminal
query replies, explicit test typing, and (only in the reporting case) the SGR
wheel report. The first response, draft, and second response are synthetic and
contain no user conversation data. Captures vary slightly with render timing;
assertions depend on displayed content and state, not fixed timing snapshots.

The earlier baseline probe showed forced-always Codex using synchronized
cursor-addressed redraws, with no LF/IND/RI/SU/SD/scroll-region operations; no-alt
used LF and partial-height scrolling regions. This is why forced-always requires
PR190's completed-frame history rather than only primary scrollback.

## Independent history review and limits

Reviewed the separate bounded primary/alternate buffers, duplicate and blank
frame suppression, reset/screen-switch behavior, chunk-independent synchronized
frame boundaries, output-driven live return, and matching Render/Snapshot cells.
The native test failed against original history WIP `bed4e4a` with
`AltScreen=true, HistoryLen=0`, then passed with correction `b8a1081d`. No blocker
remains in the corrected history behavior exercised here. An empty `Write(nil)`
was found to return live without actual input; PR2 guards and tests that input path.

History is a bounded record of previously rendered content, not a reconstructed
full conversation. Spinner changes may consume alternate-frame capacity; apps
without synchronized output have no alternate-frame history. New output returns
live even while browsing. These are the agreed history policies.

Local routing/config tests also cover boundaries and empty history, rail/pane
coordinates, Shift over Alt, reporting clicks/wheels, rebinding, and one-column
header indicators. A race-detector probe found the existing emulator Close/Read
race; the same failure reproduces on unmodified baseline `90ac3c7` in
`TestFeedLossless`, and no teardown refactor is included here.
