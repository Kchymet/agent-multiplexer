package agent

import "fmt"

// Keys is how one agent's interactive TUI is driven from outside: the byte
// sequences that submit a line of input, interrupt a turn, and answer the
// runtime's permission prompt. Steering a published session (docs/
// remote-provider-sessions.md §3.1) delivers these to the agent's PTY, so which
// bytes a runtime expects is per-kind data on the Harness — not a switch at the
// call site, which is exactly how the kind knowledge in this package leaked
// everywhere before the registry existed.
//
// A zero Keys means "this harness cannot be steered by keystroke": the caller
// refuses the verb with a clear error rather than typing bytes into a runtime
// whose UI it does not know. Steerable reports that.
//
// These are the CLIs' *default* bindings, and a user can rebind them (Claude
// Code via keybindings.json, Codex via `tui.keymap`). amux drives the defaults;
// a rebound runtime steers wrong, which is why the delivery mechanism is the
// daemon's business and the wire verb stays abstract.
type Keys struct {
	// Submit ends a line of typed input and sends it. Used for both `prompt` and
	// `interject` — the runtime itself decides whether text arriving mid-turn
	// starts a turn, queues, or steers.
	Submit []byte
	// Interrupt stops the current turn without killing the process (`stop`).
	Interrupt []byte
	// Allow answers a pending permission prompt affirmatively.
	Allow []byte
	// Deny answers a pending permission prompt negatively.
	Deny []byte
	// InterruptOnlyWhileBusy marks an Interrupt key that is destructive when no
	// turn is running — Claude Code's Ctrl+C clears the composer on the first
	// press at an idle prompt and *exits* on the second, so a caller repeating
	// `stop` at an idle agent would kill the very session the verb promises to
	// keep alive. The daemon refuses to send such a key unless the harness reports
	// the agent mid-turn. A key that is merely inert when idle leaves this false.
	InterruptOnlyWhileBusy bool
}

// Control bytes the harnesses' key mappings are built from. Named so a reader
// sees the key, not a magic escape.
var (
	keyEnter = []byte("\r")   // carriage return: what a terminal sends for Enter
	keyEsc   = []byte("\x1b") // ESC
	keyCtrlC = []byte("\x03") // ETX: what a terminal sends for Ctrl+C
)

// String renders the mapping with its control bytes visible, so a test failure or
// a log line reads "submit=\r interrupt=\x03" rather than a slice of numbers.
func (k Keys) String() string {
	s := fmt.Sprintf("submit=%q interrupt=%q allow=%q deny=%q",
		k.Submit, k.Interrupt, k.Allow, k.Deny)
	if k.InterruptOnlyWhileBusy {
		s += " interrupt-only-while-busy"
	}
	return s
}

// Steerable reports whether these keys are complete enough to steer with: every
// verb's bytes must be present, so a half-known runtime is refused as a whole
// rather than accepting `prompt` and silently mis-answering `permission`.
func (k Keys) Steerable() bool {
	return len(k.Submit) > 0 && len(k.Interrupt) > 0 && len(k.Allow) > 0 && len(k.Deny) > 0
}

// claudeKeys maps Claude Code's interactive bindings, verified by driving a real
// CLI 2.1.261 in a PTY:
//
//   - Enter submits the composer. While a turn runs Claude Code *queues* the
//     message and sends it at the next boundary — which is precisely `interject`.
//   - A permission prompt renders its first option focused ("❯ 1. Yes"), and
//     Enter submits the focused answer, so allow needs no option counting.
//   - Esc on a permission prompt declines it — the prompt itself says "Esc to
//     cancel", and the run leaves the tool uncalled and the session alive.
//
// Interrupt is Ctrl+C, not the Esc the docs headline, because Esc is not
// reliable here: with "editorMode": "vim" in Claude Code's settings — a per-user
// setting amux cannot assume away — Esc is swallowed by the composer to enter vim
// NORMAL mode and the turn keeps running. Worse, the composer is then left in
// NORMAL mode, so the next steering text loses the characters vim reads as
// commands (an "Instead…" arriving as "nstead…"). Ctrl+C interrupts in both modes
// and leaves the composer alone. Its cost is that a second press at an *idle*
// prompt exits Claude Code, hence InterruptOnlyWhileBusy.
func claudeKeys() Keys {
	return Keys{
		Submit: keyEnter, Interrupt: keyCtrlC, Allow: keyEnter, Deny: keyEsc,
		InterruptOnlyWhileBusy: true,
	}
}

// codexKeys maps Codex's interactive bindings (verified against CLI 0.153.2 and
// the defaults in codex-rs/tui/src/keymap.rs):
//
//   - composer.submit = Enter. Sent during an active turn, Codex steers the
//     running model with it rather than starting a second turn.
//   - chat.interrupt_turn = Esc.
//   - approval.approve = "y".
//   - approval.decline = Esc or "n"; amux sends "n" so a decline can never be
//     mistaken for the Esc that, on an empty composer, steps back to edit the
//     previous message instead.
//
// Esc stays the interrupt here: it is Codex's own chat-level binding rather than
// a composer key, and at an idle composer it merely steps back to edit the last
// message — inert, not destructive — so it needs no busy guard.
func codexKeys() Keys {
	return Keys{Submit: keyEnter, Interrupt: keyEsc, Allow: []byte("y"), Deny: []byte("n")}
}
