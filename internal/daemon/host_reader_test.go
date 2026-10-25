package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"amux/internal/core"
)

func TestHostReaderObservesDisconnectWithoutDispatch(t *testing.T) {
	read, write := io.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	actions, done := readHostActions(bufio.NewReader(read), cancel)
	if err := json.NewEncoder(write).Encode(core.Action{Action: core.ActionQuery, Query: core.QueryAccountCeremony}); err != nil {
		t.Fatal(err)
	}
	// Dispatch is deliberately not draining the admitted frame.
	write.Close()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("EOF hidden behind dispatch")
	}
	<-done
	if a := <-actions; a.Query != core.QueryAccountCeremony {
		t.Fatal("frame reordered")
	}
}
func TestHostReaderRejectsOversizeAndQueueOverflow(t *testing.T) {
	for _, test := range []string{"frame", "queue"} {
		t.Run(test, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			input := strings.Repeat("x", hostActionMaxBytes+1)
			if test == "queue" {
				input = strings.Repeat("{\"action\":\"refresh\"}\n", hostActionQueue+1)
			}
			actions, done := readHostActions(bufio.NewReader(strings.NewReader(input)), cancel)
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("reader failed to bound input")
			}
			if ctx.Err() == nil {
				t.Fatal("overload connection not retired")
			}
			count := 0
			for range actions {
				count++
			}
			if count > hostActionQueue {
				t.Fatal("queue exceeded bound")
			}
		})
	}
}
