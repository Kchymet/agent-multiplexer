package enrich

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// deploy is a fake pipeline "deploy" record, to prove the registry and the
// rollup are generic. Not shipped.
type deploy struct {
	Name  string `json:"name"`
	State string `json:"state"` // a normalized state
	Ended string `json:"ended,omitempty"`
}

var (
	errAuth = errors.New("deploy: not logged in")
	errRate = errors.New("deploy: rate limited")
	errFlap = errors.New("deploy: 502")
)

// fakeSource reports whatever deploys it is told to, per session.
type fakeSource struct {
	kind     string
	interval time.Duration
	deploys  map[string][]deploy // by session id
	err      error
	resetAt  time.Time
	polls    int
	scopes   []Scope
}

func (f *fakeSource) Kind() string            { return f.kind }
func (f *fakeSource) Interval() time.Duration { return f.interval }
func (f *fakeSource) Retention() Retention    { return Retention{Window: time.Hour, Max: 2} }

func (f *fakeSource) Poll(_ context.Context, scope Scope, prev map[string][]Entry) (Observation, error) {
	f.polls++
	f.scopes = append(f.scopes, scope)
	if errors.Is(f.err, errAuth) {
		return Observation{}, f.err
	}
	obs := Observation{Sessions: map[string][]Entry{}}
	for _, s := range scope.Sessions {
		was := map[string]string{}
		for _, e := range prev[s.ID] {
			var d deploy
			_ = json.Unmarshal(e.Data, &d)
			was[e.Key] = d.State
		}
		var entries []Entry
		for _, d := range f.deploys[s.ID] {
			data, _ := json.Marshal(d)
			e := Entry{Key: d.Name, Open: d.State != hp.StatusDone && d.State != hp.StatusFailed, ObservedAt: now, Data: data}
			if d.Ended != "" {
				e.EndedAt, _ = time.Parse(time.RFC3339, d.Ended)
			}
			entries = append(entries, e)
			if was[d.Name] != d.State {
				obs.Changes = append(obs.Changes, Change{SessionID: s.ID, Key: d.Name, Changes: []string{was[d.Name] + "→" + d.State}})
			}
		}
		obs.Sessions[s.ID] = entries
	}
	return obs, f.err
}

func (f *fakeSource) Classify(err error) Failure {
	switch {
	case errors.Is(err, errAuth):
		return Failure{Class: FailSetup, Reason: "deploy_auth"}
	case errors.Is(err, errRate):
		return Failure{Class: FailRateLimit, Reason: hp.StaleRateLimited, ResetAt: f.resetAt}
	}
	return Failure{Class: FailTransient, Reason: hp.StaleError}
}

func (f *fakeSource) Render(e Entry) hp.Enrichment {
	var d deploy
	_ = json.Unmarshal(e.Data, &d)
	return hp.Enrichment{ID: d.Name, Title: "deploy " + d.Name, URL: "https://deploy.test/" + d.Name, Label: d.Name + " " + d.State,
		Status: hp.EnrichmentStatus{State: d.State, Detail: d.State}}
}

type regHarness struct {
	reg    *Registry
	src    *fakeSource
	saved  map[string]string
	events []Change
	logs   []string
	clock  time.Time
}

func newRegHarness(t *testing.T, saved map[string]string) *regHarness {
	t.Helper()
	h := &regHarness{saved: saved, clock: now}
	if h.saved == nil {
		h.saved = map[string]string{}
	}
	h.src = &fakeSource{kind: "deploy", interval: time.Minute, deploys: map[string][]deploy{}}
	h.reg = NewRegistry(RegistryConfig{
		Scope: func() (Scope, error) {
			return Scope{Sessions: []ScopeSession{{ID: "m1", Workgroup: "wg"}, {ID: "m2", Workgroup: "wg"}}}, nil
		},
		Load: func() (map[string]string, error) {
			out := map[string]string{}
			for k, v := range h.saved {
				out[k] = v
			}
			return out, nil
		},
		Save: func(id, doc string) error {
			if doc == "" {
				delete(h.saved, id)
			} else {
				h.saved[id] = doc
			}
			return nil
		},
		Emit: func(c Change) { h.events = append(h.events, c) },
		Legacy: func(doc []byte) (string, []Entry, bool) {
			return "deploy", []Entry{{Key: "legacy", Open: true, Data: json.RawMessage(`{"name":"legacy","state":"active"}`)}}, true
		},
		Logf: func(format string, args ...any) { h.logs = append(h.logs, fmt.Sprintf(format, args...)) },
		Now:  func() time.Time { return h.clock },
	})
	h.reg.Register(h.src)
	return h
}

func (h *regHarness) poll() {
	h.clock = h.clock.Add(time.Minute)
	h.reg.PollKind(context.Background(), "deploy")
}

func ids(list *hp.Enrichments) string {
	if list == nil {
		return ""
	}
	var out []string
	for _, e := range *list {
		out = append(out, e.ID+":"+e.Status.State)
	}
	return strings.Join(out, ",")
}

// A source's entries are rendered with the envelope the registry fills in
// (kind, tone from status, observed_at, stale), persisted per session per
// kind, and survive a restart; transitions are emitted with their kind.
func TestRegistryPersistsRendersAndEmits(t *testing.T) {
	h := newRegHarness(t, nil)
	h.src.deploys["m1"] = []deploy{{Name: "staging", State: hp.StatusActive}}
	h.poll()
	list := h.reg.Enrichments("m1")
	if list == nil || len(*list) != 1 {
		t.Fatalf("enrichments = %+v", list)
	}
	e := (*list)[0]
	if e.Kind != "deploy" || e.Tone != hp.TonePending || e.ObservedAt != now.Format(time.RFC3339) || e.Stale || e.Status.Reasons == nil {
		t.Fatalf("envelope = %+v", e)
	}
	if len(h.events) != 1 || h.events[0].Kind != "deploy" || h.events[0].Key != "staging" {
		t.Fatalf("events = %+v", h.events)
	}
	if !strings.HasPrefix(h.saved["m1"], `{"v":2,"kinds":{"deploy":[`) {
		t.Fatalf("saved = %s", h.saved["m1"])
	}
	if _, ok := h.saved["m2"]; ok {
		t.Fatal("a session with no entries is persisted")
	}

	// An unchanged observation (only observed_at moves) is not re-saved.
	doc := h.saved["m1"]
	h.events = nil
	h.poll()
	if h.saved["m1"] != doc || len(h.events) != 0 {
		t.Fatalf("unchanged poll re-saved or re-emitted: %v", h.events)
	}

	// Restart: state comes back from the store.
	h2 := newRegHarness(t, h.saved)
	if got := ids(h2.reg.Enrichments("m1")); got != "staging:active" {
		t.Fatalf("after restart = %s", got)
	}
}

func TestRegistryLegacyDocument(t *testing.T) {
	h := newRegHarness(t, map[string]string{"m1": `[{"pr":{"repo":"o/r","number":1}}]`})
	if got := ids(h.reg.Enrichments("m1")); got != "legacy:active" {
		t.Fatalf("legacy = %s", got)
	}
}

// Finished entries stay Window after they ended, at most Max of them; open
// entries always stay.
func TestRegistryRetention(t *testing.T) {
	h := newRegHarness(t, nil)
	ended := func(ago time.Duration) string { return now.Add(-ago).Format(time.RFC3339) }
	h.src.deploys["m1"] = []deploy{
		{Name: "live", State: hp.StatusActive},
		{Name: "old", State: hp.StatusDone, Ended: ended(3 * time.Hour)},
		{Name: "a", State: hp.StatusDone, Ended: ended(10 * time.Minute)},
		{Name: "b", State: hp.StatusFailed, Ended: ended(20 * time.Minute)},
		{Name: "c", State: hp.StatusDone, Ended: ended(30 * time.Minute)},
	}
	h.poll()
	if got := ids(h.reg.Enrichments("m1")); got != "live:active,a:done,b:failed" {
		t.Fatalf("retained = %s", got)
	}
	h.clock = now.Add(45 * time.Minute)
	if got := ids(h.reg.Enrichments("m1")); got != "live:active,a:done" {
		t.Fatalf("retained 45 minutes later = %s", got)
	}
}

// Backoff is generic: exponential and capped for transient failures, never
// before a rate limit resets, MaxBackoff during a setup condition (logged
// once), back to the interval on success.
func TestRegistryBackoffAndSetup(t *testing.T) {
	h := newRegHarness(t, nil)
	h.src.err = errFlap
	h.poll()
	h.poll()
	if d := h.reg.Delay("deploy"); d != 4*time.Minute {
		t.Fatalf("delay after 2 transient failures = %v", d)
	}
	for range 10 {
		h.poll()
	}
	if d := h.reg.Delay("deploy"); d != DefaultMaxBackoff {
		t.Fatalf("delay is not capped: %v", d)
	}

	h.src.err, h.src.resetAt = errRate, h.clock.Add(50*time.Minute)
	h.poll()
	if d := h.reg.Delay("deploy"); d < 49*time.Minute {
		t.Fatalf("delay %v ignores the rate-limit reset", d)
	}

	h.logs = nil
	h.src.err = errAuth
	h.poll()
	h.poll()
	if d := h.reg.Delay("deploy"); d != DefaultMaxBackoff {
		t.Fatalf("setup delay = %v", d)
	}
	setupLogs := 0
	for _, l := range h.logs {
		if strings.Contains(l, "unavailable (deploy_auth)") {
			setupLogs++
		}
	}
	if setupLogs != 1 {
		t.Fatalf("setup condition logged %d times: %q", setupLogs, h.logs)
	}

	h.src.err = nil
	h.poll()
	if d := h.reg.Delay("deploy"); d != time.Minute {
		t.Fatalf("delay after recovery = %v", d)
	}
}

// Staleness is generic: a setup condition is stale at once (with the source's
// reason); otherwise a source is stale with no success within twice its
// interval. Open entries inherit it; finished ones never are.
func TestRegistryStaleness(t *testing.T) {
	h := newRegHarness(t, nil)
	h.src.deploys["m1"] = []deploy{{Name: "live", State: hp.StatusActive}, {Name: "done", State: hp.StatusDone}}
	h.poll()
	st := h.reg.Sources()[0]
	if st.Stale || st.Wire() != (hp.EnrichmentSource{Kind: "deploy", ObservedAt: h.clock.Format(time.RFC3339), IntervalS: 60}) {
		t.Fatalf("fresh status = %+v", st)
	}

	h.src.err = errFlap
	h.poll() // one failure inside 2× the interval: not stale yet
	if st := h.reg.Sources()[0]; st.Stale || st.LastReason != hp.StaleError {
		t.Fatalf("after one failure = %+v", st)
	}
	h.poll() // now 2 minutes since the last success
	h.clock = h.clock.Add(time.Second)
	if st := h.reg.Sources()[0]; !st.Stale || st.StaleReason != hp.StaleError {
		t.Fatalf("after 2× interval = %+v", st)
	}
	list := *h.reg.Enrichments("m1")
	for _, e := range list {
		if e.Stale != (e.ID == "live") {
			t.Fatalf("entry %s stale = %v", e.ID, e.Stale)
		}
	}

	h.src.err = errAuth
	h.poll()
	if st := h.reg.Sources()[0]; !st.Stale || st.StaleReason != "deploy_auth" {
		t.Fatalf("setup status = %+v", st)
	}
}

// The fake deploy kind flips the rollup through its normalized status alone:
// a failed deploy is attention ("needs attention · N", since not only PRs
// contribute), an awaiting_merge one waits for merge; it is counted under its
// own kind and in the totals.
func TestRollupGenericKind(t *testing.T) {
	h := newRegHarness(t, nil)
	h.src.deploys["m1"] = []deploy{{Name: "prod", State: hp.StatusFailed}}
	h.src.deploys["m2"] = []deploy{{Name: "staging", State: hp.StatusActive}}
	h.poll()
	members := []Member{
		{ID: "m1", Label: "a", State: hp.StateReady, Enrichments: *h.reg.Enrichments("m1")},
		{ID: "m2", Label: "b", State: hp.StateReady, Enrichments: append(*h.reg.Enrichments("m2"), PullRequest(open(7, approved)))},
	}
	sources := append(h.reg.Sources(), prSource)
	r := Rollup(Input{OwnState: hp.StateReady, Members: members, Sources: sources})
	if r.Rollup.State != hp.RollupPRAttention || r.Rollup.Label != "needs attention · 1" || r.Rollup.Reason != "a deploy deploy prod failed" {
		t.Fatalf("rollup = %+v", r.Rollup)
	}
	if got := r.Rollup.Enrichments.Other["deploy"]; got[hp.StatusFailed] != 1 || got[hp.StatusActive] != 1 {
		t.Fatalf("deploy counts = %v", got)
	}
	if r.Rollup.Totals[hp.StatusFailed] != 1 || r.Rollup.Totals[hp.StatusAwaitingMerge] != 1 || r.Rollup.Totals[hp.StatusActive] != 1 {
		t.Fatalf("totals = %v", r.Rollup.Totals)
	}
	b, _ := json.Marshal(r.Rollup.Enrichments)
	if !strings.Contains(string(b), `"deploy":{`) || !strings.Contains(string(b), `"pull_request":{"open":1`) {
		t.Fatalf("enrichments JSON = %s", b)
	}
	var back hp.RollupEnrichments
	if err := json.Unmarshal(b, &back); err != nil || back.PullRequest == nil || back.Other["deploy"][hp.StatusFailed] != 1 {
		t.Fatalf("round trip = %+v %v", back, err)
	}

	// Fixed: the deploy no longer drives anything; the PR waits for merge.
	h.src.deploys["m1"] = []deploy{{Name: "prod", State: hp.StatusOK}}
	h.poll()
	members[0].Enrichments = *h.reg.Enrichments("m1")
	r = Rollup(Input{OwnState: hp.StateReady, Members: members, Sources: append(h.reg.Sources(), prSource)})
	if r.Rollup.State != hp.RollupWaitingMerge || r.Rollup.Label != "waiting for merge · 1 PR" {
		t.Fatalf("rollup after fix = %+v", r.Rollup)
	}
	// An awaiting_merge deploy joins the merge tier with a generic label.
	h.src.deploys["m1"] = []deploy{{Name: "prod", State: hp.StatusAwaitingMerge}}
	h.poll()
	members[0].Enrichments = *h.reg.Enrichments("m1")
	r = Rollup(Input{OwnState: hp.StateReady, Members: members, Sources: append(h.reg.Sources(), prSource)})
	if r.Rollup.State != hp.RollupWaitingMerge || r.Rollup.Label != "waiting for merge · 2" {
		t.Fatalf("rollup with a deploy awaiting merge = %+v", r.Rollup)
	}
}
