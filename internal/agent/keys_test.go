package agent

import (
	"reflect"
	"testing"
	"time"
)

// TestKeysPerKind pins each registered kind's steering keystrokes to the bytes
// its CLI actually expects. Claude Code's were verified by driving a real 2.1.261
// in a PTY: Enter submits, "❯ 1. Yes" is focused on a permission prompt so Enter
// allows, Esc declines it, and Ctrl+C — not Esc — is what actually interrupts a
// turn (with "editorMode": "vim" set, Esc only enters the composer's NORMAL
// mode). Codex's come from the defaults in codex-rs/tui/src/keymap.rs for 0.153.2:
// composer.submit=Enter, chat.interrupt_turn=Esc, approval.approve='y',
// approval.decline=Esc|'n'.
//
// An upstream rebinding is a real risk, which is why this is a table a reader
// can re-verify rather than a computation — if a CLI changes its keys, this test
// is the place the change gets recorded.
func TestKeysPerKind(t *testing.T) {
	cases := []struct {
		kind string
		want Keys
	}{
		{"claude", Keys{
			Submit: []byte("\r"), Interrupt: []byte("\x03"), Allow: []byte("\r"), Deny: []byte("\x1b"),
			InterruptOnlyWhileBusy: true,
			PasteStart:             []byte("\x1b[200~"), PasteEnd: []byte("\x1b[201~"), SubmitDelay: 100 * time.Millisecond,
		}},
		{"codex", Keys{
			Submit: []byte("\r"), Interrupt: []byte("\x1b"), Allow: []byte("y"), Deny: []byte("n"),
		}},
	}
	for _, c := range cases {
		t.Run(c.kind, func(t *testing.T) {
			got := HarnessFor(c.kind).Keys()
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("Keys() = %v, want %v", got, c.want)
			}
			if !got.Steerable() {
				t.Fatal("Steerable() = false for a kind amux can drive")
			}
		})
	}
}

// TestDestructiveInterruptIsFlagged pins the property the daemon's `stop` guard
// keys off. Claude Code's Ctrl+C exits the CLI on a second press at an idle
// prompt, so it must be marked unsafe-when-idle; Codex's Esc is inert there and
// must not be, or `stop` would never fire for a runtime that reports no turn
// state at all.
func TestDestructiveInterruptIsFlagged(t *testing.T) {
	if !HarnessFor("claude").Keys().InterruptOnlyWhileBusy {
		t.Error("claude: Ctrl+C exits at an idle prompt and must be gated on a running turn")
	}
	if HarnessFor("codex").Keys().InterruptOnlyWhileBusy {
		t.Error("codex: Esc is inert when idle; gating it would make stop unusable")
	}
}

// TestUnsteerableKindsAreHonest: a harness amux can launch but whose UI it does
// not know must report no keys, so the daemon refuses the verb instead of typing
// bytes into a runtime it can't predict. hermes takes the vendor-neutral no-op
// defaults; an unregistered kind resolves to noopHarness.
func TestUnsteerableKindsAreHonest(t *testing.T) {
	for _, kind := range []string{"hermes", "some-future-cli"} {
		t.Run(kind, func(t *testing.T) {
			if k := HarnessFor(kind).Keys(); k.Steerable() {
				t.Fatalf("Keys() = %v reports steerable, want the honest zero value", k)
			}
		})
	}
}

// TestSteerableRequiresEveryKey proves the all-or-nothing rule: a half-known
// runtime is refused whole, so amux can never accept `prompt` on a kind whose
// permission answer it would get wrong.
func TestSteerableRequiresEveryKey(t *testing.T) {
	full := claudeKeys()
	if !full.Steerable() {
		t.Fatal("a complete mapping must be steerable")
	}
	for _, drop := range []func(Keys) Keys{
		func(k Keys) Keys { k.Submit = nil; return k },
		func(k Keys) Keys { k.Interrupt = nil; return k },
		func(k Keys) Keys { k.Allow = nil; return k },
		func(k Keys) Keys { k.Deny = nil; return k },
	} {
		if got := drop(full); got.Steerable() {
			t.Errorf("Steerable() = true for an incomplete mapping %v", got)
		}
	}
	if (Keys{}).Steerable() {
		t.Error("the zero Keys must not be steerable")
	}
}

// TestEveryHarnessAnswersKeys is the registry contract: Keys is part of the
// interface, so adding a kind means deciding its steering story rather than
// leaving a caller to switch on the kind string somewhere else.
func TestEveryHarnessAnswersKeys(t *testing.T) {
	for _, h := range Harnesses() {
		k := h.Keys()
		// Either a complete mapping or none at all — never a partial one.
		partial := !k.Steerable() &&
			(len(k.Submit) > 0 || len(k.Interrupt) > 0 || len(k.Allow) > 0 || len(k.Deny) > 0)
		if partial {
			t.Errorf("%s: Keys() = %v is partial; give it every key or none", h.Kind(), k)
		}
	}
}
