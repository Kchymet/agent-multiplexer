# Pane selection through the real renderer

These 70×10 frames come from the output PTY of the production Bubble Tea
constructor, decoded into an outer terminal emulator. They are not direct
`model.View()` dumps. The daemon connection and clipboard delivery are replaced
with deterministic fixtures; the latter writes OSC52 only to the test output
PTY. The same constructor and unchanged Accounts PTY tests remain in use after
integrating master through PR191 and PR192.

Run from the repository root:

```sh
AMUX_SELECTION_EVIDENCE="$PWD/docs/evidence/pane-selection" \
  go test ./internal/nativetui -run '^TestSelectionPTYRendererAndClipboard$' -count=1
```

- `selected`: raw SGR Shift-left press, unmodified motion, and actual rendered
  selection of `界é`. The wide glyph is selected whole; no local-selection
  clipboard write occurs before release and the child receives no mouse bytes.
- Before selection, a visible-app OSC52 write is delivered through the same
  terminal-only test sink; hidden-pane writes and clipboard queries are ignored.
- Release adds exactly one `OSC 52 ; c ; 55WMw6k= BEL` through the same output
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

The same raw-PTY test also pastes `q`, `s`, `alt+h`, `alt+1`, `ctrl+c`, and
multiline Unicode text through bracketed-paste decoding. Exact text bytes reach
the child and focus remains on the pane. This preserves the existing text path
(which does not re-emit bracket delimiters); it does not test image paste.

App copy and local copy share one injected terminal-only sink and the production
SerialFile. This fixture creates no Copier worker and cannot resolve or invoke
host clipboard tools, even on WSL. Clipboard unit tests separately exercise
Copier platform/tmux routes with fake dependencies. Daemon replay stripping
remains covered by the existing replay regressions.

`validation.txt` records the integrated master preflight and actual PTY/native
fixture results. Native Codex uses synthetic localhost responses without a
user account or model request.
