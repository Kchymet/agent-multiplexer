package direct

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"amux/internal/core"
	"amux/internal/runtimeevents"
	amuxstore "amux/internal/store"
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// wakeBackend is fakeBackend with scripted per-source event logs, read the
// way runtimeevents.ReadSources reads them: each source by its own 1-based
// index.
type wakeBackend struct {
	*fakeBackend
	evMu sync.Mutex
	logs map[string]map[string][]hp.RuntimeEvent // sid -> source kind -> events
	gens map[string]string                       // sid+"/"+kind -> generation
	// identFail makes SessionIdentities fail; srcFail makes SourceEvents fail.
	identFail bool
	srcFail   int
	// missing hides a source (sid+"/"+kind): it reads as absent, head 0 and
	// no generation, as runtimeevents reports a file that does not exist.
	missing map[string]bool
	// onSourceRead, when set, runs inside SourceEvents (after the fetch-time
	// visibility check): it simulates a replacement or an id reuse racing the
	// event lookup.
	onSourceRead func()
}

func newWakeBackend(rows ...core.Session) *wakeBackend {
	return &wakeBackend{fakeBackend: newFake(rows...), logs: map[string]map[string][]hp.RuntimeEvent{}, gens: map[string]string{}, missing: map[string]bool{}}
}

func (b *wakeBackend) SessionIdentities(ctx context.Context, ids []string) (map[string]amuxstore.SessionIdentity, error) {
	b.evMu.Lock()
	fail := b.identFail
	b.evMu.Unlock()
	if fail {
		return nil, errors.New("daemon busy")
	}
	return b.fakeBackend.SessionIdentities(ctx, ids)
}

// add appends ev to sid's transcript source; index must be the next one.
func (b *wakeBackend) add(sid string, index int64, ev hp.RuntimeEvent) {
	b.addSrc(sid, "claude", index, ev)
}

func (b *wakeBackend) addSrc(sid, kind string, index int64, ev hp.RuntimeEvent) {
	b.evMu.Lock()
	defer b.evMu.Unlock()
	if b.logs[sid] == nil {
		b.logs[sid] = map[string][]hp.RuntimeEvent{}
	}
	if want := int64(len(b.logs[sid][kind])) + 1; index != want {
		panic(fmt.Sprintf("non-dense index %d for %s/%s (want %d)", index, sid, kind, want))
	}
	b.logs[sid][kind] = append(b.logs[sid][kind], ev)
}

// copySrc replaces a source with a new file (new inode) that copies the same
// transcript (same first line), as restore-before-resume writes it.
func (b *wakeBackend) copySrc(sid, kind string, evs ...hp.RuntimeEvent) {
	b.evMu.Lock()
	defer b.evMu.Unlock()
	b.logs[sid][kind] = evs
	b.gens[sid+"/"+kind] += "~"
}

// replaceSrc replaces a source with a NEW file (new generation).
func (b *wakeBackend) replaceSrc(sid, kind string, evs ...hp.RuntimeEvent) {
	b.evMu.Lock()
	defer b.evMu.Unlock()
	b.logs[sid][kind] = evs
	b.gens[sid+"/"+kind] += "'"
}

func (b *wakeBackend) setMissing(sid, kind string, missing bool) {
	b.evMu.Lock()
	defer b.evMu.Unlock()
	b.missing[sid+"/"+kind] = missing
}

// truncateSrc shortens a source in place (same generation).
func (b *wakeBackend) truncateSrc(sid, kind string, n int) {
	b.evMu.Lock()
	defer b.evMu.Unlock()
	b.logs[sid][kind] = b.logs[sid][kind][:n]
}

func (b *wakeBackend) SourceEvents(ctx context.Context, sid string, after map[string]int64, limit int) (runtimeevents.SourcesPage, bool) {
	b.evMu.Lock()
	defer b.evMu.Unlock()
	if b.srcFail > 0 {
		b.srcFail--
		return runtimeevents.SourcesPage{}, false
	}
	if hook := b.onSourceRead; hook != nil {
		b.onSourceRead = nil
		b.evMu.Unlock()
		hook()
		b.evMu.Lock()
	}
	var page runtimeevents.SourcesPage
	kinds := make([]string, 0, len(b.logs[sid]))
	for k := range b.logs[sid] {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		evs := b.logs[sid][k]
		// Generation "<file id>.<first line>": replaceSrc changes both (a new
		// transcript), copySrc only the file id (a copy of the same one).
		g := b.gens[sid+"/"+k]
		fileID, line := "f-"+sid+"-"+k+g, "l-"+sid+"-"+k+strings.ReplaceAll(g, "~", "")
		src := runtimeevents.SourceEvents{Kind: k, Head: int64(len(evs)), Generation: fileID + "." + line}
		if b.missing[sid+"/"+k] {
			page.Sources = append(page.Sources, runtimeevents.SourceEvents{Kind: k})
			continue
		}
		from := max(after[k], 0)
		if from < src.Head {
			to := min(src.Head, from+int64(limit))
			src.First, src.Events = from+1, append([]hp.RuntimeEvent(nil), evs[from:to]...)
		}
		page.Sources = append(page.Sources, src)
	}
	return page, true
}

func permEv(item, req, gen string) hp.RuntimeEvent {
	p, _ := json.Marshal(map[string]string{"request_id": req, "runtime_generation": gen, "action": "rm -rf secret"})
	return hp.RuntimeEvent{Type: hp.TypePermissionRequest, ItemID: item, Payload: p}
}

func resolvedEv(req, gen string) hp.RuntimeEvent {
	p, _ := json.Marshal(map[string]string{"request_id": req, "runtime_generation": gen, "decision": "allow"})
	return hp.RuntimeEvent{Type: hp.TypePermissionResolved, Payload: p}
}

func turnEndEv() hp.RuntimeEvent {
	return hp.RuntimeEvent{Type: hp.TypeTurnEnd, Payload: json.RawMessage(`{"stop_reason":"end_turn"}`)}
}

func textEv() hp.RuntimeEvent { return hp.RuntimeEvent{Type: hp.TypeText} }

func testRouteID(n byte) string {
	b := make([]byte, 32)
	b[0] = n
	return hp.DirectWakeRoutePrefix + base64.RawURLEncoding.EncodeToString(b)
}

func testSecretN(n byte) string {
	b := make([]byte, 32)
	b[31] = n
	return base64.RawURLEncoding.EncodeToString(b)
}

type wakeHarness struct {
	t    *testing.T
	root string
	clk  *clock
	a    *Authority
	b    *wakeBackend
	ws   *WakeService
	srv  *Server
}

func newWakeHarness(t *testing.T, rows ...core.Session) *wakeHarness {
	t.Helper()
	if len(rows) == 0 {
		rows = testRows()
	}
	h := &wakeHarness{t: t, root: t.TempDir(), clk: &clock{t: time.Unix(1_800_000_000, 0)}}
	h.a = openTestAuthority(t, h.root, Lifetimes{}, h.clk.now)
	if _, err := h.a.EnableWake("usr_owner", "prov-1"); err != nil {
		t.Fatal(err)
	}
	h.b = newWakeBackend(rows...)
	h.ws = NewWakeService(h.a, h.root, h.b, nil)
	srv, err := NewServer(Config{Authority: h.a, Backend: h.b, Endpoints: []string{testEndpoint}, AllowOrigins: []string{testOrigin},
		WatchPoll: 20 * time.Millisecond, Revalidate: time.Hour, Keepalive: time.Hour, Now: h.clk.now, Wake: h.ws})
	if err != nil {
		t.Fatal(err)
	}
	h.srv = srv
	return h
}

func (h *wakeHarness) post(path, token string, body any) (int, map[string]any) {
	h.t.Helper()
	raw, _ := json.Marshal(body)
	w := do(h.srv, http.MethodPost, path, token, string(raw))
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func (h *wakeHarness) subscribe(token, route, secret string) map[string]any {
	h.t.Helper()
	code, out := h.post(PathWakeSubscribe, token, WakeSubscribeRequest{RouteID: route, ActivationSecret: secret})
	if code != 200 {
		h.t.Fatalf("subscribe: %d %v", code, out)
	}
	return out
}

// activate leases the activation item for route and settles it with status.
func (h *wakeHarness) activate(route, status string) {
	h.t.Helper()
	items := h.ws.Lease(context.Background(), 16, 0)
	for _, it := range items {
		if it.Kind == "activate" && it.RouteID == route {
			if it.Secret == "" || it.GrantRef == "" {
				h.t.Fatalf("activate item missing proof: %+v", it)
			}
			h.ws.Settle([]WakeSettleResult{{ID: it.ID, Status: status}})
			return
		}
	}
	h.t.Fatalf("no activate item for %s in %+v", route, items)
}

func (h *wakeHarness) fetch(token, route string, req WakeFetchRequest) WakeFetchResponse {
	h.t.Helper()
	req.RouteID = route
	raw, _ := json.Marshal(req)
	w := do(h.srv, http.MethodPost, PathWakeEvents, token, string(raw))
	if w.Code != 200 {
		h.t.Fatalf("fetch: %d %s", w.Code, w.Body)
	}
	var out WakeFetchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		h.t.Fatal(err)
	}
	return out
}

func (h *wakeHarness) ack(token, route string, items ...WakeAckItem) int {
	h.t.Helper()
	code, out := h.post(PathWakeAck, token, WakeAckRequest{RouteID: route, Items: items})
	if code != 200 {
		h.t.Fatalf("ack: %d %v", code, out)
	}
	return int(out["acked"].(float64))
}

func (h *wakeHarness) tick() { h.ws.Tick(context.Background()) }

func TestWakePermissionFlowAndVersionedAck(t *testing.T) {
	h := newWakeHarness(t)
	tok := enroll(t, h.a, []string{PermRead, PermApprove}, allSessions)
	route := testRouteID(1)
	h.b.add("a1", 1, textEv())
	h.subscribe(tok.AccessToken, route, testSecretN(1))
	h.tick() // baseline at head 1
	// Wakes wait for activation: an event before activation is recorded but
	// its wake item is not leasable yet.
	h.b.add("a1", 2, permEv("it-1", "req-1", "gen-1"))
	h.tick()
	for _, it := range h.ws.Lease(context.Background(), 16, 0) {
		if it.Kind == "wake" {
			t.Fatalf("wake leased before activation: %+v", it)
		}
		h.ws.Settle([]WakeSettleResult{{ID: it.ID, Status: hp.DirectWakeUnavailable}})
	}
	h.clk.add(wakeBackoffMax) // past the activation's retry backoff
	h.activate(route, hp.DirectWakeAccepted)
	items := h.ws.Lease(context.Background(), 16, 0)
	if len(items) != 1 || items[0].Kind != "wake" || items[0].Secret != "" {
		t.Fatalf("lease after activation = %+v", items)
	}
	recordWake := items[0].WakeID
	h.ws.Settle([]WakeSettleResult{{ID: items[0].ID, Status: hp.DirectWakeAccepted}})

	f := h.fetch(tok.AccessToken, route, WakeFetchRequest{})
	if len(f.Items) != 1 {
		t.Fatalf("fetch = %+v", f)
	}
	it := f.Items[0]
	if it.WakeID != recordWake || it.State != WakeStateOpen || it.RequestID != "req-1" || it.RuntimeGeneration != "gen-1" ||
		it.ItemID != "it-1" || it.SessionID != "a1" || it.Incarnation != "inc-a1" || it.Index != 2 {
		t.Fatalf("item = %+v", it)
	}
	// Acked open stays pending (in snapshots).
	if n := h.ack(tok.AccessToken, route, WakeAckItem{WakeID: it.WakeID, Seq: it.Seq}); n != 1 {
		t.Fatalf("acked %d", n)
	}
	snap := h.fetch(tok.AccessToken, route, WakeFetchRequest{Snapshot: true})
	if len(snap.Items) != 1 || !snap.Items[0].Acked || snap.Complete == nil || !*snap.Complete {
		t.Fatalf("snapshot after ack = %+v", snap)
	}
	// Resolution: same record wake id, new seq, unacked, minimal; and a fresh
	// delivery wake id so Harness's accepted-delivery dedupe does not swallow it.
	h.b.add("a1", 3, resolvedEv("req-1", "gen-1"))
	h.tick()
	f2 := h.fetch(tok.AccessToken, route, WakeFetchRequest{AfterSeq: f.NextSeq})
	if len(f2.Items) != 1 {
		t.Fatalf("after resolution = %+v", f2)
	}
	r := f2.Items[0]
	if r.WakeID != it.WakeID || r.State != WakeStateResolved || r.Seq <= it.Seq || r.Acked || r.RequestID != "" || r.SessionID != "" {
		t.Fatalf("clearing revision = %+v", r)
	}
	deliveries := h.ws.Lease(context.Background(), 16, 0)
	if len(deliveries) != 1 || deliveries[0].WakeID == recordWake {
		t.Fatalf("resolution delivery must use a fresh wake id: %+v (record %s)", deliveries, recordWake)
	}
	h.ws.Settle([]WakeSettleResult{{ID: deliveries[0].ID, Status: hp.DirectWakeAccepted}})
	// An old ack for the earlier revision is ignored: the resolution survives.
	if n := h.ack(tok.AccessToken, route, WakeAckItem{WakeID: it.WakeID, Seq: it.Seq}); n != 0 {
		t.Fatalf("stale ack counted %d", n)
	}
	if again := h.fetch(tok.AccessToken, route, WakeFetchRequest{AfterSeq: f.NextSeq}); len(again.Items) != 1 {
		t.Fatalf("stale ack removed the resolution: %+v", again)
	}
	if n := h.ack(tok.AccessToken, route, WakeAckItem{WakeID: r.WakeID, Seq: r.Seq}); n != 1 {
		t.Fatalf("current ack counted %d", n)
	}
	if gone := h.fetch(tok.AccessToken, route, WakeFetchRequest{}); len(gone.Items) != 0 {
		t.Fatalf("acked resolution still listed: %+v", gone)
	}
	// Re-evaluation cannot recreate it (the mark is past it).
	h.tick()
	if n := len(h.fetch(tok.AccessToken, route, WakeFetchRequest{}).Items); n != 0 {
		t.Fatalf("recreated %d records", n)
	}
}

func TestWakeNewSubscriptionDoesNotSkipBacklog(t *testing.T) {
	h := newWakeHarness(t)
	a := enroll(t, h.a, []string{PermRead}, allSessions)
	b := enroll(t, h.a, []string{PermRead}, allSessions)
	ra, rb := testRouteID(1), testRouteID(2)
	h.subscribe(a.AccessToken, ra, testSecretN(1))
	h.tick() // A baselined at 0
	h.b.add("a1", 1, permEv("i1", "r1", "g1"))
	h.b.add("a1", 2, permEv("i2", "r2", "g1"))
	// B arrives while A's backlog is unread.
	h.subscribe(b.AccessToken, rb, testSecretN(2))
	h.tick()
	if n := len(h.fetch(a.AccessToken, ra, WakeFetchRequest{}).Items); n != 2 {
		t.Fatalf("old subscription got %d of its backlog, want 2", n)
	}
	if n := len(h.fetch(b.AccessToken, rb, WakeFetchRequest{}).Items); n != 0 {
		t.Fatalf("new subscription got %d events from before its baseline", n)
	}
	h.b.add("a1", 3, permEv("i3", "r3", "g1"))
	h.tick()
	if n := len(h.fetch(b.AccessToken, rb, WakeFetchRequest{}).Items); n != 1 {
		t.Fatalf("new subscription got %d new events, want 1", n)
	}
}

// An identity lookup failure defers the session; it is never read with an
// empty incarnation (which would lose a watched turn end).
func TestWakeIdentityFailureDefersRead(t *testing.T) {
	h := newWakeHarness(t)
	tok := enroll(t, h.a, []string{PermRead}, allSessions)
	route := testRouteID(1)
	h.subscribe(tok.AccessToken, route, testSecretN(1))
	h.tick()
	if code, out := h.post(PathWakeWatch, tok.AccessToken, WakeWatchRequest{RouteID: route, SessionID: "a1", Incarnation: "inc-a1", Watch: true}); code != 200 {
		t.Fatalf("watch %d %v", code, out)
	}
	h.b.add("a1", 1, turnEndEv())
	h.b.identFail = true
	h.tick()
	h.tick()
	h.b.identFail = false
	h.tick()
	f := h.fetch(tok.AccessToken, route, WakeFetchRequest{})
	if len(f.Items) != 1 || f.Items[0].Kind != WakeKindTurnEnd || f.Items[0].Incarnation != "inc-a1" {
		t.Fatalf("watched turn end lost across an identity failure: %+v", f)
	}
}

func TestWakeActivationRetryBacksOff(t *testing.T) {
	h := newWakeHarness(t)
	tok := enroll(t, h.a, []string{PermRead}, allSessions)
	route := testRouteID(1)
	h.subscribe(tok.AccessToken, route, testSecretN(1))
	items := h.ws.Lease(context.Background(), 16, 0)
	if len(items) != 1 || items[0].Kind != "activate" {
		t.Fatalf("lease %+v", items)
	}
	h.ws.Settle([]WakeSettleResult{{ID: items[0].ID, Status: hp.DirectWakeUnavailable}})
	if again := h.ws.Lease(context.Background(), 16, 0); len(again) != 0 {
		t.Fatalf("activation re-leased without backoff: %+v", again)
	}
	h.clk.add(wakeBackoffMin + time.Millisecond)
	if again := h.ws.Lease(context.Background(), 16, 0); len(again) != 1 {
		t.Fatalf("activation not retried after backoff: %+v", again)
	}
}

// Opening the authority never writes wake cleanup (so a failing disk cannot
// keep direct from starting); the expired secret is unusable at once and
// leaves the file at the next real authority commit, whose failure semantics
// are the existing ones.
func TestWakeExpiredSecretErasedByNextRealCommitOnly(t *testing.T) {
	root := t.TempDir()
	clk := &clock{t: time.Unix(1_800_000_000, 0)}
	a, _ := OpenAuthority(root, Lifetimes{}, clk.now)
	_, _ = a.EnableWake("usr", "prov")
	tok := enroll(t, a, []string{PermRead}, allSessions)
	l, _ := a.Admit(context.Background(), tok.AccessToken, PermRead)
	_, _ = a.WakeSubscribe(l, testRouteID(1), testSecretN(1))
	l.Done()
	a.Close()
	clk.add(WakeActivationTTL + time.Minute)
	life := Lifetimes{}
	life, _ = life.resolve()
	onDisk := func() bool {
		raw, _ := os.ReadFile(filepath.Join(root, grantsFile))
		return strings.Contains(string(raw), testSecretN(1))
	}

	// 1. Startup with a failing store: opens, writes nothing, secret unusable.
	st, _ := openStore(root)
	writes := 0
	st.write = func(string, string, []byte) error { writes++; return fmt.Errorf("%w: disk", errNotCommitted) }
	a2, err := loadAuthority(st, life, clk.now)
	if err != nil {
		t.Fatalf("authority open must not depend on wake cleanup: %v", err)
	}
	if writes != 0 {
		t.Fatalf("startup wrote %d times", writes)
	}
	if subs := a2.wakeAuthSnapshot().Subs; len(subs) != 0 {
		t.Fatalf("expired activation still vouched for: %+v", subs)
	}
	if !onDisk() {
		t.Fatal("test setup: secret should still be on disk")
	}

	// 2. An ambiguous real commit keeps the existing fail-closed behavior.
	st.write = func(string, string, []byte) error { return fmt.Errorf("%w: dir sync", errAmbiguousCommit) }
	if _, _, err := a2.NewCode("x", []string{PermRead}, allSessions, 0); err == nil {
		t.Fatal("ambiguous commit reported success")
	}
	if a2.degraded == nil {
		t.Fatal("an ambiguous real commit must degrade the authority as before")
	}
	a2.Close()

	// 3. A later successful real commit persists the erasure.
	a3, err := OpenAuthority(root, Lifetimes{}, clk.now)
	if err != nil {
		t.Fatal(err)
	}
	defer a3.Close()
	if _, _, err := a3.NewCode("y", []string{PermRead}, allSessions, 0); err != nil {
		t.Fatal(err)
	}
	if onDisk() {
		t.Fatal("expired secret still on disk after a real commit")
	}
}

func TestWakeHostCapIgnoresExpiredActivations(t *testing.T) {
	saved := maxWakeSubsPerHost
	maxWakeSubsPerHost = 1
	defer func() { maxWakeSubsPerHost = saved }()
	h := newWakeHarness(t)
	a := enroll(t, h.a, []string{PermRead}, allSessions)
	h.subscribe(a.AccessToken, testRouteID(1), testSecretN(1))
	h.clk.add(WakeActivationTTL + time.Second)
	b := enroll(t, h.a, []string{PermRead}, allSessions)
	h.subscribe(b.AccessToken, testRouteID(2), testSecretN(2))
}

func TestWakeAckDropsPendingResolutionDelivery(t *testing.T) {
	h := newWakeHarness(t)
	tok := enroll(t, h.a, []string{PermRead}, allSessions)
	route := testRouteID(1)
	h.subscribe(tok.AccessToken, route, testSecretN(1))
	h.tick()
	h.activate(route, hp.DirectWakeAccepted)
	h.b.add("a1", 1, permEv("i", "r", "g"))
	h.tick()
	items := h.ws.Lease(context.Background(), 16, 0)
	h.ws.Settle([]WakeSettleResult{{ID: items[0].ID, Status: hp.DirectWakeAccepted}})
	h.b.add("a1", 2, resolvedEv("r", "g"))
	h.tick()
	f := h.fetch(tok.AccessToken, route, WakeFetchRequest{})
	h.ack(tok.AccessToken, route, WakeAckItem{WakeID: f.Items[0].WakeID, Seq: f.Items[0].Seq})
	if again := h.ws.Lease(context.Background(), 16, 0); len(again) != 0 {
		t.Fatalf("resolution push still queued after the phone cleared the card: %+v", again)
	}
}

func TestWakeChangingSessionsDoNotStarveRoundRobin(t *testing.T) {
	var rows []core.Session
	for i := 0; i < 12; i++ {
		rows = append(rows, core.Session{ID: fmt.Sprintf("s%02d", i), Title: "x", Runtime: "claude"})
	}
	h := newWakeHarness(t, rows...)
	h.ws.perTick = 4
	tok := enroll(t, h.a, []string{PermRead}, allSessions)
	route := testRouteID(1)
	h.subscribe(tok.AccessToken, route, testSecretN(1))
	h.tick()
	h.b.add("s11", 1, permEv("i", "r", "g"))
	for i := 0; i < 8; i++ {
		// Sessions s00..s05 change state on every tick.
		for j := 0; j < 6; j++ {
			rows[j].State = fmt.Sprintf("st%d", i%2)
		}
		h.b.setRows(rows...)
		h.tick()
	}
	if n := len(h.fetch(tok.AccessToken, route, WakeFetchRequest{}).Items); n != 1 {
		t.Fatalf("round-robin session starved by changing sessions (%d records)", n)
	}
}

func TestWakeTurnEndOnlyForWatchedInstance(t *testing.T) {
	h := newWakeHarness(t)
	tok := enroll(t, h.a, []string{PermRead}, allSessions)
	route := testRouteID(1)
	h.subscribe(tok.AccessToken, route, testSecretN(1))
	h.tick()
	h.b.add("a1", 1, turnEndEv())
	h.tick()
	if n := len(h.fetch(tok.AccessToken, route, WakeFetchRequest{}).Items); n != 0 {
		t.Fatalf("unwatched turn end recorded")
	}
	code, out := h.post(PathWakeWatch, tok.AccessToken, WakeWatchRequest{RouteID: route, SessionID: "a1", Incarnation: "inc-old", Watch: true})
	if code != http.StatusConflict || out["code"] != CodeWakeIncarnation || out["incarnation"] != "inc-a1" {
		t.Fatalf("stale incarnation watch = %d %v", code, out)
	}
	code, out = h.post(PathWakeWatch, tok.AccessToken, WakeWatchRequest{RouteID: route, SessionID: "a1", Incarnation: "inc-a1", Watch: true})
	if code != 200 || out["watched"] != true || out["incarnation"] != "inc-a1" {
		t.Fatalf("watch = %d %v", code, out)
	}
	h.b.add("a1", 2, turnEndEv())
	h.tick()
	f := h.fetch(tok.AccessToken, route, WakeFetchRequest{})
	if len(f.Items) != 1 || f.Items[0].Kind != WakeKindTurnEnd || f.Items[0].Index != 2 {
		t.Fatalf("watched turn end = %+v", f)
	}
	// A new instance under the same id drops the watch.
	h.b.setInc("a1", "inc-new")
	h.b.add("a1", 3, turnEndEv())
	h.tick()
	h.tick()
	f = h.fetch(tok.AccessToken, route, WakeFetchRequest{})
	for _, it := range f.Items {
		if it.Kind == WakeKindTurnEnd && it.State == WakeStateDone && it.Index == 3 {
			t.Fatalf("watch survived a new incarnation: %+v", f)
		}
	}
}

func TestWakePostedCardClearsOnIncarnationAndScopeChange(t *testing.T) {
	rows := testRows()
	h := newWakeHarness(t, rows...)
	tok := enroll(t, h.a, []string{PermRead}, ResourceScope{Kind: ResourceWorkgroups, Workgroups: []string{"wg1"}})
	route := testRouteID(1)
	h.subscribe(tok.AccessToken, route, testSecretN(1))
	h.tick()
	h.b.add("a1", 1, permEv("i1", "r1", "g1"))
	h.b.add("wg1", 1, permEv("i2", "r2", "g1"))
	h.tick()
	first := h.fetch(tok.AccessToken, route, WakeFetchRequest{})
	if len(first.Items) != 2 {
		t.Fatalf("records = %+v", first)
	}
	// a1 leaves the workgroup (scope change); wg1 gets a new instance.
	rows[1].RootID = "wg2"
	h.b.setRows(rows...)
	h.b.setInc("wg1", "inc-wg1-new")
	f := h.fetch(tok.AccessToken, route, WakeFetchRequest{AfterSeq: first.NextSeq})
	got := map[string]string{}
	for _, it := range f.Items {
		if it.SessionID != "" || it.RequestID != "" {
			t.Fatalf("clearing record carries session metadata: %+v", it)
		}
		got[it.WakeID] = it.State
	}
	for _, it := range first.Items {
		want := WakeStateWithdrawn
		if it.SessionID == "wg1" {
			want = WakeStateStale
		}
		if got[it.WakeID] != want {
			t.Fatalf("card %s (%s) = %q, want %q; fetch %+v", it.WakeID, it.SessionID, got[it.WakeID], want, f)
		}
	}
}

func TestWakeSnapshotHighWaterDoesNotSkipAppends(t *testing.T) {
	h := newWakeHarness(t)
	tok := enroll(t, h.a, []string{PermRead}, allSessions)
	route := testRouteID(1)
	h.subscribe(tok.AccessToken, route, testSecretN(1))
	h.tick()
	for i := 1; i <= 3; i++ {
		h.b.add("a1", int64(i), permEv(fmt.Sprint("i", i), fmt.Sprint("r", i), "g"))
	}
	h.tick()
	p1 := h.fetch(tok.AccessToken, route, WakeFetchRequest{Snapshot: true, Limit: 2})
	if len(p1.Items) != 2 || !p1.HasMore || p1.SnapshotHW == 0 {
		t.Fatalf("page 1 = %+v", p1)
	}
	// Appended and resolved during paging.
	h.b.add("a1", 4, permEv("i4", "r4", "g"))
	h.b.add("a1", 5, resolvedEv("r1", "g"))
	h.tick()
	p2 := h.fetch(tok.AccessToken, route, WakeFetchRequest{Snapshot: true, SnapshotHW: p1.SnapshotHW, AfterSeq: p1.NextSeq, Limit: 2})
	if p2.HasMore || p2.NextSeq != p1.SnapshotHW {
		t.Fatalf("final page next_seq = %d, want snapshot_hw %d (%+v)", p2.NextSeq, p1.SnapshotHW, p2)
	}
	for _, it := range p2.Items {
		if it.Seq > p1.SnapshotHW {
			t.Fatalf("snapshot returned a record past its high-water mark: %+v", it)
		}
	}
	inc := h.fetch(tok.AccessToken, route, WakeFetchRequest{AfterSeq: p2.NextSeq})
	var sawNew, sawResolved bool
	for _, it := range inc.Items {
		sawNew = sawNew || it.State == WakeStateOpen && it.RequestID == "r4"
		sawResolved = sawResolved || it.State == WakeStateResolved
	}
	if !sawNew || !sawResolved {
		t.Fatalf("appends during paging skipped: %+v", inc)
	}
}

func TestWakeRevocationFencesEveryOperation(t *testing.T) {
	h := newWakeHarness(t)
	tok := enroll(t, h.a, []string{PermRead}, allSessions)
	route := testRouteID(1)
	h.subscribe(tok.AccessToken, route, testSecretN(1))
	h.tick()
	h.activate(route, hp.DirectWakeAccepted)
	h.b.add("a1", 1, permEv("i", "r", "g"))
	h.tick()
	if err := h.a.Revoke(tok.ClientID, "test"); err != nil {
		t.Fatal(err)
	}
	if items := h.ws.Lease(context.Background(), 16, 0); len(items) != 0 {
		t.Fatalf("leased after revoke: %+v", items)
	}
	if routes := h.ws.Routes(); len(routes) != 0 {
		t.Fatalf("route sync lists a revoked grant: %+v", routes)
	}
	raw, _ := json.Marshal(WakeFetchRequest{RouteID: route})
	if w := do(h.srv, http.MethodPost, PathWakeEvents, tok.AccessToken, string(raw)); w.Code != http.StatusUnauthorized {
		t.Fatalf("fetch after revoke = %d", w.Code)
	}
	h.tick()
	if _, ok := h.ws.store.data.Subs[subKey(tok.ClientID, route, h.a.wakeAuthSnapshot().Nonce)]; ok {
		t.Fatal("revoked grant's wake data was not reconciled away")
	}
}

func TestWakeDisableAndRepinMakeOldDataInert(t *testing.T) {
	h := newWakeHarness(t)
	tok := enroll(t, h.a, []string{PermRead}, allSessions)
	route := testRouteID(1)
	h.subscribe(tok.AccessToken, route, testSecretN(1))
	h.tick()
	old := h.a.wakeAuthSnapshot().Nonce
	if _, err := h.a.EnableWake("usr_owner", "prov-2"); err != nil { // re-pin
		t.Fatal(err)
	}
	if nonce := h.a.wakeAuthSnapshot().Nonce; nonce == old {
		t.Fatal("re-pin kept the nonce")
	}
	if subs := h.a.wakeAuthSnapshot().Subs; len(subs) != 0 {
		t.Fatalf("re-pin kept subscriptions: %+v", subs)
	}
	if _, err := h.a.DisableWake("test"); err != nil {
		t.Fatal(err)
	}
	if h.a.wakeAuthSnapshot() != nil || len(h.ws.Routes()) != 0 {
		t.Fatal("disabled wake still vouches")
	}
	code, out := h.post(PathWakeSubscribe, tok.AccessToken, WakeSubscribeRequest{RouteID: route, ActivationSecret: testSecretN(1)})
	if code != http.StatusConflict || out["code"] != CodeWakeDisabled {
		t.Fatalf("subscribe while disabled = %d %v", code, out)
	}
}

// A failed or ambiguous wake.json write makes wake-ups degraded and nothing
// else: token refresh and direct reads keep working.
func TestWakeStoreFailureNeverDegradesDirect(t *testing.T) {
	h := newWakeHarness(t)
	tok := enroll(t, h.a, []string{PermRead}, allSessions)
	route := testRouteID(1)
	h.subscribe(tok.AccessToken, route, testSecretN(1))
	h.ws.store.write = func(string, string, []byte) error { return fmt.Errorf("%w: disk", errAmbiguousCommit) }
	h.b.add("a1", 1, permEv("i", "r", "g")) // new wake data to write
	h.tick()
	_ = h.ws.store.flush(true)
	if deg, _ := h.ws.Degraded(); !deg {
		t.Fatal("ambiguous wake write did not degrade wake-ups")
	}
	if w := do(h.srv, http.MethodGet, PathSession, tok.AccessToken, ""); w.Code != 200 {
		t.Fatalf("direct sessions after wake failure = %d", w.Code)
	}
	if _, err := h.a.Refresh(tok.RefreshToken); err != nil {
		t.Fatalf("token refresh after wake failure: %v", err)
	}
	raw, _ := json.Marshal(WakeFetchRequest{RouteID: route})
	if w := do(h.srv, http.MethodPost, PathWakeEvents, tok.AccessToken, string(raw)); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("wake fetch while degraded = %d", w.Code)
	}
	// Capabilities say so.
	w := do(h.srv, http.MethodGet, PathCapabilities, tok.AccessToken, "")
	var caps CapabilitiesResponse
	_ = json.Unmarshal(w.Body.Bytes(), &caps)
	if caps.Wake == nil || !caps.Wake.Degraded || !caps.Wake.Enabled {
		t.Fatalf("capabilities wake = %+v", caps.Wake)
	}
}

// An older binary that rewrites grants.json drops the wake fields; the new
// binary then reopens with wake off and wake.json inert, and re-enabling gets
// a fresh nonce no old record can match.
func TestWakeOldBinaryRewriteLeavesWakeDataInert(t *testing.T) {
	root := t.TempDir()
	clk := &clock{t: time.Unix(1_800_000_000, 0)}
	a, err := OpenAuthority(root, Lifetimes{}, clk.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.EnableWake("usr", "prov"); err != nil {
		t.Fatal(err)
	}
	b := newWakeBackend(testRows()...)
	ws := NewWakeService(a, root, b, nil)
	tok := enroll(t, a, []string{PermRead}, allSessions)
	l, _ := a.Admit(context.Background(), tok.AccessToken, PermRead)
	if _, err := a.WakeSubscribe(l, testRouteID(1), testSecretN(1)); err != nil {
		t.Fatal(err)
	}
	l.Done()
	ws.Tick(context.Background())
	b.add("a1", 1, permEv("i", "r", "g"))
	ws.Tick(context.Background())
	_ = ws.store.flush(true)
	oldNonce := a.wakeAuthSnapshot().Nonce
	a.Close()
	// The older binary: lenient decode, rewrite without unknown fields.
	path := filepath.Join(root, grantsFile)
	raw, _ := os.ReadFile(path)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	delete(m, "wake")
	for _, g := range m["grants"].(map[string]any) {
		delete(g.(map[string]any), "wakeSubs")
	}
	raw, _ = json.Marshal(m)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	a2, err := OpenAuthority(root, Lifetimes{}, clk.now)
	if err != nil {
		t.Fatal(err)
	}
	defer a2.Close()
	if a2.WakeStatus().Enabled || a2.wakeAuthSnapshot() != nil {
		t.Fatal("wake survived an old-binary rewrite")
	}
	ws2 := NewWakeService(a2, root, b, nil)
	if len(ws2.store.data.Subs) != 0 {
		t.Fatalf("wake.json not inert after rewrite: %d subs", len(ws2.store.data.Subs))
	}
	if _, err := a2.EnableWake("usr", "prov"); err != nil {
		t.Fatal(err)
	}
	if a2.wakeAuthSnapshot().Nonce == oldNonce {
		t.Fatal("re-enable reused the old nonce")
	}
}

// A revoke commits in grants.json; the wake.json cleanup that follows can fail
// or never happen (crash) without anything of that grant becoming reachable.
func TestWakeRevokeThenCleanupCrashIsReconciled(t *testing.T) {
	root := t.TempDir()
	clk := &clock{t: time.Unix(1_800_000_000, 0)}
	a, _ := OpenAuthority(root, Lifetimes{}, clk.now)
	_, _ = a.EnableWake("usr", "prov")
	b := newWakeBackend(testRows()...)
	ws := NewWakeService(a, root, b, nil)
	tok := enroll(t, a, []string{PermRead}, allSessions)
	l, _ := a.Admit(context.Background(), tok.AccessToken, PermRead)
	_, _ = a.WakeSubscribe(l, testRouteID(1), testSecretN(1))
	l.Done()
	ws.Tick(context.Background())
	b.add("a1", 1, permEv("i", "r", "g"))
	ws.Tick(context.Background())
	_ = ws.store.flush(true)
	// Crash right after the revoke commit: wake.json still holds the data.
	ws.store.write = func(string, string, []byte) error { return fmt.Errorf("%w: crash", errNotCommitted) }
	if err := a.Revoke(tok.ClientID, "test"); err != nil {
		t.Fatal(err)
	}
	if items := ws.Lease(context.Background(), 16, 0); len(items) != 0 {
		t.Fatalf("lease after revoke with failing cleanup: %+v", items)
	}
	a.Close()
	a2, _ := OpenAuthority(root, Lifetimes{}, clk.now)
	defer a2.Close()
	ws2 := NewWakeService(a2, root, b, nil)
	if len(ws2.store.data.Subs) != 0 || len(ws2.Routes()) != 0 {
		t.Fatalf("restart did not reconcile revoked grant's wake data: subs=%d routes=%v", len(ws2.store.data.Subs), ws2.Routes())
	}
}

func TestWakeActivationSecretErasedAndRouteGoneFailsSubscription(t *testing.T) {
	h := newWakeHarness(t)
	tok := enroll(t, h.a, []string{PermRead}, allSessions)
	route := testRouteID(1)
	h.subscribe(tok.AccessToken, route, testSecretN(1))
	h.activate(route, hp.DirectWakeRouteGone)
	g := h.a.state.Grants[tok.ClientID]
	s := g.WakeSubs[route]
	if s.State != wakeSubStateFailed || s.Failure != WakeFailRouteGone || s.Secret != "" {
		t.Fatalf("after route_gone: %+v", s)
	}
	raw, _ := os.ReadFile(filepath.Join(h.root, grantsFile))
	if strings.Contains(string(raw), testSecretN(1)) {
		t.Fatal("activation secret persisted after a terminal transition")
	}
	// Expiry also erases it, and the subscription reads as failed.
	route2 := testRouteID(2)
	h.subscribe(tok.AccessToken, route2, testSecretN(2))
	h.clk.add(WakeActivationTTL + time.Second)
	tok2, err := h.a.Refresh(tok.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	code, out := h.post(PathWakeSubscriptions, tok2.AccessToken, struct{}{})
	if code != 200 || !strings.Contains(fmt.Sprint(out), WakeFailActivationExpired) {
		t.Fatalf("expired activation = %d %v", code, out)
	}
	if items := h.ws.Lease(context.Background(), 16, 0); len(items) != 0 {
		t.Fatalf("expired activation leased: %+v", items)
	}
}

func TestWakeSubscribeRules(t *testing.T) {
	h := newWakeHarness(t)
	a := enroll(t, h.a, []string{PermRead}, allSessions)
	b := enroll(t, h.a, []string{PermRead}, allSessions)
	route := testRouteID(1)
	// Several grants may hold activating entries for one route.
	h.subscribe(a.AccessToken, route, testSecretN(1))
	h.subscribe(b.AccessToken, route, testSecretN(9))
	// Same secret: idempotent; new secret on pending: replaces.
	h.subscribe(a.AccessToken, route, testSecretN(1))
	h.subscribe(a.AccessToken, route, testSecretN(2))
	if d := h.a.state.Grants[a.ClientID].WakeSubs[route].SecretDigest; d != secretDigest(testSecretN(2)) {
		t.Fatal("new secret did not replace the pending activation")
	}
	h.activate(route, hp.DirectWakeAccepted) // first leased activation (a or b) is accepted
	var active string
	for _, tok := range []TokenResponse{a, b} {
		if h.a.state.Grants[tok.ClientID].WakeSubs[route].State == wakeSubStateActive {
			active = tok.AccessToken
		}
	}
	if active == "" {
		t.Fatal("no subscription became active")
	}
	code, out := h.post(PathWakeSubscribe, active, WakeSubscribeRequest{RouteID: route, ActivationSecret: testSecretN(7)})
	if code != http.StatusConflict || out["code"] != CodeWakeAlreadyActive {
		t.Fatalf("new secret on active = %d %v", code, out)
	}
	// Per-grant cap counts only non-failed entries; failures stay readable but
	// bounded, and a retry remains possible.
	c := enroll(t, h.a, []string{PermRead}, allSessions)
	for i := 0; i < 20; i++ {
		r := testRouteID(byte(100 + i))
		h.subscribe(c.AccessToken, r, testSecretN(byte(i+1)))
		h.activate(r, hp.DirectWakeRouteGone)
	}
	failed := 0
	for _, s := range h.a.state.Grants[c.ClientID].WakeSubs {
		if s.State == wakeSubStateFailed {
			failed++
		}
	}
	if failed > MaxWakeFailedPerGrant {
		t.Fatalf("%d failure records retained, bound %d", failed, MaxWakeFailedPerGrant)
	}
	for i := 0; i < MaxWakeSubsPerGrant; i++ {
		h.subscribe(c.AccessToken, testRouteID(byte(200+i)), testSecretN(byte(50+i)))
	}
	code, out = h.post(PathWakeSubscribe, c.AccessToken, WakeSubscribeRequest{RouteID: testRouteID(250), ActivationSecret: testSecretN(60)})
	if code != http.StatusConflict || out["code"] != CodeCapacity {
		t.Fatalf("over per-grant cap = %d %v", code, out)
	}
}

func TestWakeHostCapBoundsRouteSync(t *testing.T) {
	saved := maxWakeSubsPerHost
	maxWakeSubsPerHost = 3
	defer func() { maxWakeSubsPerHost = saved }()
	h := newWakeHarness(t)
	var toks []TokenResponse
	for i := 0; i < 4; i++ {
		toks = append(toks, enroll(t, h.a, []string{PermRead}, allSessions))
	}
	for i := 0; i < 3; i++ {
		h.subscribe(toks[i].AccessToken, testRouteID(byte(i+1)), testSecretN(byte(i+1)))
	}
	code, out := h.post(PathWakeSubscribe, toks[3].AccessToken, WakeSubscribeRequest{RouteID: testRouteID(9), ActivationSecret: testSecretN(9)})
	if code != http.StatusConflict || out["code"] != CodeCapacity {
		t.Fatalf("over host cap = %d %v", code, out)
	}
	if n := len(h.ws.Routes()); n != 3 {
		t.Fatalf("route sync lists %d, want all 3", n)
	}
}

func TestWakeRoutesRejectQueryAndPassPreflight(t *testing.T) {
	h := newWakeHarness(t)
	tok := enroll(t, h.a, []string{PermRead}, allSessions)
	raw, _ := json.Marshal(WakeFetchRequest{RouteID: testRouteID(1)})
	if w := do(h.srv, http.MethodPost, PathWakeEvents+"?route_id="+testRouteID(1), tok.AccessToken, string(raw)); w.Code != http.StatusBadRequest {
		t.Fatalf("route in query = %d", w.Code)
	}
	for _, p := range []string{PathWakeSubscribe, PathWakeUnsubscribe, PathWakeSubscriptions, PathWakeWatch, PathWakeEvents, PathWakeAck} {
		w := do(h.srv, http.MethodOptions, p, "", "", "Origin", testOrigin, "Access-Control-Request-Method", "POST")
		if w.Code != http.StatusNoContent || !strings.Contains(w.Header().Get("Access-Control-Allow-Methods"), "POST") {
			t.Fatalf("preflight %s = %d %v", p, w.Code, w.Header())
		}
	}
	// Unknown fields are refused.
	if w := do(h.srv, http.MethodPost, PathWakeAck, tok.AccessToken, `{"route_id":"`+testRouteID(1)+`","wake_ids":["x"]}`); w.Code != http.StatusBadRequest {
		t.Fatalf("legacy unversioned ack body = %d", w.Code)
	}
}

func TestWakeSessionsCarryIncarnation(t *testing.T) {
	h := newWakeHarness(t)
	tok := enroll(t, h.a, []string{PermRead}, allSessions)
	w := do(h.srv, http.MethodGet, PathSession, tok.AccessToken, "")
	var resp SessionsResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	var ids []string
	for _, s := range resp.Sessions {
		if s.Incarnation != "inc-"+s.ID {
			t.Fatalf("row %s incarnation %q", s.ID, s.Incarnation)
		}
		ids = append(ids, s.ID)
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		t.Fatal("no rows")
	}
}

// A dropped pending record keeps every snapshot incomplete, including a retry
// after a lost final response, until the phone confirms recovered_through.
func TestWakeLossWatermarkSurvivesRetries(t *testing.T) {
	h := newWakeHarness(t)
	tok := enroll(t, h.a, []string{PermRead}, allSessions)
	route := testRouteID(1)
	h.subscribe(tok.AccessToken, route, testSecretN(1))
	h.tick()
	h.activate(route, hp.DirectWakeAccepted)
	h.b.add("a1", 1, permEv("i", "r", "g"))
	h.tick()
	h.clk.add(wakeRetention + time.Minute) // the open record expires unacked
	fresh, err := h.a.Refresh(tok.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	at := fresh.AccessToken
	first := h.fetch(at, route, WakeFetchRequest{Snapshot: true})
	if first.Complete == nil || *first.Complete || first.LossThrough == 0 {
		t.Fatalf("snapshot after loss = %+v", first)
	}
	// The final response was lost; the retry must still say incomplete.
	retry := h.fetch(at, route, WakeFetchRequest{Snapshot: true})
	if retry.Complete == nil || *retry.Complete || retry.LossThrough != first.LossThrough {
		t.Fatalf("retry after lost response = %+v", retry)
	}
	// Recovering beyond the watermark is refused.
	raw, _ := json.Marshal(WakeFetchRequest{RouteID: route, Snapshot: true, RecoveredThrough: first.LossThrough + 1})
	if w := do(h.srv, http.MethodPost, PathWakeEvents, at, string(raw)); w.Code != http.StatusBadRequest {
		t.Fatalf("recovered_through past the watermark = %d", w.Code)
	}
	done := h.fetch(at, route, WakeFetchRequest{Snapshot: true, RecoveredThrough: first.LossThrough})
	if done.Complete == nil || !*done.Complete {
		t.Fatalf("snapshot after confirmed recovery = %+v", done)
	}
	again := h.fetch(at, route, WakeFetchRequest{Snapshot: true})
	if again.Complete == nil || !*again.Complete {
		t.Fatalf("second consecutive snapshot = %+v", again)
	}
}

func TestWakeCursorBounds(t *testing.T) {
	h := newWakeHarness(t)
	tok := enroll(t, h.a, []string{PermRead}, allSessions)
	route := testRouteID(1)
	h.subscribe(tok.AccessToken, route, testSecretN(1))
	h.tick()
	h.b.add("a1", 1, permEv("i", "r", "g"))
	h.tick()
	cur := h.fetch(tok.AccessToken, route, WakeFetchRequest{}).NextSeq
	for _, bad := range []WakeFetchRequest{
		{RouteID: route, Snapshot: true, SnapshotHW: cur + 5},
		{RouteID: route, Snapshot: true, SnapshotHW: cur, AfterSeq: cur + 1},
		{RouteID: route, SnapshotHW: cur},
	} {
		raw, _ := json.Marshal(bad)
		if w := do(h.srv, http.MethodPost, PathWakeEvents, tok.AccessToken, string(raw)); w.Code != http.StatusBadRequest {
			t.Fatalf("%+v = %d %s", bad, w.Code, w.Body)
		}
	}
	// A cursor beyond the store (older wake.json restored) gets an explicit
	// reset, never a silent forward jump.
	f := h.fetch(tok.AccessToken, route, WakeFetchRequest{AfterSeq: cur + 100})
	if f.Gap == nil || f.Gap.Reason != WakeGapReset || f.NextSeq != cur {
		t.Fatalf("future cursor = %+v", f)
	}
}

// A source-read failure on the first read defers the baseline; it never
// becomes head 0, which would replay old activity as new.
func TestWakeHistoryFailureDefersBaseline(t *testing.T) {
	root := t.TempDir()
	clk := &clock{t: time.Unix(1_800_000_000, 0)}
	a := openTestAuthority(t, root, Lifetimes{}, clk.now)
	_, _ = a.EnableWake("usr", "prov")
	wb := newWakeBackend(core.Session{ID: "s1", Title: "x", Runtime: "claude"})
	wb.add("s1", 1, permEv("old", "old", "g"))
	wb.add("s1", 2, permEv("old2", "old2", "g"))
	wb.srcFail = 1000
	ws := NewWakeService(a, root, wb, nil)
	tok := enroll(t, a, []string{PermRead}, allSessions)
	l, _ := a.Admit(context.Background(), tok.AccessToken, PermRead)
	_, _ = a.WakeSubscribe(l, testRouteID(1), testSecretN(1))
	l.Done()
	nonce := a.wakeAuthSnapshot().Nonce
	ws.Tick(context.Background())
	if ws.store.based(tok.ClientID, testRouteID(1), nonce, "s1") {
		t.Fatal("baseline set from a failed source read")
	}
	wb.evMu.Lock()
	wb.srcFail = 0
	wb.evMu.Unlock()
	ws.Tick(context.Background())
	if low, ok := ws.store.lowMarks("s1", []wakeTarget{{tok.ClientID, testRouteID(1), nonce}}); !ok || low["claude"] != 2 {
		t.Fatalf("baseline after recovery = %v %v, want claude=2", low, ok)
	}
	ws.Tick(context.Background())
	sub := ws.store.data.Subs[subKey(tok.ClientID, testRouteID(1), nonce)]
	if len(sub.Records) != 0 {
		t.Fatalf("old activity replayed: %+v", sub.Records)
	}
}

// Constructing, starting and ticking the wake service with an expired
// activation pending and a failing grants.json write: the wake path makes no
// authority write at all, direct stays fully usable, and a real authority
// mutation still fails closed.
func TestWakeServiceNeverWritesTheAuthorityForCleanup(t *testing.T) {
	root := t.TempDir()
	clk := &clock{t: time.Unix(1_800_000_000, 0)}
	a, _ := OpenAuthority(root, Lifetimes{}, clk.now)
	_, _ = a.EnableWake("usr", "prov")
	tok := enroll(t, a, []string{PermRead}, allSessions)
	l, _ := a.Admit(context.Background(), tok.AccessToken, PermRead)
	_, _ = a.WakeSubscribe(l, testRouteID(1), testSecretN(1))
	l.Done()
	// Age the activation past its TTL in memory (the access token stays valid).
	a.mu.Lock()
	a.state.Grants[tok.ClientID].WakeSubs[testRouteID(1)].ActivatingAt -= int64((WakeActivationTTL + time.Minute) / time.Second)
	a.mu.Unlock()
	writes := 0
	a.st.write = func(string, string, []byte) error { writes++; return fmt.Errorf("%w: dir sync", errAmbiguousCommit) }
	b := newWakeBackend(testRows()...)
	ws := NewWakeService(a, root, b, nil)
	for i := 0; i < 3; i++ {
		ws.Tick(context.Background())
	}
	if items := ws.Lease(context.Background(), 16, 0); len(items) != 0 {
		t.Fatalf("expired activation leased: %+v", items)
	}
	if writes != 0 || a.degraded != nil {
		t.Fatalf("wake path wrote the authority %d times (degraded=%v)", writes, a.degraded)
	}
	srv, _ := NewServer(Config{Authority: a, Backend: b, Endpoints: []string{testEndpoint}, Now: clk.now, Wake: ws})
	if w := do(srv, http.MethodGet, PathSession, tok.AccessToken, ""); w.Code != http.StatusOK {
		t.Fatalf("direct read after wake cleanup with a failing disk = %d", w.Code)
	}
	// A real authority mutation keeps the existing fail-closed semantics.
	if _, _, err := a.NewCode("x", []string{PermRead}, allSessions, 0); err == nil || a.degraded == nil {
		t.Fatalf("an uncertain real commit must fail closed (err=%v degraded=%v)", err, a.degraded)
	}
	a.Close()
}

// A real two-source session (transcript + amux journal): the journal already
// has events, then the transcript grows. A watched turn end appended to the
// transcript is woken exactly once, journal events are not replayed, and the
// record's identity and inline event stay stable across further appends to
// either source and across a restart.
func TestWakeTwoSourceSessionStableIdentity(t *testing.T) {
	h := newWakeHarness(t)
	tok := enroll(t, h.a, []string{PermRead}, allSessions)
	route := testRouteID(1)
	h.b.addSrc("a1", "claude", 1, textEv())
	h.b.addSrc("a1", "journal", 1, textEv())
	h.b.addSrc("a1", "journal", 2, textEv())
	h.subscribe(tok.AccessToken, route, testSecretN(1))
	h.tick() // baseline claude=1, journal=2
	if code, out := h.post(PathWakeWatch, tok.AccessToken, WakeWatchRequest{RouteID: route, SessionID: "a1", Incarnation: "inc-a1", Watch: true}); code != 200 {
		t.Fatalf("watch %d %v", code, out)
	}
	// Growth between the baseline read and the next read, in both sources.
	h.b.addSrc("a1", "journal", 3, textEv())
	h.b.addSrc("a1", "claude", 2, turnEndEv())
	h.tick()
	f := h.fetch(tok.AccessToken, route, WakeFetchRequest{})
	if len(f.Items) != 1 || f.Items[0].Kind != WakeKindTurnEnd || f.Items[0].Source != "claude" || f.Items[0].Index != 2 {
		t.Fatalf("two-source turn end = %+v", f)
	}
	if f.Items[0].Event == nil || f.Items[0].Event.Type != hp.TypeTurnEnd {
		t.Fatalf("inline event = %+v", f.Items[0].Event)
	}
	first := f.Items[0]
	// Further appends to the EARLIER source (the transcript) and the journal:
	// no duplicate, same identity.
	h.b.addSrc("a1", "claude", 3, textEv())
	h.b.addSrc("a1", "journal", 4, textEv())
	h.tick()
	// Clean restart (Run flushes on shutdown): a new service over the same files.
	if err := h.ws.store.flush(true); err != nil {
		t.Fatal(err)
	}
	h.ws = NewWakeService(h.a, h.root, h.b, nil)
	h.srv.cfg.Wake = h.ws
	h.tick()
	again := h.fetch(tok.AccessToken, route, WakeFetchRequest{})
	if len(again.Items) != 1 || again.Items[0].WakeID != first.WakeID || again.Items[0].Index != 2 || again.Items[0].Source != "claude" {
		t.Fatalf("after appends and restart = %+v", again)
	}
	if again.Items[0].Event == nil || again.Items[0].Event.Type != hp.TypeTurnEnd {
		t.Fatal("inline event moved after appends to another source")
	}
}

// A source truncated in place (same generation, head below the cursor) is
// re-baselined; nothing in it is replayed as new.
func TestWakeTruncatedSourceIsRebaselined(t *testing.T) {
	h := newWakeHarness(t)
	tok := enroll(t, h.a, []string{PermRead}, allSessions)
	route := testRouteID(1)
	for i := int64(1); i <= 3; i++ {
		h.b.add("a1", i, textEv())
	}
	h.subscribe(tok.AccessToken, route, testSecretN(1))
	h.tick() // claude=3
	h.b.truncateSrc("a1", "claude", 1)
	h.tick()
	h.b.add("a1", 2, permEv("new", "new", "g"))
	h.tick()
	f := h.fetch(tok.AccessToken, route, WakeFetchRequest{})
	if len(f.Items) != 1 || f.Items[0].RequestID != "new" {
		t.Fatalf("after truncation = %+v", f)
	}
}

// A source replaced by a new file (new generation) to the SAME or a larger
// head is a new source incarnation: it is read from index 0, so nothing in the
// new file is skipped, and an older record's address (old generation) never
// fetches the new file's content.
func TestWakeReplacedSourceSameOrLargerHead(t *testing.T) {
	h := newWakeHarness(t)
	tok := enroll(t, h.a, []string{PermRead}, allSessions)
	route := testRouteID(1)
	h.subscribe(tok.AccessToken, route, testSecretN(1))
	h.tick()
	if code, out := h.post(PathWakeWatch, tok.AccessToken, WakeWatchRequest{RouteID: route, SessionID: "a1", Incarnation: "inc-a1", Watch: true}); code != 200 {
		t.Fatalf("watch %d %v", code, out)
	}
	h.b.add("a1", 1, textEv())
	h.b.add("a1", 2, turnEndEv())
	h.tick()
	before := h.fetch(tok.AccessToken, route, WakeFetchRequest{})
	if len(before.Items) != 1 || before.Items[0].Index != 2 || before.Items[0].Event == nil {
		t.Fatalf("first turn end = %+v", before)
	}
	// Replace with a new file whose head (3) is larger, holding a new turn end
	// at index 2 and an open permission at index 3.
	h.b.replaceSrc("a1", "claude", textEv(), turnEndEv(), permEv("p", "r", "g"))
	h.tick()
	h.tick()
	f := h.fetch(tok.AccessToken, route, WakeFetchRequest{})
	var turnEnds, perms int
	for _, it := range f.Items {
		switch {
		case it.Kind == WakeKindTurnEnd && it.WakeID == before.Items[0].WakeID:
			if it.Event != nil {
				t.Fatalf("old-generation record fetched new-file content: %+v", it.Event)
			}
		case it.Kind == WakeKindTurnEnd:
			turnEnds++
			if it.Event == nil || it.Index != 2 {
				t.Fatalf("new-generation turn end = %+v", it)
			}
		case it.Kind == WakeKindPermission:
			perms++
		}
	}
	if turnEnds != 1 || perms != 1 {
		t.Fatalf("replacement with a larger head skipped events: %+v", f)
	}
	// Same-head replacement too.
	h.b.replaceSrc("a1", "claude", textEv(), textEv(), turnEndEv())
	h.tick()
	h.tick()
	g := h.fetch(tok.AccessToken, route, WakeFetchRequest{AfterSeq: f.NextSeq})
	n := 0
	for _, it := range g.Items {
		if it.Kind == WakeKindTurnEnd && it.State == WakeStateDone && it.Index == 3 {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("same-head replacement skipped the new turn end: %+v", g)
	}
}

// A source that appears after the baseline (a journal created later) starts
// at 0: its first event is wake-worthy.
func TestWakeLateSourceStartsAtZero(t *testing.T) {
	h := newWakeHarness(t)
	tok := enroll(t, h.a, []string{PermRead}, allSessions)
	route := testRouteID(1)
	h.b.add("a1", 1, textEv())
	h.subscribe(tok.AccessToken, route, testSecretN(1))
	h.tick()
	h.b.addSrc("a1", "permission", 1, permEv("p1", "r1", "g"))
	h.tick()
	f := h.fetch(tok.AccessToken, route, WakeFetchRequest{})
	if len(f.Items) != 1 || f.Items[0].Source != "permission" || f.Items[0].Index != 1 {
		t.Fatalf("late source event = %+v", f)
	}
}

func TestWakeSubscriptionsDiscloseUncoveredSessions(t *testing.T) {
	h := newWakeHarness(t)
	tok := enroll(t, h.a, []string{PermRead}, allSessions)
	h.subscribe(tok.AccessToken, testRouteID(1), testSecretN(1))
	h.b.noIdentityFor("d1")
	code, out := h.post(PathWakeSubscriptions, tok.AccessToken, struct{}{})
	if code != 200 || fmt.Sprint(out["uncovered_sessions"]) != "[d1]" {
		t.Fatalf("subscriptions = %d %v", code, out)
	}
}

// noIdentityFor makes the store report no identity for sid.
func (b *wakeBackend) noIdentityFor(sid string) { b.fakeBackend.setInc(sid, "-") }

// Inline events are proven to be the recorded event: a different event at the
// same index (same generation) and a session id reused between the
// visibility check and the lookup both omit the event.
func TestWakeInlineEventMustMatchRecord(t *testing.T) {
	h := newWakeHarness(t)
	tok := enroll(t, h.a, []string{PermRead}, allSessions)
	route := testRouteID(1)
	h.subscribe(tok.AccessToken, route, testSecretN(1))
	h.tick()
	h.b.add("a1", 1, permEv("item", "req", "gen"))
	h.tick()
	f := h.fetch(tok.AccessToken, route, WakeFetchRequest{})
	if len(f.Items) != 1 || f.Items[0].Event == nil {
		t.Fatalf("baseline fetch = %+v", f)
	}
	// Unrelated content at the recorded index (same generation).
	h.b.evMu.Lock()
	h.b.logs["a1"]["claude"][0] = permEv("other", "rm -rf", "gen")
	h.b.evMu.Unlock()
	if g := h.fetch(tok.AccessToken, route, WakeFetchRequest{}); len(g.Items) != 1 || g.Items[0].Event != nil {
		t.Fatalf("mismatched event attached: %+v", g.Items)
	}
	// Restore, then reuse the session id between visibility and lookup.
	h.b.evMu.Lock()
	h.b.logs["a1"]["claude"][0] = permEv("item", "req", "gen")
	h.b.evMu.Unlock()
	h.b.onSourceRead = func() { h.b.setInc("a1", "inc-reused") }
	g := h.fetch(tok.AccessToken, route, WakeFetchRequest{})
	for _, it := range g.Items {
		if it.Event != nil {
			t.Fatalf("event attached across a session id reuse: %+v", it)
		}
	}
}

// Restore-before-resume copies the captured transcript over the path (a new
// file, same first line, with the historical turn ends): the copied history
// is not replayed, a genuine new turn end after it is woken exactly once, and
// a recorded item's inline event survives the copy (exact ids still checked).
func TestWakeResumeCopyDoesNotReplayHistory(t *testing.T) {
	h := newWakeHarness(t)
	tok := enroll(t, h.a, []string{PermRead}, allSessions)
	route := testRouteID(1)
	h.subscribe(tok.AccessToken, route, testSecretN(1))
	h.tick()
	if code, out := h.post(PathWakeWatch, tok.AccessToken, WakeWatchRequest{RouteID: route, SessionID: "a1", Incarnation: "inc-a1", Watch: true}); code != 200 {
		t.Fatalf("watch %d %v", code, out)
	}
	h.b.add("a1", 1, textEv())
	h.b.add("a1", 2, turnEndEv())
	h.tick()
	first := h.fetch(tok.AccessToken, route, WakeFetchRequest{})
	if len(first.Items) != 1 {
		t.Fatalf("first = %+v", first)
	}
	// Copy: same history (indexes 1-2) plus one new turn end at 4.
	h.b.copySrc("a1", "claude", textEv(), turnEndEv(), textEv(), turnEndEv())
	h.tick()
	h.tick()
	f := h.fetch(tok.AccessToken, route, WakeFetchRequest{})
	var idx []int64
	for _, it := range f.Items {
		if it.Kind == WakeKindTurnEnd {
			idx = append(idx, it.Index)
			if it.Event == nil {
				t.Fatalf("inline event lost across a same-transcript copy: %+v", it)
			}
		}
	}
	sort.Slice(idx, func(i, j int) bool { return idx[i] < idx[j] })
	if len(idx) != 2 || idx[0] != 2 || idx[1] != 4 {
		t.Fatalf("turn ends after a resume copy = %v, want the original 2 and the new 4 only", idx)
	}
	// dte1: each turn end carries the shared occurrence key from its exact
	// source position; the copy keeps the first-line digest, so the original
	// turn end keeps its key, and the new one has its own.
	for _, it := range f.Items {
		if it.Kind != WakeKindTurnEnd {
			continue
		}
		want := hp.DirectTurnKey("a1", "inc-a1", "claude", "l-a1-claude", it.Index)
		if want == "" || it.TurnKey != want {
			t.Fatalf("turn end %d key %q, want %q", it.Index, it.TurnKey, want)
		}
	}
	if first.Items[0].TurnKey == "" || first.Items[0].TurnKey != hp.DirectTurnKey("a1", "inc-a1", "claude", "l-a1-claude", 2) {
		t.Fatalf("pre-copy key %q", first.Items[0].TurnKey)
	}
}

// A permission raised right after subscribing (before any detector tick) is
// woken: the subscription baselines eagerly. A session created after the
// subscription, with events already written before the next tick, starts at
// 0. An old session that later enters the grant's scope does not replay its
// history.
func TestWakeBaselineRaces(t *testing.T) {
	rows := testRows()
	h := newWakeHarness(t, rows...)
	tok := enroll(t, h.a, []string{PermRead}, ResourceScope{Kind: ResourceWorkgroups, Workgroups: []string{"wg1"}})
	route := testRouteID(1)
	h.b.add("a1", 1, textEv())
	h.b.add("d1", 1, permEv("old", "old", "g")) // d1 is outside the grant's scope
	h.subscribe(tok.AccessToken, route, testSecretN(1))
	h.b.add("a1", 2, permEv("fast", "fast", "g")) // before any tick
	// A brand-new session in scope, already holding a permission.
	rows = append(rows, core.Session{ID: "n1", Title: "new", RootID: "wg1", Runtime: "claude"})
	h.b.setRows(rows...)
	h.b.add("n1", 1, permEv("new", "new", "g"))
	h.tick()
	h.tick()
	got := map[string]bool{}
	for _, it := range h.fetch(tok.AccessToken, route, WakeFetchRequest{}).Items {
		got[it.RequestID] = true
	}
	if !got["fast"] || !got["new"] || got["old"] {
		t.Fatalf("after subscribe races: %v", got)
	}
	// d1 (old) moves into scope: its history is not replayed.
	for i := range rows {
		if rows[i].ID == "d1" {
			rows[i].RootID = "wg1"
		}
	}
	h.b.setRows(rows...)
	h.tick()
	h.tick()
	for _, it := range h.fetch(tok.AccessToken, route, WakeFetchRequest{}).Items {
		if it.RequestID == "old" {
			t.Fatal("an old session entering scope replayed its history")
		}
	}
}

// A source that is absent for one or more ticks (between a restore's delete
// and its copy) is unknown, not empty: when the copy (old prefix + one new
// event) appears, old watched turn ends are not replayed and the new one is
// delivered.
func TestWakeMissingSourceKeepsMarks(t *testing.T) {
	h := newWakeHarness(t)
	tok := enroll(t, h.a, []string{PermRead}, allSessions)
	route := testRouteID(1)
	h.subscribe(tok.AccessToken, route, testSecretN(1))
	h.tick()
	if code, out := h.post(PathWakeWatch, tok.AccessToken, WakeWatchRequest{RouteID: route, SessionID: "a1", Incarnation: "inc-a1", Watch: true}); code != 200 {
		t.Fatalf("watch %d %v", code, out)
	}
	h.b.add("a1", 1, textEv())
	h.b.add("a1", 2, turnEndEv())
	h.tick()
	first := h.fetch(tok.AccessToken, route, WakeFetchRequest{})
	if len(first.Items) != 1 || first.Items[0].Index != 2 {
		t.Fatalf("first = %+v", first)
	}
	h.b.setMissing("a1", "claude", true)
	h.tick()
	h.tick()
	h.b.copySrc("a1", "claude", textEv(), turnEndEv(), textEv(), turnEndEv())
	h.b.setMissing("a1", "claude", false)
	h.tick()
	h.tick()
	var idx []int64
	for _, it := range h.fetch(tok.AccessToken, route, WakeFetchRequest{}).Items {
		if it.Kind == WakeKindTurnEnd {
			idx = append(idx, it.Index)
		}
	}
	sort.Slice(idx, func(i, j int) bool { return idx[i] < idx[j] })
	if len(idx) != 2 || idx[0] != 2 || idx[1] != 4 {
		t.Fatalf("turn ends after missing ticks + restore = %v, want the original 2 and the new 4", idx)
	}
}

// The eager pass decides "existed at subscribe time" by membership, never by
// clock: a slow pass crossing seconds, an out-of-scope old session that later
// enters scope, and a session whose head read failed during the pass all
// baseline at their heads; a reused id observed under its old incarnation
// starts a new file at 0.
func TestWakeEagerMembership(t *testing.T) {
	rows := testRows()
	h := newWakeHarness(t, rows...)
	tok := enroll(t, h.a, []string{PermRead}, ResourceScope{Kind: ResourceWorkgroups, Workgroups: []string{"wg1"}})
	route := testRouteID(1)
	h.b.add("a1", 1, permEv("a-old", "a-old", "g"))
	h.b.add("d1", 1, permEv("d-old", "d-old", "g")) // out of scope
	// The first head read of the pass fails, and the pass is slow.
	h.b.srcFail = 1
	h.b.onSourceRead = func() { h.clk.add(5 * time.Second) }
	h.subscribe(tok.AccessToken, route, testSecretN(1))
	h.clk.add(5 * time.Second)
	h.tick()
	h.tick()
	for _, it := range h.fetch(tok.AccessToken, route, WakeFetchRequest{}).Items {
		t.Fatalf("pre-subscribe history replayed: %+v", it)
	}
	// d1 enters scope: still history.
	for i := range rows {
		if rows[i].ID == "d1" {
			rows[i].RootID = "wg1"
		}
	}
	h.b.setRows(rows...)
	h.tick()
	h.tick()
	for _, it := range h.fetch(tok.AccessToken, route, WakeFetchRequest{}).Items {
		t.Fatalf("old session entering scope replayed: %+v", it)
	}
}

// An id observed by the eager pass (out of scope) is reused by a new session
// that is in scope. With no stored generation proving the transcript is a
// new file, the reused id baselines at its heads: the old session's history
// is never replayed (a new file is read from 0 only when proven new).
func TestWakeEagerReusedID(t *testing.T) {
	rows := testRows()
	h := newWakeHarness(t, rows...)
	tok := enroll(t, h.a, []string{PermRead}, ResourceScope{Kind: ResourceWorkgroups, Workgroups: []string{"wg1"}})
	route := testRouteID(1)
	h.b.add("d1", 1, permEv("d-old", "d-old", "g")) // out of scope
	h.subscribe(tok.AccessToken, route, testSecretN(1))
	// d1's id is reused by a new in-scope session with a new transcript.
	h.b.setInc("d1", "inc-d1-new")
	h.b.replaceSrc("d1", "claude", permEv("d-new", "d-new", "g"))
	for i := range rows {
		if rows[i].ID == "d1" {
			rows[i].RootID = "wg1"
		}
	}
	h.b.setRows(rows...)
	h.tick()
	h.tick()
	got := map[string]bool{}
	for _, it := range h.fetch(tok.AccessToken, route, WakeFetchRequest{}).Items {
		got[it.RequestID] = true
	}
	if got["d-old"] {
		t.Fatalf("reused id replayed the old session: %v", got)
	}
}

// Two subscriptions and a reused id: A (all sessions) has d1 baselined, so
// the store knows d1's transcript generation; B (wg1) observed d1 out of
// scope. d1 is reused by a new in-scope session with a NEW transcript: both
// get the new permission and neither replays the old one. With a shared
// (same-file) source instead, neither replays and the next event wakes both.
func TestWakeReusedIDTwoSubscribers(t *testing.T) {
	for _, sameFile := range []bool{false, true} {
		rows := testRows()
		h := newWakeHarness(t, rows...)
		tokA := enroll(t, h.a, []string{PermRead}, allSessions)
		tokB := enroll(t, h.a, []string{PermRead}, ResourceScope{Kind: ResourceWorkgroups, Workgroups: []string{"wg1"}})
		rA, rB := testRouteID(1), testRouteID(2)
		h.b.add("d1", 1, permEv("d-old", "d-old", "g"))
		h.subscribe(tokA.AccessToken, rA, testSecretN(1))
		h.tick()
		h.subscribe(tokB.AccessToken, rB, testSecretN(2))
		h.tick()
		seqA := h.fetch(tokA.AccessToken, rA, WakeFetchRequest{}).NextSeq
		h.b.setInc("d1", "inc-d1-new")
		if sameFile {
			h.b.add("d1", 2, permEv("d-new", "d-new", "g2"))
		} else {
			h.b.replaceSrc("d1", "claude", permEv("d-new", "d-new", "g2"))
		}
		for i := range rows {
			if rows[i].ID == "d1" {
				rows[i].RootID = "wg1"
			}
		}
		h.b.setRows(rows...)
		h.tick()
		h.tick()
		if sameFile {
			// Same file: the reused id restarts at its head (never replays);
			// the next event wakes both.
			h.b.add("d1", 3, permEv("d-next", "d-next", "g2"))
			h.tick()
			h.tick()
		}
		want := "d-new"
		if sameFile {
			want = "d-next"
		}
		for _, c := range []struct {
			name, tok, route string
			after            int64
		}{{"A", tokA.AccessToken, rA, seqA}, {"B", tokB.AccessToken, rB, 0}} {
			got := map[string]bool{}
			for _, it := range h.fetch(c.tok, c.route, WakeFetchRequest{AfterSeq: c.after}).Items {
				if it.Kind == WakeKindPermission {
					got[it.RequestID] = true
				}
			}
			if got["d-old"] || !got[want] {
				t.Fatalf("sameFile=%v %s: %v (want %s, no d-old)", sameFile, c.name, got, want)
			}
		}
	}
}
