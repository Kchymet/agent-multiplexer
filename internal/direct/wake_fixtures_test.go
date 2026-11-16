package direct

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// The wake-v1 fixtures are the direct wake API contract clients copy byte for
// byte (docs/direct-wake-contract.md §6.3, §8). They are generated from the
// server's own types; requests are also decoded strictly, as the server does.
// Regenerate with UPDATE_DIRECT_FIXTURES=1 go test ./internal/direct -run TestWakeFixtures.
const wakeFixtureDir = "../../docs/direct/fixtures/wake-v1"

const (
	fxRoute  = "dwr_q8Hq0xV0y3Yb2mJ1wQ7p4uZr9tN6cD5eF8gH1iJ2kL3"
	fxWake1  = "dwk_AAECAwQFBgcICQoLDA0ODw"
	fxWake2  = "dwk_EBESExQVFhcYGRobHB0eHw"
	fxSecret = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
)

func wakeFixtureFiles(t *testing.T) map[string][]byte {
	t.Helper()
	js := func(v any) []byte { return append(mustIndent(t, v), '\n') }
	f, tr := false, true
	open := WakeItem{Seq: 41, WakeID: fxWake1, Kind: WakeKindPermission, State: WakeStateOpen, Acked: false,
		SessionID: "e1deda", Incarnation: "inc-7f3a", ItemID: "item_9", RequestID: "call_7", RuntimeGeneration: "rg_01J9",
		Ordinal: 123, CreatedAt: 1793000000000}
	turn := WakeItem{Seq: 42, WakeID: fxWake2, Kind: WakeKindTurnEnd, State: WakeStateDone, SessionID: "e1deda",
		Incarnation: "inc-7f3a", Ordinal: 130, CreatedAt: 1793000060000}
	resolved := WakeItem{Seq: 44, WakeID: fxWake1, Kind: WakeKindPermission, State: WakeStateResolved, CreatedAt: 1793000000000}
	withdrawn := WakeItem{Seq: 45, WakeID: fxWake2, Kind: WakeKindTurnEnd, State: WakeStateWithdrawn, CreatedAt: 1793000060000}
	return map[string][]byte{
		"activation-challenge.json": js(map[string]string{
			"activation_secret": fxSecret, "activation_challenge": hp.DirectWakeChallenge(fxSecret),
		}),
		"capabilities-wake.json":        js(WakeCapabilities{V: 1, Enabled: true, Degraded: false}),
		"subscribe-request.json":        js(WakeSubscribeRequest{RouteID: fxRoute, ActivationSecret: fxSecret}),
		"subscribe-response.json":       js(WakeSubView{RouteID: fxRoute, State: wakeSubStateActivate, CreatedAt: 1793000000}),
		"subscribe-error-disabled.json": js(APIError{Code: CodeWakeDisabled, Error: "wake-ups are not enabled on this host"}),
		"unsubscribe-request.json":      js(WakeRouteRequest{RouteID: fxRoute}),
		"unsubscribe-response.json":     js(WakeUnsubscribeResponse{Removed: true}),
		"subscriptions-request.json":    js(WakeSubscriptionsRequest{}),
		"subscriptions-response.json": js(WakeSubscriptionsResponse{Subscriptions: []WakeSubscription{
			{WakeSubView: WakeSubView{RouteID: fxRoute, State: wakeSubStateActive, CreatedAt: 1793000000},
				Watches: []WakeWatch{{SessionID: "e1deda", Incarnation: "inc-7f3a"}}},
			{WakeSubView: WakeSubView{RouteID: "dwr_AQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", State: wakeSubStateFailed,
				Failure: WakeFailActivationExpired, CreatedAt: 1792990000}, Watches: []WakeWatch{}},
		}}),
		"watch-request.json":  js(WakeWatchRequest{RouteID: fxRoute, SessionID: "e1deda", Incarnation: "inc-7f3a", Watch: true}),
		"watch-response.json": js(WakeWatchResponse{RouteID: fxRoute, SessionID: "e1deda", Incarnation: "inc-7f3a", Watched: true}),
		"watch-error-incarnation-changed.json": js(struct {
			APIError
			Incarnation string `json:"incarnation"`
		}{APIError{Code: CodeWakeIncarnation, Error: "the session instance changed"}, "inc-8b21"}),
		"events-request-incremental.json": js(WakeFetchRequest{RouteID: fxRoute, AfterSeq: 40, Limit: 64}),
		"events-response-incremental.json": js(WakeFetchResponse{RouteID: fxRoute, Items: []WakeItem{open, turn},
			NextSeq: 42, HasMore: false}),
		"events-response-clearing.json": js(WakeFetchResponse{RouteID: fxRoute, Items: []WakeItem{resolved, withdrawn},
			NextSeq: 45, HasMore: false}),
		"events-response-gap.json": js(WakeFetchResponse{RouteID: fxRoute, Items: []WakeItem{}, NextSeq: 45,
			Gap: &WakeGap{ThroughSeq: 43, Reason: WakeGapCap}}),
		"events-response-gap-reset.json": js(WakeFetchResponse{RouteID: fxRoute, Items: []WakeItem{}, NextSeq: 12,
			Gap: &WakeGap{ThroughSeq: 12, Reason: WakeGapReset}}),
		"events-request-snapshot-first.json": js(WakeFetchRequest{RouteID: fxRoute, Snapshot: true, Limit: 64}),
		"events-request-snapshot-next.json":  js(WakeFetchRequest{RouteID: fxRoute, Snapshot: true, SnapshotHW: 45, AfterSeq: 41, Limit: 64}),
		"events-response-snapshot-incomplete.json": js(WakeFetchResponse{RouteID: fxRoute, Items: []WakeItem{open},
			NextSeq: 45, SnapshotHW: 45, Complete: &f, Reason: WakeGapExpired, LossThrough: 39}),
		"events-request-snapshot-recovered.json": js(WakeFetchRequest{RouteID: fxRoute, Snapshot: true, Limit: 64, RecoveredThrough: 39}),
		"events-response-snapshot-complete.json": js(WakeFetchResponse{RouteID: fxRoute, Items: []WakeItem{open},
			NextSeq: 45, SnapshotHW: 45, Complete: &tr}),
		"ack-request.json":  js(WakeAckRequest{RouteID: fxRoute, Items: []WakeAckItem{{WakeID: fxWake1, Seq: 44}, {WakeID: fxWake2, Seq: 45}}}),
		"ack-response.json": js(WakeAckResponse{Acked: 2}),
	}
}

func TestWakeFixtures(t *testing.T) {
	want := wakeFixtureFiles(t)
	want["SHA256SUMS"] = sums(want)
	if os.Getenv("UPDATE_DIRECT_FIXTURES") == "1" {
		if err := os.MkdirAll(wakeFixtureDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for n, b := range want {
			if err := os.WriteFile(filepath.Join(wakeFixtureDir, n), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	entries, err := os.ReadDir(wakeFixtureDir)
	if err != nil {
		t.Fatalf("missing wake fixtures (regenerate with UPDATE_DIRECT_FIXTURES=1): %v", err)
	}
	if len(entries) != len(want) {
		t.Errorf("wake fixture dir has %d files, contract has %d", len(entries), len(want))
	}
	for n, b := range want {
		got, err := os.ReadFile(filepath.Join(wakeFixtureDir, n))
		if err != nil || !bytes.Equal(got, b) {
			t.Errorf("fixture %s drifted from the server types (regenerate with UPDATE_DIRECT_FIXTURES=1)", n)
		}
	}
	// Requests decode strictly into the server's request types.
	strict := func(name string, v any) {
		dec := json.NewDecoder(bytes.NewReader(want[name]))
		dec.DisallowUnknownFields()
		if err := dec.Decode(v); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	strict("subscribe-request.json", &WakeSubscribeRequest{})
	strict("unsubscribe-request.json", &WakeRouteRequest{})
	strict("subscriptions-request.json", &WakeSubscriptionsRequest{})
	strict("watch-request.json", &WakeWatchRequest{})
	strict("events-request-incremental.json", &WakeFetchRequest{})
	strict("events-request-snapshot-first.json", &WakeFetchRequest{})
	strict("events-request-snapshot-next.json", &WakeFetchRequest{})
	strict("events-request-snapshot-recovered.json", &WakeFetchRequest{})
	strict("ack-request.json", &WakeAckRequest{})
	// Clearing revisions carry ids only.
	var clearing WakeFetchResponse
	_ = json.Unmarshal(want["events-response-clearing.json"], &clearing)
	for _, it := range clearing.Items {
		if it.SessionID != "" || it.RequestID != "" || it.Incarnation != "" || it.ItemID != "" {
			t.Errorf("clearing fixture carries session metadata: %+v", it)
		}
	}
}
