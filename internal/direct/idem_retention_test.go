package direct

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"amux/internal/core"
	"amux/internal/daemon"
	"amux/internal/hostbackend"
)

// retentionRig is a server on a test clock whose fake backend never blocks on
// its applied channel, with a refresh helper (the access token is long dead
// after a day's advance).
type retentionRig struct {
	t     *testing.T
	root  string
	c     *clock
	f     *fakeBackend
	a     *Authority
	s     *Server
	tok   TokenResponse
	perms []string
}

func newRetentionRig(t *testing.T) *retentionRig {
	t.Helper()
	r := &retentionRig{t: t, root: t.TempDir(), c: &clock{t: time.Unix(1_800_000_000, 0)}, f: workflowFake(), perms: allPerms}
	r.f.applied = make(chan core.Action, 4096)
	r.f.newID = "n1"
	r.a = openTestAuthority(t, r.root, Lifetimes{}, r.c.now)
	r.s = newTestServer(t, r.a, r.f)
	r.tok = enroll(t, r.a, r.perms, allSessions)
	return r
}

// refresh rotates the grant's tokens and returns a live access token.
func (r *retentionRig) refresh() string {
	r.t.Helper()
	tok, err := r.a.Refresh(r.tok.RefreshToken)
	if err != nil {
		r.t.Fatal(err)
	}
	r.tok = tok
	return tok.AccessToken
}

// restart closes the authority and reopens the same root on the same clock.
func (r *retentionRig) restart() {
	r.t.Helper()
	r.a.Close()
	r.a = openTestAuthority(r.t, r.root, Lifetimes{}, r.c.now)
	r.s = newTestServer(r.t, r.a, r.f)
	r.refresh() // access tokens are memory-only
}

func (r *retentionRig) post(sid, verb string, fields map[string]string, key string) (int, string) {
	r.t.Helper()
	code, ec, _ := act(r.s, r.tok.AccessToken, sid, verb, fields, key)
	return code, ec
}

// past moves the clock beyond the full-result retention of everything
// admitted so far.
func (r *retentionRig) past() { r.c.add(idemRetention + time.Minute) }

// The core regression: an action finishes, its full result's retention
// passes, and the same key and body is replayed (a lost reply retried late).
// It must never reach the daemon again, for every effectful verb, sync and
// creation, whatever the recorded outcome was.
func TestExpiredResultIsNeverReRun(t *testing.T) {
	verbs := []struct {
		sid, verb string
		fields    map[string]string
	}{
		{"a1", VerbPrompt, map[string]string{"text": "hi"}},
		{"a1", VerbInterject, map[string]string{"text": "hey"}},
		{"a1", VerbStop, nil},
		{"wg1", VerbGoal, map[string]string{"status": "paused"}},
		{"a1", VerbPermission, map[string]string{"request_id": "r1", "runtime_generation": "g1", "decision": "allow"}},
		{"a1", VerbStart, nil},
		{"a1", VerbRename, map[string]string{"name": "x"}},
		{"wg3", VerbArchive, nil},
		{"wg2", VerbUnarchive, nil},
		{"wg1", VerbAddAgent, map[string]string{"agent": "claude"}},
		{"", VerbNewWorkgroup, map[string]string{"name": "n"}},
		{"", VerbNewWorkgroupEmpty, map[string]string{"repos": "api"}},
	}
	outcomes := []struct {
		name string
		err  error
		code string // first answer's error code ("" = success)
	}{
		{"ok", nil, ""},
		{"rejected", &daemon.RejectedError{Msg: "no"}, CodeRejected},
		{"unavailable", hostbackend.ErrUnavailable, CodeUnavailable},
	}
	for _, o := range outcomes {
		for _, v := range verbs {
			t.Run(o.name+"/"+v.verb, func(t *testing.T) {
				r := newRetentionRig(t)
				r.f.applyErr = o.err
				code, ec := r.post(v.sid, v.verb, v.fields, "k")
				if ec != o.code || (o.code == "" && code != http.StatusOK) {
					t.Fatalf("first: %d %s", code, ec)
				}
				// Within retention the full result replays.
				if _, ec := r.post(v.sid, v.verb, v.fields, "k"); ec != o.code {
					t.Fatalf("replay within retention: %s", ec)
				}
				r.past()
				r.refresh()
				r.f.applyErr = nil
				for range 2 { // the first replay compacts, the second reads the tombstone
					code, ec = r.post(v.sid, v.verb, v.fields, "k")
					if code != http.StatusConflict || ec != CodeIdemExpired {
						t.Fatalf("late replay: %d %s", code, ec)
					}
				}
				// A different body under the spent key is still a conflict.
				if code, ec := r.post("a1", VerbPrompt, map[string]string{"text": "other"}, "k"); code != http.StatusUnprocessableEntity || ec != CodeIdemConflict {
					t.Fatalf("changed body: %d %s", code, ec)
				}
				if n := r.f.applies.Load(); n != 1 {
					t.Fatalf("daemon called %d times", n)
				}
				g := r.a.state.Grants[r.tok.ClientID]
				if _, full := g.Idem["k"]; full || g.Spent[spentKey("k")] == "" {
					t.Fatalf("not compacted: idem=%v spent=%v", g.Idem, g.Spent)
				}
				// A fresh key still runs.
				if code, ec := r.post(v.sid, v.verb, v.fields, "k2"); code != http.StatusOK {
					t.Fatalf("fresh key: %d %s", code, ec)
				}
			})
		}
	}
}

// The idem_expired answer is fixed: it never claims the action did or did not
// run.
func TestIdemExpiredMessageIsFixed(t *testing.T) {
	r := newRetentionRig(t)
	r.post("a1", VerbPrompt, map[string]string{"text": "hi"}, "k")
	r.past()
	w := do(r.s, "POST", PathSession+"/a1/actions", r.refresh(), actionBody(VerbPrompt, "hi", "k"))
	var e APIError
	if err := jsonStrict(w.Body.Bytes(), &e); err != nil || e.Code != CodeIdemExpired || e.Error != idemExpiredMsg {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

// Tombstones survive restart, and a full record written by a previous process
// (the legacy shape, no tombstone set) is compacted after reload, not dropped.
func TestSpentKeysSurviveRestartAndRepeatedPruning(t *testing.T) {
	r := newRetentionRig(t)
	r.post("a1", VerbPrompt, map[string]string{"text": "day0"}, "d0")
	r.restart() // the full record is on disk, compacted by nobody yet
	for day := 1; day <= 5; day++ {
		r.past()
		r.refresh()
		if code, _ := r.post("a1", VerbPrompt, map[string]string{"text": fmt.Sprint("day", day)}, fmt.Sprint("d", day)); code != http.StatusOK {
			t.Fatalf("day %d: %d", day, code)
		}
		r.restart()
		for prev := 0; prev < day; prev++ {
			if _, ec := r.post("a1", VerbPrompt, map[string]string{"text": fmt.Sprint("day", prev)}, fmt.Sprint("d", prev)); ec != CodeIdemExpired {
				t.Fatalf("day %d: key d%d answered %q", day, prev, ec)
			}
		}
	}
	if n := r.f.applies.Load(); n != 6 {
		t.Fatalf("daemon called %d times", n)
	}
	if g := r.a.state.Grants[r.tok.ClientID]; len(g.Spent) != 5 || len(g.Idem) != 1 {
		t.Fatalf("spent %d idem %d", len(g.Spent), len(g.Idem))
	}
}

// A full tombstone set never evicts: an expired record that cannot be
// compacted is kept whole yet still answers idem_expired (never its stale
// result), and the grant then fails closed with capacity_exhausted.
func TestSpentQuotaKeepsRecordsAndFailsClosed(t *testing.T) {
	oldIdem, oldSpent := maxIdemPerGrant, maxSpentPerGrant
	maxIdemPerGrant, maxSpentPerGrant = 2, 1
	t.Cleanup(func() { maxIdemPerGrant, maxSpentPerGrant = oldIdem, oldSpent })
	r := newRetentionRig(t)
	for _, k := range []string{"k1", "k2"} {
		if code, _ := r.post("", VerbNewWorkgroup, map[string]string{"name": k}, k); code != http.StatusOK {
			t.Fatal(k)
		}
	}
	r.past()
	r.refresh()
	if code, ec := r.post("", VerbNewWorkgroup, map[string]string{"name": "k3"}, "k3"); code != http.StatusOK {
		t.Fatalf("k3 after one compaction: %d %s", code, ec)
	}
	r.past()
	r.refresh()
	if code, ec := r.post("", VerbNewWorkgroup, map[string]string{"name": "k4"}, "k4"); code != http.StatusConflict || ec != CodeCapacity {
		t.Fatalf("k4 at capacity: %d %s", code, ec)
	}
	g := r.a.state.Grants[r.tok.ClientID]
	if len(g.Spent) != 1 || len(g.Idem) != 2 {
		t.Fatalf("spent %d idem %d", len(g.Spent), len(g.Idem))
	}
	r.restart()
	for _, k := range []string{"k1", "k2", "k3"} {
		code, ec := r.post("", VerbNewWorkgroup, map[string]string{"name": k}, k)
		if code != http.StatusConflict || ec != CodeIdemExpired {
			t.Fatalf("%s: %d %s (a retained record must not replay its stale result)", k, code, ec)
		}
		if code, ec := r.post("", VerbNewWorkgroup, map[string]string{"name": "changed"}, k); code != http.StatusUnprocessableEntity || ec != CodeIdemConflict {
			t.Fatalf("%s changed body: %d %s", k, code, ec)
		}
	}
	if n := r.f.applies.Load(); n != 3 {
		t.Fatalf("daemon called %d times", n)
	}
}

// A tombstone hash collision can only refuse: a fresh key whose spentKey is
// already taken is never run.
func TestSpentCollisionRefusesNeverRuns(t *testing.T) {
	r := newRetentionRig(t)
	g := r.a.state.Grants[r.tok.ClientID]
	g.Spent = map[string]string{spentKey("fresh"): "0000000000000000"}
	if code, ec := r.post("a1", VerbPrompt, map[string]string{"text": "x"}, "fresh"); code == http.StatusOK || (ec != CodeIdemConflict && ec != CodeIdemExpired) {
		t.Fatalf("collision: %d %s", code, ec)
	}
	if r.f.applies.Load() != 0 {
		t.Fatal("colliding key ran")
	}
}

// Tombstones are per grant: another grant's spent key is its own fresh key.
func TestSpentKeysAreGrantIsolated(t *testing.T) {
	r := newRetentionRig(t)
	r.post("a1", VerbPrompt, map[string]string{"text": "hi"}, "k")
	r.past()
	r.refresh()
	if _, ec := r.post("a1", VerbPrompt, map[string]string{"text": "hi"}, "k"); ec != CodeIdemExpired {
		t.Fatal(ec)
	}
	other := enroll(t, r.a, allPerms, allSessions)
	if code, ec, _ := act(r.s, other.AccessToken, "a1", VerbPrompt, map[string]string{"text": "hi"}, "k"); code != http.StatusOK {
		t.Fatalf("other grant: %d %s", code, ec)
	}
	if len(r.a.state.Grants[other.ClientID].Spent) != 0 {
		t.Fatal("tombstone leaked across grants")
	}
	if r.f.applies.Load() != 2 {
		t.Fatalf("applies %d", r.f.applies.Load())
	}
}

// A failed commit never loses protection: the compaction stays in memory
// (refusing), the full record stays on disk (refusing after restart), and
// either way the key is never re-run. An ambiguous commit degrades as before.
func TestCompactionSurvivesSaveFailure(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		t.Run(fmt.Sprint("ambiguous=", ambiguous), func(t *testing.T) {
			r := newRetentionRig(t)
			r.post("a1", VerbPrompt, map[string]string{"text": "hi"}, "k")
			r.past()
			r.refresh()
			failingWrites(r.a, ambiguous)
			// A fresh key's admission commit (carrying the compaction) fails.
			if code, ec := r.post("a1", VerbPrompt, map[string]string{"text": "new"}, "k2"); code != http.StatusServiceUnavailable || ec != CodeUnavailable {
				t.Fatalf("failed admission: %d %s", code, ec)
			}
			if _, ec := r.post("a1", VerbPrompt, map[string]string{"text": "hi"}, "k"); ec != CodeIdemExpired {
				t.Fatalf("in memory after failed commit: %s", ec)
			}
			r.a.st.write = writeAtomic
			r.restart()
			if _, ec := r.post("a1", VerbPrompt, map[string]string{"text": "hi"}, "k"); ec != CodeIdemExpired {
				t.Fatalf("after restart: %s", ec)
			}
			if r.f.applies.Load() != 1 {
				t.Fatalf("applies %d", r.f.applies.Load())
			}
		})
	}
}

// Unresolved records (admitted, ambiguous, or a state this binary does not
// know) are never compacted: they keep answering idem_ambiguous.
func TestUnresolvedAndUnknownRecordsAreNeverCompacted(t *testing.T) {
	r := newRetentionRig(t)
	g := r.a.state.Grants[r.tok.ClientID]
	h := actionHash("a1", ActionRequest{Verb: VerbPrompt, Fields: map[string]string{"text": "hi"}})
	at := r.c.now().Unix()
	g.Idem = map[string]*idemRecord{
		"amb":    {BodyHash: h, State: idemUnknown, At: at},
		"adm":    {BodyHash: h, State: idemAdmitted, Boot: "old", At: at},
		"future": {BodyHash: h, State: "reconciled", At: at},
	}
	r.past()
	r.refresh()
	for k := range g.Idem {
		if _, ec := r.post("a1", VerbPrompt, map[string]string{"text": "hi"}, k); ec != CodeIdemAmbiguous {
			t.Fatalf("%s: %s", k, ec)
		}
	}
	if len(g.Spent) != 0 || len(g.Idem) != 3 || r.f.applies.Load() != 0 {
		t.Fatalf("spent %v idem %d applies %d", g.Spent, len(g.Idem), r.f.applies.Load())
	}
}

// A replay still in flight keeps its in-progress answer; retention does not
// touch running records.
func TestInFlightKeyUnaffectedByRetention(t *testing.T) {
	r := newRetentionRig(t)
	l, err := r.a.Admit(t.Context(), r.tok.AccessToken, PermSteer)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Done()
	h := actionHash("a1", ActionRequest{Verb: VerbPrompt, Fields: map[string]string{"text": "hi"}})
	if out, _, err := r.a.BeginAction(l, "k", h, false); err != nil || out != idemProceed {
		t.Fatal(out, err)
	}
	r.past()
	r.refresh()
	if _, ec := r.post("a1", VerbPrompt, map[string]string{"text": "hi"}, "k"); ec != CodeIdemInProgress {
		t.Fatalf("in flight: %s", ec)
	}
	// Retention is measured from admission, so a result recorded after the
	// cutoff is compacted at once: still never re-run.
	r.a.FinishResult(l, "k", ActionResponse{OK: true})
	if _, ec := r.post("a1", VerbPrompt, map[string]string{"text": "hi"}, "k"); ec != CodeIdemExpired {
		t.Fatalf("finished past the cutoff: %s", ec)
	}
	if r.f.applies.Load() != 0 {
		t.Fatal("ran")
	}
}

// The idem-retention-v1 fixture is the additive contract for idem_expired. It
// lives beside the pinned v1 and workflow-v1 manifests, never inside them.
// Regenerate with UPDATE_DIRECT_FIXTURES=1 go test ./internal/direct -run TestIdemRetentionFixtures.
const idemRetentionFixtureDir = "../../docs/direct/fixtures/idem-retention-v1"

func TestIdemRetentionFixtures(t *testing.T) {
	js := func(v any) []byte { return append(mustIndent(t, v), '\n') }
	want := map[string][]byte{
		"api-error-idem-expired.json": js(APIError{Code: CodeIdemExpired, Error: idemExpiredMsg}),
	}
	want["SHA256SUMS"] = sums(want)
	if os.Getenv("UPDATE_DIRECT_FIXTURES") == "1" {
		if err := os.MkdirAll(idemRetentionFixtureDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for n, b := range want {
			if err := os.WriteFile(filepath.Join(idemRetentionFixtureDir, n), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	entries, err := os.ReadDir(idemRetentionFixtureDir)
	if err != nil {
		t.Fatalf("missing fixtures (regenerate with UPDATE_DIRECT_FIXTURES=1): %v", err)
	}
	if len(entries) != len(want) {
		t.Errorf("fixture dir has %d files, contract has %d", len(entries), len(want))
	}
	for n, b := range want {
		got, err := os.ReadFile(filepath.Join(idemRetentionFixtureDir, n))
		if err != nil || !bytes.Equal(got, b) {
			t.Errorf("%s drifted from the wire types: %v", n, err)
		}
	}
	var e APIError
	if err := jsonStrict(want["api-error-idem-expired.json"], &e); err != nil || e.Code != CodeIdemExpired {
		t.Fatalf("fixture: %v %+v", err, e)
	}
}

// spentEntryBytes is the measured JSON cost of one tombstone, for the docs.
func TestSpentEntrySize(t *testing.T) {
	one, _ := json.Marshal(map[string]string{spentKey("a"): spentBody(actionHash("a1", ActionRequest{Verb: VerbPrompt}))})
	two, _ := json.Marshal(map[string]string{spentKey("a"): "0123456789abcdef", spentKey("b"): "0123456789abcdef"})
	if per := len(two) - len(one); per > 40 {
		t.Fatalf("a tombstone costs %d bytes of JSON", per)
	}
}
