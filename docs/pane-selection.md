# Pane text selection and copy

Drag the left mouse button over a pane to select text. Releasing a nonempty
selection sends a clipboard copy request to the outer terminal using OSC52.
The terminal must support and permit OSC52; “copy sent to terminal” confirms
the request was written, not that the terminal accepted it. An ordinary click
does not copy. No clipboard reads are performed.

Mouse priority at the start of a gesture:

1. Shift forces local selection, including Shift+Alt.
2. Otherwise Alt passes the gesture to the child.
3. Otherwise an app that requests mouse reporting receives the gesture.
4. Otherwise left drag selects locally.

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
when enabled by the hosted-app clipboard feature, the active pane's OSC52 writes
can also reach the clipboard under that feature's active-pane and replay guards.
The UI and local selection copy share one serialized output writer that preserves
the terminal descriptor used for TTY detection and resize.
