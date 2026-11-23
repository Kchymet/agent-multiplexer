package direct

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

func scopeOf(groups ...string) *ResourceScope {
	if len(groups) == 0 {
		return &ResourceScope{Kind: ResourceAllSessions}
	}
	return &ResourceScope{Kind: ResourceWorkgroups, Workgroups: groups}
}

func capsOf(t *testing.T, s http.Handler, token string) CapabilitiesResponse {
	t.Helper()
	w := do(s, "GET", PathCapabilities, token, "")
	if w.Code != 200 {
		t.Fatalf("capabilities: %d %s", w.Code, w.Body)
	}
	var c CapabilitiesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestUpdateGrantWidensWithoutReenrollment(t *testing.T) {
	f := newFake(testRows()...)
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	s := newTestServer(t, a, f)
	tok := enroll(t, a, []string{PermRead}, allSessions)
	if w := do(s, "POST", PathSession+"/a1/actions", tok.AccessToken, actionBody(VerbPrompt, "x", "k1")); w.Code != 403 {
		t.Fatalf("prompt before widening: %d %s", w.Code, w.Body)
	}
	c := capsOf(t, s, tok.AccessToken)
	if c.Grant == nil || c.Grant.ClientID != tok.ClientID || !slices.Equal(c.Grant.Permissions, []string{PermRead}) || c.Grant.UpdatedAt != 0 {
		t.Fatalf("grant before: %+v", c.Grant)
	}
	res, err := a.UpdateGrant(GrantUpdate{ClientID: tok.ClientID, Permissions: []string{PermRead, PermSteer}}, "test")
	if err != nil || !res.Changed || res.Narrowed || res.CutOperations != 0 {
		t.Fatalf("widen: %+v %v", res, err)
	}
	// The same access token, the same client: the new right applies to the
	// next request, with no re-enrollment and no refresh.
	if w := do(s, "POST", PathSession+"/a1/actions", tok.AccessToken, actionBody(VerbPrompt, "x", "k2")); w.Code != 200 {
		t.Fatalf("prompt after widening: %d %s", w.Code, w.Body)
	}
	c = capsOf(t, s, tok.AccessToken)
	if c.Grant == nil || !slices.Equal(c.Grant.Permissions, []string{PermRead, PermSteer}) || c.Grant.UpdatedAt == 0 ||
		!slices.Equal(c.Permissions, []string{PermRead, PermSteer}) || !slices.Contains(c.Verbs, VerbPrompt) {
		t.Fatalf("capabilities after widening: %+v", c)
	}
	// The refresh keeps the identity and reports the new scope.
	next, err := a.Refresh(tok.RefreshToken)
	if err != nil || next.ClientID != tok.ClientID || next.Scope != "read steer" {
		t.Fatalf("refresh: %+v %v", next, err)
	}
}

func TestUpdateGrantNarrowingCutsLiveStreamBeforeReturning(t *testing.T) {
	f := newFake(testRows()...)
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	ts := httptest.NewServer(newTestServer(t, a, f))
	t.Cleanup(ts.Close)
	tok := enroll(t, a, []string{PermRead, PermSteer}, allSessions)
	other := enroll(t, a, []string{PermRead}, allSessions)
	events := sse(t, openStream(t, ts, tok.AccessToken, PathSession+"/d1/stream"))
	otherEvents := sse(t, openStream(t, ts, other.AccessToken, PathSession+"/d1/stream"))
	<-f.streamStarted
	<-f.streamStarted

	res, err := a.UpdateGrant(GrantUpdate{ClientID: tok.ClientID, Resource: scopeOf("wg1")}, "test")
	if err != nil || !res.Narrowed || res.CutOperations != 1 {
		t.Fatalf("narrow: %+v %v", res, err)
	}
	// By the time UpdateGrant returned, the stream had ended with
	// auth.grant_changed (not auth.revoked: the credential is still good).
	if e := nextEvent(t, events); e[0] != "auth" || !strings.Contains(e[1], CodeGrantChanged) {
		t.Fatalf("terminal event %v", e)
	}
	if _, ok := <-events; ok {
		t.Fatal("stream continued after the narrowing")
	}
	// Another client's stream is untouched.
	f.feed("d1") <- hp.RuntimeEventBatch{Seq: 9, Events: []hp.RuntimeEvent{{Type: hp.TypeText}}}
	if e := nextEvent(t, otherEvents); e[0] != "events" {
		t.Fatalf("other stream: %v", e)
	}
	// Reconnecting with the same token: the dropped session is gone, the kept
	// workgroup still streams.
	s := newTestServer(t, a, f)
	if w := do(s, "GET", PathSession+"/d1/history", tok.AccessToken, ""); w.Code != 404 {
		t.Fatalf("out-of-scope history after narrowing: %d %s", w.Code, w.Body)
	}
	if w := do(s, "GET", PathSession+"/a1/history", tok.AccessToken, ""); w.Code != 200 {
		t.Fatalf("in-scope history after narrowing: %d %s", w.Code, w.Body)
	}
	// Dropping steer: the next prompt is denied at admission.
	if _, err := a.UpdateGrant(GrantUpdate{ClientID: tok.ClientID, Permissions: []string{PermRead}}, "test"); err != nil {
		t.Fatal(err)
	}
	if w := do(s, "POST", PathSession+"/a1/actions", tok.AccessToken, actionBody(VerbPrompt, "x", "k")); w.Code != 403 || apiCode(t, w) != CodeDenied {
		t.Fatalf("prompt after dropping steer: %d %s", w.Code, w.Body)
	}
	if f.applies.Load() != 0 {
		t.Fatal("a dropped right reached the daemon")
	}
}

func TestUpdateGrantNarrowingCutsInFlightCallOfDroppedRight(t *testing.T) {
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	tok := enroll(t, a, []string{PermRead, PermSteer, PermApprove}, allSessions)
	steer, err := a.Admit(context.Background(), tok.AccessToken, PermSteer)
	if err != nil {
		t.Fatal(err)
	}
	approve, err := a.Admit(context.Background(), tok.AccessToken, PermApprove)
	if err != nil {
		t.Fatal(err)
	}
	defer approve.Done()
	ended := make(chan struct{})
	go func() {
		<-steer.Context().Done()
		time.Sleep(50 * time.Millisecond) // the barrier waits for Done
		close(ended)
		steer.Done()
	}()
	res, err := a.UpdateGrant(GrantUpdate{ClientID: tok.ClientID, Permissions: []string{PermRead, PermApprove}}, "test")
	if err != nil || res.CutOperations != 1 {
		t.Fatalf("narrow: %+v %v", res, err)
	}
	select {
	case <-ended:
	default:
		t.Fatal("UpdateGrant returned before the cut call finished")
	}
	if steer.AuthEnded() != CodeGrantChanged || steer.Revoked() || steer.Expired() {
		t.Fatalf("cut lease: %q revoked=%v expired=%v", steer.AuthEnded(), steer.Revoked(), steer.Expired())
	}
	// A call under a right that was kept, with the scope unchanged, runs on.
	if approve.Context().Err() != nil {
		t.Fatal("a kept right's in-flight call was cut")
	}
}

func TestUpdateGrantWideningLeavesStreamsOpen(t *testing.T) {
	f := newFake(testRows()...)
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	ts := httptest.NewServer(newTestServer(t, a, f))
	t.Cleanup(ts.Close)
	tok := enroll(t, a, []string{PermRead}, ResourceScope{Kind: ResourceWorkgroups, Workgroups: []string{"wg1"}})
	events := sse(t, openStream(t, ts, tok.AccessToken, PathSession+"/a1/stream"))
	<-f.streamStarted
	res, err := a.UpdateGrant(GrantUpdate{ClientID: tok.ClientID, Permissions: []string{PermRead, PermSteer}, Resource: scopeOf()}, "test")
	if err != nil || res.Narrowed || res.CutOperations != 0 {
		t.Fatalf("widen: %+v %v", res, err)
	}
	f.feed("a1") <- hp.RuntimeEventBatch{Seq: 4, Events: []hp.RuntimeEvent{{Type: hp.TypeText}}}
	if e := nextEvent(t, events); e[0] != "events" {
		t.Fatalf("stream after widening: %v", e)
	}
}

func TestUpdateGrantNarrowingClosesWatchForReconnect(t *testing.T) {
	f := newFake(testRows()...)
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	srv := newTestServer(t, a, f)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Host = testHost
		srv.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	tok := enroll(t, a, []string{PermRead}, allSessions)
	conn := dialWatch(t, ts, testOrigin)
	_ = conn.WriteJSON(WatchAuth{Type: "auth", AccessToken: tok.AccessToken})
	var welcome WatchWelcome
	if err := conn.ReadJSON(&welcome); err != nil {
		t.Fatal(err)
	}
	var snap WatchSnapshot
	if err := conn.ReadJSON(&snap); err != nil || len(snap.Sessions) != 5 {
		t.Fatalf("snapshot: %v %d", err, len(snap.Sessions))
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		_, _ = a.UpdateGrant(GrantUpdate{ClientID: tok.ClientID, Resource: scopeOf("wg1")}, "test")
	}()
	// 4002 (renew and reconnect, which every client already does) with the
	// grant_changed reason.
	if code, reason := closeCode(t, conn); code != CloseAuthExpired || reason != CodeGrantChanged {
		t.Fatalf("close: %d %s", code, reason)
	}
	conn = dialWatch(t, ts, testOrigin)
	_ = conn.WriteJSON(WatchAuth{Type: "auth", AccessToken: tok.AccessToken})
	if err := conn.ReadJSON(&welcome); err != nil {
		t.Fatal(err)
	}
	if err := conn.ReadJSON(&snap); err != nil || len(snap.Sessions) != 2 {
		t.Fatalf("snapshot after narrowing: %v %+v", err, snap.Sessions)
	}
}

func TestUpdateGrantScopeChanges(t *testing.T) {
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	tok := enroll(t, a, []string{PermRead, PermSteer, PermCreate, PermAccounts}, allSessions)
	// Narrowing to workgroups drops the inherited all-sessions-only rights.
	res, err := a.UpdateGrant(GrantUpdate{ClientID: tok.ClientID, Resource: scopeOf("wg2", "wg1", "wg1")}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Client.Permissions, []string{PermRead, PermSteer}) || !slices.Equal(res.Dropped, []string{PermCreate, PermAccounts}) ||
		res.Client.ResourceScope.Kind != ResourceWorkgroups || !slices.Equal(res.Client.ResourceScope.Workgroups, []string{"wg1", "wg2"}) || !res.Narrowed {
		t.Fatalf("to workgroups: %+v", res)
	}
	// Asking for create over workgroups is refused, and changes nothing.
	if _, err := a.UpdateGrant(GrantUpdate{ClientID: tok.ClientID, Permissions: []string{PermRead, PermCreate}}, "test"); err == nil || !strings.Contains(err.Error(), "all_sessions") {
		t.Fatalf("create over workgroups: %v", err)
	}
	// Workgroups -> all sessions, with create back: a pure widening.
	res, err = a.UpdateGrant(GrantUpdate{ClientID: tok.ClientID, Permissions: []string{PermRead, PermCreate}, Resource: scopeOf()}, "test")
	if err != nil || res.Narrowed != true || res.Client.ResourceScope.Kind != ResourceAllSessions {
		// steer was dropped, so this is (also) a narrowing
		t.Fatalf("to all sessions: %+v %v", res, err)
	}
	// Removing one workgroup of two is a narrowing; adding one is not.
	if _, err := a.UpdateGrant(GrantUpdate{ClientID: tok.ClientID, Permissions: []string{PermRead}, Resource: scopeOf("wg1", "wg2")}, "test"); err != nil {
		t.Fatal(err)
	}
	if res, _ := a.UpdateGrant(GrantUpdate{ClientID: tok.ClientID, Resource: scopeOf("wg1", "wg2", "d1")}, "test"); res.Narrowed {
		t.Fatal("adding a workgroup counted as narrowing")
	}
	if res, _ := a.UpdateGrant(GrantUpdate{ClientID: tok.ClientID, Resource: scopeOf("wg1")}, "test"); !res.Narrowed {
		t.Fatal("removing workgroups did not count as narrowing")
	}
	// Invalid requests.
	for _, u := range []GrantUpdate{
		{ClientID: tok.ClientID},
		{ClientID: tok.ClientID, Permissions: []string{PermSteer}},
		{ClientID: tok.ClientID, Permissions: []string{PermRead, "root"}},
		{ClientID: tok.ClientID, Resource: scopeOf(ConsoleID)},
		{ClientID: "c_nope", Permissions: []string{PermRead}},
	} {
		if _, err := a.UpdateGrant(u, "test"); err == nil {
			t.Fatalf("accepted %+v", u)
		}
	}
	if err := a.Revoke(tok.ClientID, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.UpdateGrant(GrantUpdate{ClientID: tok.ClientID, Permissions: []string{PermRead, PermSteer}}, "test"); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("update of a revoked grant: %v", err)
	}
}

func TestUpdateGrantIsAuditedListedAndDurable(t *testing.T) {
	root := t.TempDir()
	a, err := OpenAuthority(root, Lifetimes{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tok := enroll(t, a, []string{PermRead}, allSessions)
	// A request equal to the grant is a no-op: nothing written, nothing audited.
	if res, err := a.UpdateGrant(GrantUpdate{ClientID: tok.ClientID, Permissions: []string{PermRead}}, "cli:op"); err != nil || res.Changed {
		t.Fatalf("no-op: %+v %v", res, err)
	}
	if _, err := a.UpdateGrant(GrantUpdate{ClientID: tok.ClientID, Permissions: []string{PermRead, PermSteer}}, "cli:op"); err != nil {
		t.Fatal(err)
	}
	recs, err := a.AuditRecords(tok.ClientID, 0)
	if err != nil || len(recs) != 1 {
		t.Fatalf("audit: %+v %v", recs, err)
	}
	r := recs[0]
	if r.Route != AuditRouteGrant || r.Verb != "update" || r.Target != tok.ClientID || r.Outcome != "ok" ||
		r.Text != "read all_sessions -> read,steer all_sessions" || r.Note != "host:cli:op" || r.At == 0 {
		t.Fatalf("audit record: %+v", r)
	}
	a.Close()
	// Durable, and visible in the operator's list after a restart.
	a = openTestAuthority(t, root, Lifetimes{}, nil)
	var info ClientInfo
	for _, c := range a.List() {
		if c.ClientID == tok.ClientID {
			info = c
		}
	}
	if !slices.Equal(info.Permissions, []string{PermRead, PermSteer}) || info.UpdatedAt == 0 || len(info.Changes) != 1 {
		t.Fatalf("listed: %+v", info)
	}
	ch := info.Changes[0]
	if ch.Source != GrantSourceHost || ch.By != "cli:op" || !slices.Equal(ch.OldPerms, []string{PermRead}) || !slices.Equal(ch.NewPerms, []string{PermRead, PermSteer}) {
		t.Fatalf("change: %+v", ch)
	}
	// Bounded history.
	for i := 0; i < maxGrantChanges+5; i++ {
		perms := []string{PermRead}
		if i%2 == 0 {
			perms = append(perms, PermRespond)
		}
		if _, err := a.UpdateGrant(GrantUpdate{ClientID: tok.ClientID, Permissions: perms}, "x"); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(a.List()[0].Changes); n != maxGrantChanges {
		t.Fatalf("kept %d changes", n)
	}
}

func TestUpdateGrantRollsBackAnUncommittedChange(t *testing.T) {
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	tok := enroll(t, a, []string{PermRead}, allSessions)
	write := a.st.write
	a.st.write = func(dir, name string, data []byte) error { return errNotCommitted }
	if _, err := a.UpdateGrant(GrantUpdate{ClientID: tok.ClientID, Permissions: []string{PermRead, PermSteer}}, "x"); err == nil {
		t.Fatal("uncommitted update reported success")
	}
	a.st.write = write
	if _, err := a.Admit(context.Background(), tok.AccessToken, PermSteer); err != errDenied {
		t.Fatalf("an unsaved widening took effect: %v", err)
	}
}

func TestAdminUpdateOperation(t *testing.T) {
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	tok := enroll(t, a, []string{PermRead}, allSessions)
	resp := Administer(a, nil, AdminRequest{Op: OpUpdate, Update: &GrantUpdate{ClientID: tok.ClientID, Permissions: []string{PermRead, PermApprove}}, Actor: "tui"})
	if !resp.OK || resp.Update == nil || !resp.Update.Changed || !slices.Equal(resp.Update.Client.Permissions, []string{PermRead, PermApprove}) {
		t.Fatalf("admin update: %+v", resp)
	}
	if resp := Administer(a, nil, AdminRequest{Op: OpUpdate}); resp.OK {
		t.Fatal("update without a change accepted")
	}
}

func cloudPolicyFor(t *testing.T, a *Authority, perms []string, res ResourceScope) {
	t.Helper()
	if _, err := a.EnableCloud(CloudPolicyRequest{Permissions: perms, Resource: res, OwnerPin: "owner-1", ProviderPin: "prov-1"}); err != nil {
		t.Fatal(err)
	}
}

func TestCloudRaiseGrantStaysWithinTheCeiling(t *testing.T) {
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	tok := enroll(t, a, []string{PermRead}, ResourceScope{Kind: ResourceWorkgroups, Workgroups: []string{"wg1"}})
	code := func(err error) string {
		if ce, ok := err.(*CloudError); ok {
			return ce.Code
		}
		return ""
	}
	if _, err := a.CloudRaiseGrant(tok.ClientID, []string{PermSteer}, nil, "owner-1"); code(err) != CloudErrDisabled {
		t.Fatalf("raise with cloud disabled: %v", err)
	}
	cloudPolicyFor(t, a, []string{PermRead, PermSteer}, allSessions)
	if _, err := a.CloudRaiseGrant(tok.ClientID, []string{PermSteer}, nil, "someone-else"); code(err) != CloudErrOwnerMismatch {
		t.Fatalf("raise by another requester: %v", err)
	}
	if _, err := a.CloudRaiseGrant(tok.ClientID, []string{PermApprove}, nil, "owner-1"); code(err) != CloudErrScope {
		t.Fatalf("raise beyond the ceiling: %v", err)
	}
	res, err := a.CloudRaiseGrant(tok.ClientID, []string{PermSteer}, scopeOf(), "owner-1")
	if err != nil || !slices.Equal(res.Client.Permissions, []string{PermRead, PermSteer}) || res.Client.ResourceScope.Kind != ResourceAllSessions || res.Narrowed {
		t.Fatalf("raise within the ceiling: %+v %v", res, err)
	}
	ch := res.Client.Changes[len(res.Client.Changes)-1]
	if ch.Source != GrantSourceCloud || ch.By != "owner-1" {
		t.Fatalf("change record: %+v", ch)
	}
	// The cloud can never narrow: a smaller scope is refused.
	if _, err := a.CloudRaiseGrant(tok.ClientID, nil, scopeOf("wg1"), "owner-1"); code(err) != CloudErrScope {
		t.Fatalf("cloud narrowing: %v", err)
	}
	// Rights the host granted beyond the ceiling are kept by a raise.
	if _, err := a.UpdateGrant(GrantUpdate{ClientID: tok.ClientID, Permissions: []string{PermRead, PermManage}}, "host"); err != nil {
		t.Fatal(err)
	}
	res, err = a.CloudRaiseGrant(tok.ClientID, []string{PermSteer}, nil, "owner-1")
	if err != nil || !slices.Equal(res.Client.Permissions, []string{PermRead, PermSteer, PermManage}) {
		t.Fatalf("raise keeps host rights: %+v %v", res, err)
	}
	recs, _ := a.AuditRecords(tok.ClientID, 0)
	if len(recs) == 0 || recs[0].Verb != "cloud_raise" || recs[0].Note != "cloud:owner-1" {
		t.Fatalf("audit: %+v", recs)
	}
}

func redeemAs(t *testing.T, a *Authority, opLabel string, replace bool, clientLabel, kind, device string) TokenResponse {
	t.Helper()
	code, _, err := a.newCode(opLabel, []string{PermRead}, allSessions, 0, false, replace)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := a.RedeemDevice(code, clientLabel, kind, "", device)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func grantState(a *Authority, id string) ClientInfo {
	for _, c := range a.List() {
		if c.ClientID == id {
			return c
		}
	}
	return ClientInfo{}
}

func TestEnrollReplaceRevokesThePriorClientOnTheSameDevice(t *testing.T) {
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, c.now)
	old := redeemAs(t, a, "phone", false, "Pixel", ClientAndroid, "dev-1")
	c.add(time.Second)
	otherDevice := redeemAs(t, a, "phone", false, "Pixel", ClientAndroid, "dev-2")
	c.add(time.Second)
	otherLabel := redeemAs(t, a, "tablet", false, "Pixel", ClientAndroid, "dev-1")
	c.add(time.Second)
	lease, err := a.Admit(context.Background(), old.AccessToken, PermRead)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Done()
	next := redeemAs(t, a, "phone", true, "Pixel", ClientAndroid, "dev-1")
	st := grantState(a, old.ClientID)
	if st.RevokedAt == 0 || st.ReplacedBy != next.ClientID {
		t.Fatalf("prior client: %+v", st)
	}
	if !lease.Revoked() {
		t.Fatal("the replaced client's open work was not ended")
	}
	if _, err := a.Refresh(old.RefreshToken); err == nil {
		t.Fatal("the replaced client can still refresh")
	}
	for _, keep := range []TokenResponse{otherDevice, otherLabel, next} {
		if grantState(a, keep.ClientID).RevokedAt != 0 {
			t.Fatalf("%s was revoked", keep.ClientID)
		}
	}
	recs, _ := a.AuditRecords(old.ClientID, 0)
	if len(recs) != 1 || recs[0].Verb != "replace" {
		t.Fatalf("audit: %+v", recs)
	}
	// Without a device id on either side, the device is the client kind and
	// the client's own label.
	w1 := redeemAs(t, a, "laptop", false, "Chrome on Mac", ClientWeb, "")
	c.add(time.Second)
	w2 := redeemAs(t, a, "laptop", true, "Chrome on Mac", ClientWeb, "")
	if grantState(a, w1.ClientID).ReplacedBy != w2.ClientID {
		t.Fatal("same kind and client label was not treated as the same device")
	}
}

func TestStaleClients(t *testing.T) {
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, c.now)
	first := redeemAs(t, a, "phone", false, "Pixel", ClientAndroid, "dev-1")
	c.add(time.Hour)
	second := redeemAs(t, a, "phone", false, "Pixel", ClientAndroid, "dev-1")
	c.add(time.Hour)
	idle := redeemAs(t, a, "laptop", false, "Chrome", ClientWeb, "")
	c.add(20 * 24 * time.Hour)
	if _, err := a.Refresh(second.RefreshToken); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, ci := range a.Stale(0) {
		got[ci.ClientID] = ci.Stale
	}
	if got[first.ClientID] != "superseded by "+second.ClientID || !strings.HasPrefix(got[idle.ClientID], "not seen for 20 days") || got[second.ClientID] != "" || len(got) != 2 {
		t.Fatalf("stale: %v", got)
	}
	if n := len(a.Stale(30)); n != 1 {
		t.Fatalf("stale with a 30-day threshold: %d", n)
	}
	resp := Administer(a, nil, AdminRequest{Op: OpList, Stale: true})
	if !resp.OK || len(resp.Clients) != 2 || resp.Clients[0].Stale == "" {
		t.Fatalf("admin stale list: %+v", resp)
	}
}

// Old clients send no device_id and ignore the capabilities grant object; new
// clients' device_id is ignored by older servers (unknown form fields are).
func TestGrantProtocolCompatibility(t *testing.T) {
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	s := newTestServer(t, a, newFake(testRows()...))
	for i, device := range []string{"", "install-123"} {
		code, _, err := a.NewCode("phone", []string{PermRead}, allSessions, 0)
		if err != nil {
			t.Fatal(err)
		}
		v := url.Values{"grant_type": {GrantTypeEnrollmentCode}, "code": {code}, "client_label": {"Pixel"}, "client_kind": {ClientAndroid}}
		if device != "" {
			v.Set("device_id", device)
		}
		w := do(s, "POST", PathToken, "", form(v), "Content-Type", formCT)
		if w.Code != 200 {
			t.Fatalf("redeem %d: %d %s", i, w.Code, w.Body)
		}
		var tok TokenResponse
		_ = json.Unmarshal(w.Body.Bytes(), &tok)
		// An older client's view of capabilities: the fields it always had.
		var old struct {
			Permissions   []string      `json:"permissions"`
			ResourceScope ResourceScope `json:"resource_scope"`
		}
		cw := do(s, "GET", PathCapabilities, tok.AccessToken, "")
		if err := json.Unmarshal(cw.Body.Bytes(), &old); err != nil || !slices.Equal(old.Permissions, []string{PermRead}) || old.ResourceScope.Kind != ResourceAllSessions {
			t.Fatalf("old capabilities view: %+v %v", old, err)
		}
	}
	code, _, _ := a.NewCode("phone", []string{PermRead}, allSessions, 0)
	v := url.Values{"grant_type": {GrantTypeEnrollmentCode}, "code": {code}, "client_kind": {ClientAndroid}, "device_id": {strings.Repeat("x", MaxLabelLen+1)}}
	if w := do(s, "POST", PathToken, "", form(v), "Content-Type", formCT); w.Code != 400 {
		t.Fatalf("oversized device_id: %d %s", w.Code, w.Body)
	}
}

func TestExecuteAliasExpandsToApproveEverywhere(t *testing.T) {
	got, err := ValidatePerms([]string{"read", " execute ", "steer"})
	if err != nil || !slices.Equal(got, []string{PermRead, PermSteer, PermApprove}) {
		t.Fatalf("ValidatePerms: %v %v", got, err)
	}
	if slices.Contains(Permissions, PermExecute) {
		t.Fatal("execute must stay an alias, never a stored permission")
	}
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	// Enrollment codes store the concrete names.
	tok := enroll(t, a, []string{PermRead, PermExecute}, allSessions)
	if tok.Scope != "read approve" {
		t.Fatalf("enrolled scope %q", tok.Scope)
	}
	// Updates accept the alias.
	res, err := a.UpdateGrant(GrantUpdate{ClientID: tok.ClientID, Permissions: []string{PermRead, PermSteer, PermExecute}}, "t")
	if err != nil || !slices.Equal(res.Client.Permissions, DefaultPermissions) {
		t.Fatalf("update: %+v %v", res, err)
	}
	// The cloud ceiling: the alias, and the default when none is named.
	st, err := a.EnableCloud(CloudPolicyRequest{Permissions: []string{PermRead, PermExecute}, Resource: allSessions, OwnerPin: "o", ProviderPin: "p"})
	if err != nil || !slices.Equal(st.Permissions, []string{PermRead, PermApprove}) {
		t.Fatalf("ceiling with the alias: %+v %v", st, err)
	}
	st, err = a.EnableCloud(CloudPolicyRequest{Resource: allSessions, OwnerPin: "o", ProviderPin: "p"})
	if err != nil || !slices.Equal(st.Permissions, []string{PermRead, PermSteer, PermApprove}) || st.Resource.Kind != ResourceAllSessions {
		t.Fatalf("default ceiling: %+v %v", st, err)
	}
	// The cloud raise accepts it too, within the ceiling.
	tok2 := enroll(t, a, []string{PermRead}, allSessions)
	if res, err := a.CloudRaiseGrant(tok2.ClientID, []string{PermExecute}, nil, "o"); err != nil || !slices.Equal(res.Client.Permissions, []string{PermRead, PermApprove}) {
		t.Fatalf("cloud raise with the alias: %+v %v", res, err)
	}
}

func TestDefaultPermissionsAreReadSteerAndExecute(t *testing.T) {
	if !slices.Equal(DefaultPermissions, []string{PermRead, PermSteer, PermApprove}) {
		t.Fatalf("defaults %v", DefaultPermissions)
	}
	for _, p := range []string{PermManage, PermCreate, PermAccounts, PermRespond} {
		if slices.Contains(DefaultPermissions, p) {
			t.Fatalf("%s must stay opt-in", p)
		}
	}
}
