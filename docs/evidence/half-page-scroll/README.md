# Half-page pane history evidence

Source tested: `cd09bd9e78f1c8f4f844694e5d69c753bc89b868`.
The native fixture uses checksum-pinned Codex 0.157.1 with synthetic localhost
model responses, no account or external model requests.

Both `always` (forced alternate screen) and `no-alt` (inline) passed:

- `half-up-history`: repeated Ctrl+U recovers marker `CODEX-HISTORY-070`,
  which the fixture first observed in an earlier live rendered frame.
- `half-down-live`: Ctrl+D returns to live with `DRAFT-WHEEL-MUST-STAY`
  unchanged; the captured child input is identical before and after these keys.

These native frames come from production TUI rendering while driving model key
handlers. Separately, `TestHalfPageKeysPTY` sends actual 0x15/0x04 input through
Bubble Tea and checks the outer PTY's rendered earlier/live rows, zero child
bytes, persisted F5/F6 rebinding restoring child control bytes, and bracketed
paste of shortcut names, actual control characters, and multiline text.

Reproduce from the repository root with the pinned binary used by CI:

```sh
go test ./internal/keymap ./cmd/amux ./internal/vterm ./internal/nativetui
AMUX_TEST_CODEX_BIN=/path/to/codex-0.157.1 \
  AMUX_REQUIRE_CODEX_SCROLL=1 AMUX_CODEX_SCROLL_EVIDENCE=/tmp/half-page-evidence \
  go test ./internal/nativetui -run '^TestCodexEmbeddedScroll$' -count=1 -timeout=180s -v
go test ./internal/nativetui -run '^TestHalfPageKeysPTY$' -count=1 -v
```

The existing wheel assertions still run; wheel routing has no changes in this
work. Clipboard paths and paste handling also have no production changes.
