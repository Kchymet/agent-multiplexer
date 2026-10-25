# Pane selection through the real renderer

These 70×10 frames come from the output PTY of the production Bubble Tea
constructor, decoded into an outer terminal emulator. They are not direct
`model.View()` dumps. Only the model's daemon connection is replaced with a
deterministic fixture. The same constructor and unchanged Accounts PTY tests
remain in use after merging master through PR190.

Run from the repository root:

```sh
AMUX_SELECTION_EVIDENCE="$PWD/docs/evidence/pane-selection" \
  go test ./internal/nativetui -run '^TestSelectionPTYRendererAndClipboard$' -count=1
```

- `selected`: raw SGR Shift-left press, unmodified motion, and actual rendered
  selection of `界é`. The wide glyph is selected whole; the terminal receives no
  clipboard escape before release and the child receives no mouse bytes.
- Release emits exactly one `OSC 52 ; c ; 55WMw6k= BEL` through the same output
  writer used by Bubble Tea. The test checks the actual PTY byte stream.
- `history`: raw Alt+PageUp scrolls primary history; the outer renderer shows
  the header indicator, and the child receives no key bytes.
- `live`: raw Alt+End returns live and removes the indicator. The test also
  changes the PTY to 80×12, asks Bubble Tea to query the real output descriptor,
  and verifies a 53×9 pane. This catches loss of the terminal file interface.

Additional model/terminal integration tests select retained alternate-screen
frames through the corrected PR190 Snapshot API and exercise reversed wide
selection, outside/rail release, reporting modes, modifier changes, multiple
buttons, pane switches, output/reset/resize invalidation and cancelled tails.

The native Codex test supplied by PR2 uses a real child PTY but calls TUI model
handlers directly. The test above supplies complementary input-decoder and
outer-renderer coverage; it does not claim a native Codex selection run.

Clipboard policy here concerns explicit **local selection**. An independently
enabled active-app OSC52 copy path remains a separate feature and must keep its
active-pane/replay guards when present in the base.
