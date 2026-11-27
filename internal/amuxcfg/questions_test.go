package amuxcfg

import (
	"os"
	"testing"

	"amux/internal/core"
)

func TestQuestionHoldSwitch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if !ResolveQuestionHold() {
		t.Fatal("the hold is on by default")
	}
	off := false
	if err := SetQuestionHold(&off); err != nil {
		t.Fatal(err)
	}
	if ResolveQuestionHold() {
		t.Fatal("saved off still reads on")
	}
	if v, set, err := QuestionHold(); err != nil || !set || v {
		t.Fatalf("QuestionHold = %v %v %v", v, set, err)
	}
	if err := SetQuestionHold(nil); err != nil {
		t.Fatal(err)
	}
	if _, set, _ := QuestionHold(); set || !ResolveQuestionHold() {
		t.Fatal("unset did not restore the default")
	}
	// An unreadable config fails open to the hold (which itself fails open to
	// the pane).
	if err := os.WriteFile(core.ConfigPath(), []byte(`{"questions":{"claude_hold":"maybe"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !ResolveQuestionHold() {
		t.Fatal("an invalid value must read as the default")
	}
}
