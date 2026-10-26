# Pane text selection and copy

Drag the left mouse button over a pane to select text. Releasing a nonempty
selection queues a clipboard copy through the same Copier used by app copy.
An ordinary click does not copy. “Copy queued” confirms the request was queued,
not delivered or accepted. No clipboard reads are performed.

The Copier sends OSC52 to the outer terminal and tries the host platform's
clipboard tools (pbcopy, wl-copy/xclip/xsel, or WSL's Windows clipboard tools).
Under tmux it uses `tmux load-buffer -w`, with OSC52 passthrough as a fallback.
Terminal policy, tmux configuration, and installed tools determine delivery.

Mouse priority at the start of a gesture:

1. Shift forces local selection, including Shift+Alt.
2. Otherwise Alt passes the gesture to the child.
3. Otherwise an app that requests mouse reporting receives the gesture.
4. Otherwise left drag selects locally.

Some outer terminals intercept Shift+mouse for their own selection and never
forward it to amux. Shift can force pane-local handling only when those events
reach amux; configure the outer terminal to pass them through if needed.

The owner chosen at press keeps the gesture through release, even if modifiers
change. Local motion and release outside the pane clamp to its edges; they cannot
select or activate the rail. An app-owned gesture preserves changed modifiers
and delivers its release to the original terminal, including after a pane switch.
Events remain subject to the child's mouse protocol. Alt cannot make a child
that has not enabled mouse reporting receive mouse bytes.

Selection works on the live screen and the history currently visible in the
pane. Dragging backward selects the same text as dragging forward. Either half
of a wide character selects the whole glyph. Highlighting and copying use the
same cell ranges and preserve grapheme text present in the emulator snapshot.
The pinned emulator can discard combining accents after ASCII base characters;
selection cannot recover text absent from the displayed cells.

Copy separates physical rows with newlines. Wrapped rows are not joined because
the history stores physical rows without wrap metadata. Trailing spaces are
trimmed at the pane's right edge; spaces at a partial-row endpoint are preserved.
Selection does not auto-scroll beyond the visible viewport.

Selection clears on output in its pane, any keyboard input/navigation, resize,
viewport movement, reset/reconnect, pane or tab switch, pane close, or opening a
modal. Active output can therefore cancel a drag. Other panes' output does not
cancel it. A cancelled local gesture consumes its remaining motion and release
without copying or forwarding them. A revision check also catches output parsed
before the next UI notification, so stale coordinates cannot copy different text.

The local selection feature requests a copy only after an explicit completed
drag. Its rendering, ordinary clicks, empty selections, and cancelled gestures
never initiate a copy. This is separate from an application's own copy action:
the visible pane's OSC52 writes use the same Copier. Hidden-pane writes are
dropped, clipboard queries are ignored, and OSC52 is stripped from daemon replay
so attaching again does not repeat an old copy.
The UI and local selection copy share one serialized output writer that preserves
the terminal descriptor used for TTY detection and resize.

## Codex and Claude copy/paste compatibility

When Codex or Claude enables mouse reporting, ordinary gestures still reach the
app, including its own selection/copy behavior; Alt explicitly passes through,
and Shift requests local selection when the outer terminal forwards it. An
app-generated OSC52 copy uses the visible-pane guard and shared delivery routes
above. Local selection also works for apps that do not report mouse events.
Alt cannot enable mouse reporting in an app that has not requested it.

Text pasted by the outer terminal follows the existing focused-pane input path.
Bracketed paste is decoded into a text key event, and its text bytes are sent to
the child; amux does not read the clipboard to implement paste. The existing
path does not re-emit bracketed-paste delimiters. The PTY regression verifies
exact multiline/Unicode text and shortcut-looking strings such as `q`, `s`, and
`alt+h` reach the child without invoking amux navigation. This is text transport
coverage, not a claim about every native app's interpretation of those bytes.

The native Codex fixture verifies history and draft preservation, while the
outer-PTY fixture verifies copy/paste transport with deterministic child output.
Neither test establishes image-paste support or lack of support, nor exercises
native Codex/Claude clipboard menus against a real desktop clipboard.
