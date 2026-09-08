package sessionrpc

import (
	"context"
	"encoding/json"
	"testing"

	"amux/internal/access"
	"amux/internal/core"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// TestRuntimeEventPageRoundTripsOrdinaryFileRPC pins the transport half of the
// scoped query without a socket or transcript mount. Daemon authorization and
// source selection have separate focused tests in internal/daemon.
func TestRuntimeEventPageRoundTripsOrdinaryFileRPC(t *testing.T) {
	want := core.RuntimeEventPage{
		Target: "member", Runtime: harnessproto.RuntimeCodex, CursorSequence: 6, ScannedSequence: 7,
		Events: []core.SequencedRuntimeEvent{{Sequence: 7, Event: harnessproto.RuntimeEvent{
			Type: harnessproto.TypeNotice, Direction: harnessproto.DirMeta, Payload: json.RawMessage(`{"text":"ok"}`),
		}}},
		NextCursor: "next", HasMore: true,
	}
	body, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	fixture := newFixture(t, Callbacks{
		Authorize: func(_ context.Context, principal access.Principal, call Call) error {
			if principal.SubjectID != "subject-a" || call.Route != access.RouteQuery ||
				call.Verb != core.QueryRuntimeEvents || call.ID != "member" ||
				call.Fields[core.RuntimeEventsAfterSequenceField] != "6" {
				t.Fatalf("authorized call = %+v principal=%+v", call, principal)
			}
			return nil
		},
		Dispatch: func(_ context.Context, request DispatchRequest) (DispatchResult, error) {
			return DispatchResult{Status: StatusOK, Body: body}, nil
		},
	}, ServerOptions{})
	client := fixture.client(clientOptions{})
	result, err := pumpClient(t, fixture.server, func(ctx context.Context) (Result, error) {
		return client.Query(ctx, Query{
			Verb: core.QueryRuntimeEvents, ID: "member",
			Fields: map[string]string{core.RuntimeEventsAfterSequenceField: "6"},
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Body) > MaxResponseBody {
		t.Fatalf("response body = %d bytes", len(result.Body))
	}
	var got core.RuntimeEventPage
	if err := json.Unmarshal(result.Body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Target != want.Target || got.NextCursor != want.NextCursor || len(got.Events) != 1 || got.Events[0].Sequence != 7 {
		t.Fatalf("page = %+v", got)
	}
}
