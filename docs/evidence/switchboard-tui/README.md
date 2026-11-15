# Switchboard identity in the TUI

Captures of the native TUI's chrome after adapting the Harness "Switchboard"
identity to the terminal: one shape and word per state, yellow only for
attention, blue for running and focus, and the same reading at every color
depth. They are `model.view()` renders of a deterministic fixture (every state,
a folded workgroup with a waiting member, a wide CJK title, a repo and an
external session) with each color profile pinned explicitly. They are not PTY
captures and no daemon is involved, so they show layout, glyphs and SGR output,
not how a particular terminal's palette draws them.

Regenerate from the repository root:

```sh
AMUX_SWITCHBOARD_RENDER_DIR="$PWD/docs/evidence/switchboard-tui" \
  go test ./internal/nativetui -run '^TestSwitchboardEvidence$' -count=1
```

| File | What it shows |
|---|---|
| `rail-80x24-*.ansi`, `rail-120x32-*.ansi` | The same frame as `truecolor`, `256`, `16` and `none` (`NO_COLOR`) escape output. `cat` one in a terminal to see it. |
| `rail-80x24.txt`, `rail-120x32.txt` | That frame with escapes stripped: what remains with no styling at all. |
| `rail-*-ascii.txt` | The ASCII fallback (`AMUX_ASCII=1`). |
| `compact-60x24.*` | The 18-column rail used below 72 columns. |
| `form-100x28.*` | A modal form: square border, same tokens. |

What to check:

- `waiting` is the only yellow: a dark-on-yellow chip (`48;2;255;195;0`,
  `48;5;220`, or `30;103`), and reverse video under `none`.
- `running` and the focused border are blue; `ready` is green; `unknown` reads
  "unknown", not "running", with its own `◌`.
- The selected row (`▎`) is bold on a fill in 24-bit/256 color and bold reverse
  video in `16` and `none`; the open session keeps its `▌`.
- The `none` captures contain no color parameters at all, only `1` (bold) and
  `7` (reverse).
- Every line is exactly as wide as the terminal, including the row with a
  double-width title.

`go test ./internal/nativetui -run 'TestTheme|TestASCII|TestCompact|TestAttention|TestTruncate|TestDetect|TestState|TestStatusWord|TestFolded'`
asserts each of these.
