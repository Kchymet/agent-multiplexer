package local

import (
	"context"
	"strings"
	"testing"
	"time"

	"amux/internal/engine"
)

func TestUnchangedViewportRequestsRepaint(t *testing.T) {
	e := New()
	key := engine.Key{AgentID: "repaint"}
	in, err := e.Ensure(context.Background(), engine.Spec{
		Key: key, Cols: 80, Rows: 24,
		Argv: []string{"sh", "-c", `trap 'printf "REPAINT\n"' WINCH; printf 'READY\n'; while :; do read line; done`},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Kill(key)
	output := make(chan string, 100)
	cancel := in.Subscribe(engine.Sink{Output: func(b []byte) { output <- string(b) }})
	defer cancel()
	wait := func(want string) {
		t.Helper()
		timer := time.NewTimer(3 * time.Second)
		defer timer.Stop()
		var all strings.Builder
		for {
			select {
			case s := <-output:
				all.WriteString(s)
				if strings.Contains(all.String(), want) {
					return
				}
			case <-timer.C:
				t.Fatalf("terminal did not emit %s", want)
			}
		}
	}
	wait("READY")
	in.Resize(80, 24)
	wait("REPAINT")
	if !in.Alive() {
		t.Fatal("redraw stopped the instance")
	}
}
