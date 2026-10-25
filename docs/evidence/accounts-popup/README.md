# Accounts popup checkpoint

These are actual `model.View()` captures with a synthetic, colored terminal
mirror underneath the popup. They are TUI output, not web mockups. `.txt` files
strip ANSI styling for source review; matching `.ansi` files preserve the colors,
selection, and progress bars. No live accounts or credentials were used.

| Terminal | Rail before opening | Popup | Scrolled Codex details |
| --- | --- | --- | --- |
| 80×24 | [rail](popup-80x24-rail.txt) | [popup](popup-80x24-open.txt) · [ANSI](popup-80x24-open.ansi) | [details](popup-80x24-details.txt) |
| 120×32 | [rail](popup-120x32-rail.txt) | [popup](popup-120x32-open.txt) · [ANSI](popup-120x32-open.ansi) | [details](popup-120x32-details.txt) |
| 52×12 | [rail](popup-52x12-rail.txt) | [popup](popup-52x12-open.txt) · [ANSI](popup-52x12-open.ansi) | [details](popup-52x12-details.txt) |

[Stale Claude observation, preserved legacy default, and Codex API mode in the
rail](popup-120x32-stale-api-rail.txt) and [popup](popup-120x32-stale-api.txt).

Budget means reported subscription percentage remaining (`100 - usedPercent`).
The rail labels 5h/7d explicitly; other reported windows use a limiting-window
summary. Missing/invalid data stays unknown, expired or failed refreshes stay
stale, and API subscription budget is not applicable. The popup gives both
providers space before scrollable identity, provenance, and additional windows.
At 52×12 the overview is compact; reset/provenance details require scrolling.

Reproduce from the repository root:

```sh
AMUX_ACCOUNTS_RENDER_DIR="$PWD/docs/evidence/accounts-popup" \
  go test ./internal/nativetui -run '^TestAccountsPopupRenderEvidence$' -count=1
```

The fixture enables ANSI256 output. Observation timestamps reflect capture time;
provider resets and account identities are synthetic. The same test checks exact
frame bounds and identical frame/focus restoration after Escape. Separate tests
exercise zero/full/missing/stale budgets, API mode, wide-glyph and ANSI boundaries,
keyboard/paste/mouse capture against a mouse-reporting terminal, extra-window and
confirmation scrolling, and rail navigation hints at 80×12 and 80×24. Existing
ceremony, cancellation, hidden-input, revision, and stale-reply tests remain active.

Review scope: `internal/nativetui` and these evidence files only. Backend account
ownership, credential delivery, binding logic, and wire schemas are unchanged.

Validation at the checkpoint: `make test` (root and harnessproto), `make vet`,
`make build` (including notices and binary provenance), `make cross`, and
`make vuln` passed. Checks used a temporary Go cache; full tests used temporary
`XDG_DATA_HOME` data and approved dependency/test-socket access.

An optional `go test -race ./internal/nativetui` run reports an existing
`x/vt.Emulator.Read` / `Close` shutdown race. It also reproduces on an unchanged
archive of upstream `90ac3c7` with
`go test -race ./internal/nativetui -run '^TestRestartResultReattachesAgentAndPreservesTerminal$' -count=5`.
Terminal lifecycle code is outside this change. Required CI does not run `-race`.

Review follow-up: [multiple Codex categories](popup-120x32-multi-bucket.txt)
retain native bucket names (for example `code-review:5h`) in the popup and announce
additional windows in the selected provider's scrollable details. The rail uses
5h/7d only for unscoped windows or the general `codex` bucket; multiple or other
categories get the explicit limiting-window summary. A projection regression
covers code-review and codex buckets together, including access to later windows.
