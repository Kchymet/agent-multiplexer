package runtimeevents

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// TestStructuredSingleSourceInterleavedReplayKeepsOrdinals is the canonical-source
// answer to ROOT's interleaved-replay counterexample (bin/root-coordination/
// interleaved-replay-review.go.txt): two independently-growing files (a journal and a
// structured log) can never expose a single monotonic cursor that a live tail and a
// single reconnect sweep both reproduce, because the files have no cross-file order.
//
// The fix moves the merge upstream: a structured session resolves to ONE canonical
// source — the supervisor's append-only event log — into which amux's own cold-start
// and failure notices are appended too (codexapp.AppendNotice). With one ordered file
// an event's ordinal is simply its line number, so the ordinals a resuming subscriber
// dedups on are identical whether it followed the live stream or reconnected — even
// when a late failure notice interleaves after the first turn's output. This test
// asserts exactly that at the runtimeevents boundary.
func TestStructuredSingleSourceInterleavedReplayKeepsOrdinals(t *testing.T) {
	events := filepath.Join(t.TempDir(), "structured.events.jsonl")
	if err := os.WriteFile(events, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Single canonical source: structured Path only, NO journal source.
	rec := Record{Runtime: harnessproto.RuntimeCodex, Path: events, Structured: true}
	resolve := func(string) (Record, bool) { return rec, true }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, ok := Stream(resolve, 5*time.Millisecond)(ctx, "session", 0)
	if !ok {
		t.Fatal("stream unavailable")
	}

	// The exact interleave from the counterexample: a cold-start notice, then a turn's
	// text, then a LATE failure notice — all appended to the one canonical file.
	lines := []string{
		`{"type":"notice","direction":"meta","payload":{"level":"info","text":"J1 startup"}}`,
		`{"type":"text","direction":"out","payload":{"text":"S1 first turn"}}`,
		`{"type":"notice","direction":"meta","payload":{"level":"error","text":"J2 late failure"}}`,
	}
	var live []harnessproto.RuntimeEvent
	for _, ln := range lines {
		f, err := os.OpenFile(events, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.WriteString(ln + "\n")
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		select {
		case b := <-ch:
			live = append(live, b.Events...)
		case <-time.After(time.Second):
			t.Fatal("no live event for appended line")
		}
	}
	cancel()
	if len(live) != len(lines) {
		t.Fatalf("want %d live events, got %d", len(lines), len(live))
	}

	// Reconnect after every cursor and require the replayed suffix to match the live
	// stream exactly — no loss (S1 at afterSeq=1), no relabel, no reorder.
	for after := 0; after < len(live); after++ {
		rctx, rcancel := context.WithCancel(context.Background())
		replay, _ := Stream(resolve, 5*time.Millisecond)(rctx, "session", int64(after))
		var got []harnessproto.RuntimeEvent
		timeout := time.After(time.Second)
		for len(got) < len(live)-after {
			select {
			case b := <-replay:
				got = append(got, b.Events...)
			case <-timeout:
				rcancel()
				t.Fatalf("afterSeq=%d replay incomplete: got %d of %d", after, len(got), len(live)-after)
			}
		}
		rcancel()
		if !reflect.DeepEqual(got, live[after:]) {
			t.Errorf("afterSeq=%d replay relabeled/reordered ordinals:\n got=%+v\nwant=%+v", after, got, live[after:])
		}
	}
}
