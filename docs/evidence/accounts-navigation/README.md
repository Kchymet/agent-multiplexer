# Accounts provider navigation and rail indentation

These are captures of the real Bubble Tea renderer's output through a PTY,
replayed into a terminal emulator and compared with the complete model frame.
Input is raw terminal bytes, not manually constructed `KeyMsg` or `MouseMsg` calls.
Only initialization/service calls use synthetic fixtures; the production program
constructor, input decoder, Update, commands, View, and renderer remain active.
No live account, login, or external credential was used.

The corrected requirement is provider selection: Up/k selects the previous
provider and Down/j the next, clamping at Claude/Codex. A provider change resets
the details viewport. Tab/Shift+Tab retain their existing cycling behavior.
Wheel and Page keys continue to scroll the viewport independently; no wheel bug
or terminal-mode fix is claimed. Rail budget rows gain two spaces of indentation.

| Size | Rail | Codex selected | Selected identity | Wheel viewport | Page viewport |
| --- | --- | --- | --- | --- | --- |
| 80×24 | [rail](accounts-80x24-rail.txt) | [Codex](accounts-80x24-selected-codex.txt) | [identity](accounts-80x24-codex-identity.txt) | [wheel](accounts-80x24-sgr-wheel.txt) | [page](accounts-80x24-page-down.txt) |
| 120×32 | [rail](accounts-120x32-rail.txt) | [Codex](accounts-120x32-selected-codex.txt) | [identity](accounts-120x32-codex-identity.txt) | [wheel](accounts-120x32-sgr-wheel.txt) | [page](accounts-120x32-page-down.txt) |
| 52×12 | [rail](accounts-52x12-rail.txt) | [Codex](accounts-52x12-selected-codex.txt) | [identity](accounts-52x12-codex-identity.txt) | [wheel](accounts-52x12-sgr-wheel.txt) | [page](accounts-52x12-page-down.txt) |

The adjacent captures also include Claude selection, details, closure, X10 wheel
reports, trackpad-style SGR bursts, and wheel scrolling during a blocked refresh.
The tested terminal enables both cell-motion mouse reporting (1002) and SGR mouse
encoding (1006) through the same constructor used by the interactive TUI.

[Before-fix regression](regression-before.txt): with the original handler, raw
CSI Down was decoded but left `harness=claude`, `profile=claude:central`. The
corrected test verifies CSI/SS3 arrows, literal j/k, clamping, Tab/Shift+Tab,
highlight, profile identity, details, and viewport reset at each size. Navigating
sends no account mutation. Pending refresh, confirmation, and active browser
ceremony tests cannot retarget the account; the confirmed login retains its
reviewed profile. Protected token input keeps j/k literal, ignores arrows for
selection, accepts bracketed paste, clears via Ctrl+U, and cancels via Escape.
No protected text or modal input reaches the underlying terminal.

Reproduce from the repository root:

```sh
AMUX_ACCOUNTS_NAV_RENDER_DIR="$PWD/docs/evidence/accounts-navigation" \
  go test ./internal/nativetui -run '^TestAccountsPTY' -count=1
```

Fixtures identify the origin of every frame with `work NN` terminal content.
Observed timestamps are capture-time values; account identities and reset dates
are synthetic. The tests run on Linux and macOS. Current master’s 26-column rail
width is unchanged, as are auth/binding schemas and credential lifecycle logic.
