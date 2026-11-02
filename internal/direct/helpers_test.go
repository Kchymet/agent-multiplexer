package direct

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"amux/internal/core"
	"amux/internal/hostbackend"
	amuxstore "amux/internal/store"
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

const testEndpoint = "https://amux.test:8443"
const testHost = "amux.test:8443"
const testOrigin = "https://harness.example"

// fakeBackend counts every elevated call. Blocking modes simulate a stalled
// daemon that only returns when its context ends.
type fakeBackend struct {
	mu            sync.Mutex
	rows          []core.Session
	snapshots     atomic.Int64
	applies       atomic.Int64
	histories     atomic.Int64
	streams       atomic.Int64
	activeApplies atomic.Int64
	blockSnapshot atomic.Bool
	blockApply    atomic.Bool
	applied       chan core.Action
	batchMu       sync.Mutex
	batches       map[string]chan hp.RuntimeEventBatch // per session: streams never share a channel
	applyErr      error
	streamStarted chan struct{}
	snapBlocked   chan struct{} // receives when a blocking Snapshot has entered
	catalog       MachineCatalog
	catalogs      atomic.Int64
	newID         string // the id Apply reports (creation verbs)
	// gate, when set, holds Apply (after it counts as dispatched) until a value
	// is received or ctx ends: a slow creation the test releases explicitly.
	gate chan struct{}

	// Creation identity. incs holds each session's current incarnation; a
	// session in rows or created by Apply without one gets "inc-"+id.
	// noIdentity simulates a daemon without the protocol; identErr a failing
	// identity read; identReads counts SessionIdentities calls.
	incs       map[string]string
	parents    map[string]string
	noIdentity atomic.Bool
	identErr   error
	identReads atomic.Int64
	lastExpect string
	// revokeOnRead runs (once) inside SessionIdentities, then the read fails.
	revokeOnRead func()
}

// setInc replaces (or with "" deletes) a session's current incarnation.
func (f *fakeBackend) setInc(id, inc string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.incs == nil {
		f.incs = map[string]string{}
	}
	if inc == "" {
		f.incs[id] = "-"
		return
	}
	f.incs[id] = inc
}

func (f *fakeBackend) incLocked(id string) (string, bool) {
	if inc, ok := f.incs[id]; ok {
		return inc, inc != "-"
	}
	for _, r := range f.rows {
		if r.ID == id {
			return "inc-" + id, true
		}
	}
	return "", false
}

func (f *fakeBackend) CreationIdentity(ctx context.Context) error {
	if f.noIdentity.Load() {
		return hostbackend.ErrUpgradeRequired
	}
	return ctx.Err()
}

func (f *fakeBackend) ApplyIdentity(ctx context.Context, a core.Action, expectRoot string) (amuxstore.Identity, error) {
	if f.noIdentity.Load() {
		return amuxstore.Identity{}, hostbackend.ErrUpgradeRequired
	}
	f.mu.Lock()
	f.lastExpect = expectRoot
	f.mu.Unlock()
	id, err := f.Apply(ctx, a)
	if err != nil || id == "" {
		return amuxstore.Identity{ID: id}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.incs == nil {
		f.incs = map[string]string{}
	}
	if f.parents == nil {
		f.parents = map[string]string{}
	}
	inc := "inc-" + id
	if cur, ok := f.incs[id]; ok && cur != "-" {
		inc = cur
	}
	f.incs[id] = inc
	if a.Action == core.ActionAddAgent {
		f.parents[id] = a.ID
		rootInc, _ := f.incLocked(a.ID)
		return amuxstore.Identity{ID: id, Incarnation: inc, RootID: a.ID, RootIncarnation: rootInc}, nil
	}
	return amuxstore.Identity{ID: id, Incarnation: inc, RootID: id, RootIncarnation: inc}, nil
}

func (f *fakeBackend) SessionIdentities(ctx context.Context, ids []string) (map[string]amuxstore.SessionIdentity, error) {
	f.identReads.Add(1)
	f.mu.Lock()
	revoke := f.revokeOnRead
	f.revokeOnRead = nil
	f.mu.Unlock()
	if revoke != nil {
		revoke()
		return nil, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.noIdentity.Load() {
		return nil, hostbackend.ErrUpgradeRequired
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.identErr != nil {
		return nil, f.identErr
	}
	out := map[string]amuxstore.SessionIdentity{}
	for _, id := range ids {
		inc, ok := f.incLocked(id)
		if !ok {
			continue
		}
		parent := f.parents[id]
		if parent == "" {
			for _, r := range f.rows {
				if r.ID == id {
					parent = r.RootID
				}
			}
		}
		out[id] = amuxstore.SessionIdentity{ID: id, Incarnation: inc, RootID: parent}
	}
	return out, nil
}

func (f *fakeBackend) Catalog(ctx context.Context) (MachineCatalog, error) {
	f.catalogs.Add(1)
	if err := ctx.Err(); err != nil {
		return MachineCatalog{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.catalog, nil
}

func newFake(rows ...core.Session) *fakeBackend {
	return &fakeBackend{rows: rows, applied: make(chan core.Action, 16), batches: map[string]chan hp.RuntimeEventBatch{}, streamStarted: make(chan struct{}, 4), snapBlocked: make(chan struct{}, 16)}
}

// feed returns the batch channel of one session's stream.
func (f *fakeBackend) feed(sid string) chan hp.RuntimeEventBatch {
	f.batchMu.Lock()
	defer f.batchMu.Unlock()
	if f.batches[sid] == nil {
		f.batches[sid] = make(chan hp.RuntimeEventBatch, 16)
	}
	return f.batches[sid]
}

func (f *fakeBackend) setRows(rows ...core.Session) {
	f.mu.Lock()
	f.rows = rows
	f.mu.Unlock()
}

func (f *fakeBackend) Snapshot(ctx context.Context) ([]core.Session, error) {
	f.snapshots.Add(1)
	if f.blockSnapshot.Load() {
		select {
		case f.snapBlocked <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]core.Session(nil), f.rows...), nil
}

func (f *fakeBackend) Apply(ctx context.Context, a core.Action) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err // like hostbackend: a cancelled call never dials
	}
	f.applies.Add(1)
	f.activeApplies.Add(1)
	defer f.activeApplies.Add(-1)
	if f.blockApply.Load() {
		<-ctx.Done()
		return "", ctx.Err()
	}
	f.mu.Lock()
	gate := f.gate
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	f.applied <- a
	f.mu.Lock()
	id := f.newID
	f.mu.Unlock()
	return id, f.applyErr
}

func (f *fakeBackend) History(ctx context.Context, sid string, before int64, limit int) (hp.RuntimeEventHistory, bool) {
	f.histories.Add(1)
	return hp.RuntimeEventHistory{Seq: 2, Head: 2, Runtime: "claude", Events: []hp.RuntimeEvent{{Type: hp.TypeText}}}, true
}

func (f *fakeBackend) Stream(ctx context.Context, sid string, after int64) (<-chan hp.RuntimeEventBatch, bool) {
	f.streams.Add(1)
	out := make(chan hp.RuntimeEventBatch)
	in := f.feed(sid)
	go func() {
		defer close(out)
		f.streamStarted <- struct{}{}
		for {
			select {
			case <-ctx.Done():
				return
			case b := <-in:
				select {
				case out <- b:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, true
}

func (f *fakeBackend) privileged() int64 {
	return f.applies.Load() + f.histories.Load() + f.streams.Load()
}

func (f *fakeBackend) total() int64 { return f.snapshots.Load() + f.privileged() }

// Inventory: workgroup wg1 (coordinator + member a1 with private metadata),
// workgroup wg2 (archived root + member b1), and a detached session.
func testRows() []core.Session {
	caps := &core.SessionCaps{Prompt: true, Interject: true, Cancel: true}
	return []core.Session{
		{ID: "wg1", Title: "one", IsRoot: true, Role: hp.RoleCoordinator, Section: hp.SectionWorkgroups, Caps: caps, Runtime: "claude"},
		{ID: "a1", Title: "member", RootID: "wg1", Cwd: "/home/secret/worktree", Pid: 4242, Caps: caps, Runtime: "claude",
			AccountBinding: &hp.AccountBinding{ProfileID: "claude:private"}},
		{ID: "wg2", Title: "two", IsRoot: true, Role: hp.RoleCoordinator, Archived: true, Section: hp.SectionArchived, Caps: caps},
		{ID: "b1", Title: "archived member", RootID: "wg2", Caps: caps, Runtime: "codex"},
		{ID: "d1", Title: "detached", Section: hp.SectionDetached, Caps: caps},
	}
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func openTestAuthority(t *testing.T, root string, life Lifetimes, now func() time.Time) *Authority {
	t.Helper()
	a, err := OpenAuthority(root, life, now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a
}

func enroll(t *testing.T, a *Authority, perms []string, res ResourceScope) TokenResponse {
	t.Helper()
	code, _, err := a.NewCode("test", perms, res, 0)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := a.RedeemCode(code, "phone", ClientAndroid)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

var allSessions = ResourceScope{Kind: ResourceAllSessions}

func newTestServer(t *testing.T, a *Authority, b Backend) *Server {
	t.Helper()
	s, err := NewServer(Config{Authority: a, Backend: b, Endpoints: []string{testEndpoint}, AllowOrigins: []string{testOrigin},
		WatchPoll: 20 * time.Millisecond, Revalidate: time.Hour, Keepalive: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// do sends one request through the handler (no network).
func do(s http.Handler, method, path, token string, body string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = testHost
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	if body != "" && r.Header.Get("Content-Type") == "" {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func form(v url.Values) string { return v.Encode() }

const formCT = "application/x-www-form-urlencoded"

func apiCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var e APIError
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	return e.Code
}

func actionBody(verb, text, key string) string {
	fields := map[string]string{}
	if text != "" {
		fields["text"] = text
	}
	b, _ := json.Marshal(ActionRequest{Verb: verb, Fields: fields, IdemKey: key})
	return string(b)
}
