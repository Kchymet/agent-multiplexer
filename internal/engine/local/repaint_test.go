package local

import (
	"context"
	"strings"
	"testing"
	"time"

	"amux/internal/engine"
	"github.com/creack/pty"
)

func TestUnchangedViewportRequestsRepaint(t *testing.T) {
	e := New()
	key := engine.Key{AgentID: "repaint"}
	in, err := e.Ensure(context.Background(), engine.Spec{
		Key: key, Cols: 80, Rows: 24,
		Argv: []string{"sh", "-c", `previous=$(stty size); trap 'current=$(stty size); if [ "$current" != "$previous" ]; then printf "REPAINT %s\n" "$current"; previous=$current; fi' WINCH; printf 'READY\n'; while :; do read line; done`},
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
	in.Resize(80, 24) // Concurrent same-size attachments must still allow repaint.
	wait("REPAINT 24 79")
	wait("REPAINT 24 80")
	if !in.Alive() {
		t.Fatal("redraw stopped the instance")
	}
}

func TestNewResizeSupersedesPendingRepaint(t *testing.T) {
	e := New()
	key := engine.Key{AgentID: "resize-wins"}
	in, err := e.Ensure(context.Background(), engine.Spec{Key: key, Cols: 80, Rows: 24, Argv: []string{"sh", "-c", "while :; do read line; done"}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Kill(key)
	in.Resize(80, 24)
	in.Resize(100, 30)
	time.Sleep(300 * time.Millisecond)
	local := in.(*instance)
	local.mu.Lock()
	size, err := pty.GetsizeFull(local.ptmx)
	local.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if size.Cols != 100 || size.Rows != 30 {
		t.Fatalf("stale repaint restored viewport to %dx%d", size.Cols, size.Rows)
	}
}
