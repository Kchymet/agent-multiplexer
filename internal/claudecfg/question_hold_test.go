package claudecfg

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAskUserQuestionTimeout(t *testing.T) {
	dir := t.TempDir()
	h := At(dir)
	if _, ok := h.AskUserQuestionTimeout(); ok {
		t.Fatal("no settings: never")
	}
	for v, want := range map[string]time.Duration{`"60s"`: time.Minute, `"5m"`: 5 * time.Minute, `"10m"`: 10 * time.Minute, `"never"`: 0, `""`: 0, `42`: 0} {
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"askUserQuestionTimeout":`+v+`}`), 0o600); err != nil {
			t.Fatal(err)
		}
		got, ok := h.AskUserQuestionTimeout()
		if got != want || ok != (want > 0) {
			t.Errorf("%s: %v %v, want %v", v, got, ok, want)
		}
	}
}
