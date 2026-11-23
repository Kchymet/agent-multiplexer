# Pane fixtures for typed delivery

Each `*.pane` file is one terminal screen an agent runtime painted, replayed
through the production emulator (`vterm.Capture`) and classified by
`composer.Classify`. `TestPaneFixtures` (composer) and the typed-delivery tests
(daemon) are driven by them.

Format: `#` header lines, a `---8<---` line, then the body.

- `runtime`, `size` (cols x rows) — how to replay it.
- `provenance` — **REAL** (bytes a genuine runtime wrote, cut unedited from a
  checked-in capture) or **RECONSTRUCTED** (hand-written to the runtime's layout;
  the header says what it is modeled on).
- `want-state` (`free` | `draft` | `dialog` | `unknown`), `want-busy`,
  `want-text` (Go-quoted composer text) — the expected classification.
- `encoding: raw` — the body is the byte stream itself.
- `encoding: escaped` — each body line is the inside of a Go string literal
  (`\x1b`, ` `, `\"`); lines are joined with CR LF, and `{{RULE}}` expands
  to a rule the width of the pane.

Why some are reconstructed: a nested Claude Code could not be launched from
the sandbox that produced this change (no binary on PATH, and starting one
against a local fake Messages API was refused by the session's permission
policy), so no busy Claude Code pane could be captured live. The Claude Code
fixtures follow the real 2.1.263 screen CI captured (TestNativeClaudeTypedDelivery:
`❯` + NO-BREAK SPACE prompt between two full-width rules, dim footer) and the
2.1 layouts of the spinner, tool-running, queued-message, permission and
auto-mode screens. Replace them with live captures when available.
