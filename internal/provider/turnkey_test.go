package provider

import (
	"testing"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// withTurnKeys stamps only turn_end events of a labelled batch that carry a
// complete source position; unlabelled batches and other events are left
// untouched, and the batch's own events are not mutated.
func TestWithTurnKeys(t *testing.T) {
	b := harnessproto.RuntimeEventBatch{
		Events: []harnessproto.RuntimeEvent{{Type: harnessproto.TypeText}, {Type: harnessproto.TypeTurnEnd}, {Type: harnessproto.TypeTurnEnd}},
		Positions: map[int]harnessproto.TurnSourcePos{
			1: {Kind: "codex", Index: 7, LineDigest: "bGluZTE"},
			2: {Kind: "codex", Index: 9}, // no first-line digest: unknown
		},
	}
	got := withTurnKeys("s1", "inc-1", b)
	if want := harnessproto.DirectTurnKey("s1", "inc-1", "codex", "bGluZTE", 7); got[1].Occurrence != want || want == "" {
		t.Fatalf("labelled turn_end occurrence = %q, want %q", got[1].Occurrence, want)
	}
	if got[0].Occurrence != "" || got[2].Occurrence != "" {
		t.Fatalf("unexpected occurrences: %+v", got)
	}
	if b.Events[1].Occurrence != "" {
		t.Fatal("batch events mutated")
	}
	for _, ev := range withTurnKeys("s1", "", b) {
		if ev.Occurrence != "" {
			t.Fatal("an unlabelled batch got a turn key")
		}
	}
}
