package direct

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"amux/internal/access"
)

const (
	testOwner    = "user_owner"
	testProvider = "prov_7"
	testVerifier = "dBjftJeZ4CVP-mJ0kWbaCPaFfvAZ8tZ_xwvoGbZT4eNC-JqHT8JN2WtW"
	// S256(testVerifier), computed independently (Python hashlib).
	testChallenge = "1jDx-gnaFyRPFEUjjAI5rVpavjPS4tt83QRwl7h-atI"
)

var wg1 = ResourceScope{Kind: ResourceWorkgroups, Workgroups: []string{"wg1"}}

func cloudPolicyReq(perms []string, res ResourceScope) CloudPolicyRequest {
	return CloudPolicyRequest{Permissions: perms, Resource: res, OwnerPin: testOwner, ProviderPin: testProvider}
}

func issueReq(key string) CloudIssueRequest {
	return CloudIssueRequest{IssueKey: key, Requester: testOwner, ProviderID: testProvider, CodeChallenge: testChallenge,
		CodeChallengeMethod: CodeChallengeMethodS256, Permissions: []string{PermRead}, Resource: wg1, Label: "Pixel via cloud"}
}

func enabledAuthority(t *testing.T, c *clock) (*Authority, string) {
	t.Helper()
	root := t.TempDir()
	var now func() time.Time
	if c != nil {
		now = c.now
	}
	a := openTestAuthority(t, root, Lifetimes{}, now)
	if _, err := a.EnableCloud(cloudPolicyReq([]string{PermRead, PermSteer}, ResourceScope{Kind: ResourceWorkgroups, Workgroups: []string{"wg1", "wg2"}})); err != nil {
		t.Fatal(err)
	}
	return a, root
}

// countWrites counts store writes from now on.
func countWrites(a *Authority) *atomic.Int32 {
	var n atomic.Int32
	a.st.write = func(dir, name string, data []byte) error { n.Add(1); return writeAtomic(dir, name, data) }
	return &n
}

func cloudCode(t *testing.T, err error) string {
	t.Helper()
	var ce *CloudError
	if !errors.As(err, &ce) {
		t.Fatalf("not a cloud error: %v", err)
	}
	return ce.Code
}

func TestS256Challenge(t *testing.T) {
	if S256Challenge(testVerifier) != testChallenge {
		t.Fatal("S256 mismatch")
	}
	for v, ok := range map[string]bool{
		testVerifier: true, strings.Repeat("a", 43): true, strings.Repeat("a", 128): true, "A-._~" + strings.Repeat("0", 38): true,
		strings.Repeat("a", 42): false, strings.Repeat("a", 129): false, strings.Repeat("a", 42) + "+": false, strings.Repeat("a", 42) + "=": false,
	} {
		if ValidVerifier(v) != ok {
			t.Errorf("ValidVerifier(%q) != %v", v, ok)
		}
	}
}

func TestCloudIssuanceIsOffByDefault(t *testing.T) {
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	writes := countWrites(a)
	if _, _, err := a.IssueCloudCode(issueReq("k1")); cloudCode(t, err) != CloudErrDisabled {
		t.Fatalf("issue with no policy: %v", err)
	}
	if st := a.CloudStatus(); st.Enabled || st.Generation != 0 {
		t.Fatalf("default status %+v", st)
	}
	if writes.Load() != 0 {
		t.Fatal("refused issuance wrote")
	}
}

func TestCloudPolicyRequiresExplicitCeilingAndPins(t *testing.T) {
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	bad := []CloudPolicyRequest{
		{Permissions: []string{PermRead}, OwnerPin: testOwner, ProviderPin: testProvider},                                                                                     // no resource
		{Permissions: []string{PermRead}, Resource: ResourceScope{Kind: ResourceWorkgroups, Workgroups: []string{ConsoleID}}, OwnerPin: testOwner, ProviderPin: testProvider}, // console
		{Permissions: []string{"admin"}, Resource: allSessions, OwnerPin: testOwner, ProviderPin: testProvider},
		{Permissions: []string{PermRead}, Resource: allSessions, ProviderPin: testProvider},
		{Permissions: []string{PermRead}, Resource: allSessions, OwnerPin: testOwner},
		{Permissions: []string{PermRead}, Resource: allSessions, OwnerPin: "has space", ProviderPin: testProvider},
		{Permissions: []string{PermRead}, Resource: allSessions, OwnerPin: testOwner, ProviderPin: testProvider, MaxOutstanding: MaxCloudOutstanding + 1},
		{Permissions: []string{PermRead}, Resource: allSessions, OwnerPin: testOwner, ProviderPin: testProvider, MaxPerHour: -1},
	}
	writes := countWrites(a)
	for i, req := range bad {
		if _, err := a.EnableCloud(req); err == nil {
			t.Errorf("policy %d accepted", i)
		}
	}
	if writes.Load() != 0 || a.CloudStatus().Enabled {
		t.Fatal("a refused policy was stored")
	}
}

func TestCloudIssuanceRefusalsWriteNothing(t *testing.T) {
	a, root := enabledAuthority(t, nil)
	before, _ := os.ReadFile(filepath.Join(root, grantsFile))
	writes := countWrites(a)
	cases := map[string]struct {
		mut  func(*CloudIssueRequest)
		want string
	}{
		"wrong owner":        {func(r *CloudIssueRequest) { r.Requester = "someone_else" }, CloudErrOwnerMismatch},
		"wrong provider":     {func(r *CloudIssueRequest) { r.ProviderID = "prov_8" }, CloudErrProviderMismatch},
		"plain method":       {func(r *CloudIssueRequest) { r.CodeChallengeMethod = "plain" }, CloudErrInvalidChallenge},
		"short challenge":    {func(r *CloudIssueRequest) { r.CodeChallenge = testChallenge[:42] }, CloudErrInvalidChallenge},
		"padded challenge":   {func(r *CloudIssueRequest) { r.CodeChallenge = testChallenge + "=" }, CloudErrInvalidChallenge},
		"perms over ceiling": {func(r *CloudIssueRequest) { r.Permissions = []string{PermRead, "admin"} }, CloudErrInvalidRequest},
		"workgroup over": {func(r *CloudIssueRequest) {
			r.Resource = ResourceScope{Kind: ResourceWorkgroups, Workgroups: []string{"wg3"}}
		}, CloudErrScope},
		"all over workgroups": {func(r *CloudIssueRequest) { r.Resource = allSessions }, CloudErrScope},
		"console": {func(r *CloudIssueRequest) {
			r.Resource = ResourceScope{Kind: ResourceWorkgroups, Workgroups: []string{ConsoleID}}
		}, CloudErrInvalidRequest},
		"no resource":         {func(r *CloudIssueRequest) { r.Resource = ResourceScope{} }, CloudErrInvalidRequest},
		"no issue key":        {func(r *CloudIssueRequest) { r.IssueKey = "" }, CloudErrInvalidRequest},
		"no requester":        {func(r *CloudIssueRequest) { r.Requester = "" }, CloudErrInvalidRequest},
		"control in label":    {func(r *CloudIssueRequest) { r.Label = "a\nb" }, CloudErrInvalidRequest},
		"oversized issue key": {func(r *CloudIssueRequest) { r.IssueKey = strings.Repeat("k", MaxCloudIssueKeyLen+1) }, CloudErrInvalidRequest},
	}
	for name, c := range cases {
		req := issueReq("k-" + strings.ReplaceAll(name, " ", "-"))
		c.mut(&req)
		if _, _, err := a.IssueCloudCode(req); cloudCode(t, err) != c.want {
			t.Errorf("%s: %v, want %s", name, err, c.want)
		}
	}
	// read-only ceiling refuses steer.
	if _, err := a.EnableCloud(cloudPolicyReq([]string{PermRead}, wg1)); err != nil {
		t.Fatal(err)
	}
	before, _ = os.ReadFile(filepath.Join(root, grantsFile))
	writes.Store(0)
	req := issueReq("k-steer")
	req.Permissions = []string{PermRead, PermSteer}
	if _, _, err := a.IssueCloudCode(req); cloudCode(t, err) != CloudErrScope {
		t.Fatalf("steer over a read ceiling: %v", err)
	}
	if writes.Load() != 0 {
		t.Fatalf("refusals wrote %d times", writes.Load())
	}
	after, _ := os.ReadFile(filepath.Join(root, grantsFile))
	if !bytes.Equal(before, after) {
		t.Fatal("refusals changed the registry")
	}
}

func TestCloudIssuanceAllSessionsCeilingAdmitsOrdinaryScopes(t *testing.T) {
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	if _, err := a.EnableCloud(cloudPolicyReq([]string{PermRead}, allSessions)); err != nil {
		t.Fatal(err)
	}
	for i, res := range []ResourceScope{allSessions, wg1} {
		req := issueReq(string(rune('a' + i)))
		req.Resource = res
		if _, _, err := a.IssueCloudCode(req); err != nil {
			t.Fatalf("%+v: %v", res, err)
		}
	}
	req := issueReq("console")
	req.Resource = ResourceScope{Kind: ResourceWorkgroups, Workgroups: []string{ConsoleID}}
	if _, _, err := a.IssueCloudCode(req); err == nil {
		t.Fatal("console issued under all_sessions")
	}
}

func TestCloudIssuanceIdempotency(t *testing.T) {
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	a, _ := enabledAuthority(t, c)
	code, exp, err := a.IssueCloudCode(issueReq("k1"))
	if err != nil {
		t.Fatal(err)
	}
	if exp != c.now().Unix()+int64(CloudCodeTTL/time.Second) {
		t.Fatalf("ttl: %d", exp-c.now().Unix())
	}
	again, exp2, err := a.IssueCloudCode(issueReq("k1"))
	if err != nil || again != code || exp2 != exp {
		t.Fatalf("same-key replay: %v %v", err, again == code)
	}
	if st := a.CloudStatus(); st.Outstanding != 1 || st.IssuedLastHour != 1 {
		t.Fatalf("replay minted: %+v", st)
	}
	other := issueReq("k1")
	other.Permissions = []string{PermRead, PermSteer}
	if _, _, err := a.IssueCloudCode(other); cloudCode(t, err) != CloudErrIdemConflict {
		t.Fatalf("conflict: %v", err)
	}
	// Consumed: same key is refused, never reminted.
	if _, err := a.Redeem(code, "phone", ClientAndroid, testVerifier); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.IssueCloudCode(issueReq("k1")); cloudCode(t, err) != CloudErrIssueExpired {
		t.Fatalf("after consumption: %v", err)
	}
	// Expired: same.
	if _, _, err := a.IssueCloudCode(issueReq("k2")); err != nil {
		t.Fatal(err)
	}
	c.add(CloudCodeTTL)
	if _, _, err := a.IssueCloudCode(issueReq("k2")); cloudCode(t, err) != CloudErrIssueExpired {
		t.Fatalf("after expiry: %v", err)
	}
	// Replay material is gone from disk once consumed or expired.
	_, _, _ = a.IssueCloudCode(issueReq("k3")) // triggers a prune + commit
	raw, _ := os.ReadFile(filepath.Join(a.st.root, grantsFile))
	if bytes.Contains(raw, []byte(code)) {
		t.Fatal("consumed code still on disk")
	}
	if bytes.Contains(raw, []byte(testVerifier)) {
		t.Fatal("verifier on disk")
	}
}

func TestCloudIssuanceCaps(t *testing.T) {
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, c.now)
	p := cloudPolicyReq([]string{PermRead}, wg1)
	p.MaxOutstanding, p.MaxPerHour = 2, 3
	if _, err := a.EnableCloud(p); err != nil {
		t.Fatal(err)
	}
	var codes []string
	for _, k := range []string{"a", "b"} {
		code, _, err := a.IssueCloudCode(issueReq(k))
		if err != nil {
			t.Fatal(err)
		}
		codes = append(codes, code)
	}
	if _, _, err := a.IssueCloudCode(issueReq("c")); cloudCode(t, err) != CloudErrCapacity {
		t.Fatalf("outstanding cap: %v", err)
	}
	if _, err := a.Redeem(codes[0], "x", ClientWeb, testVerifier); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.IssueCloudCode(issueReq("c")); err != nil {
		t.Fatalf("after redeeming one: %v", err)
	}
	if _, err := a.Redeem(codes[1], "x", ClientWeb, testVerifier); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.IssueCloudCode(issueReq("d")); cloudCode(t, err) != CloudErrRateLimited {
		t.Fatalf("hourly cap: %v", err)
	}
	c.add(cloudRateWindow + time.Second)
	if _, _, err := a.IssueCloudCode(issueReq("d")); err != nil {
		t.Fatalf("after the window: %v", err)
	}
}

func TestCloudIssuanceAmbiguousCommitIsNotReminted(t *testing.T) {
	a, _ := enabledAuthority(t, nil)
	failingWrites(a, true)
	if _, _, err := a.IssueCloudCode(issueReq("k1")); cloudCode(t, err) != CloudErrUnavailable {
		t.Fatalf("ambiguous: %v", err)
	}
	a.st.write = writeAtomic
	if _, _, err := a.IssueCloudCode(issueReq("k1")); err == nil {
		t.Fatal("same key reminted after an ambiguous commit")
	}
	if _, _, err := a.IssueCloudCode(issueReq("k2")); err == nil {
		t.Fatal("issuance continued in a degraded authority")
	}
}

func TestCloudIssuanceUncommittedWriteRollsBack(t *testing.T) {
	a, _ := enabledAuthority(t, nil)
	failingWrites(a, false)
	if _, _, err := a.IssueCloudCode(issueReq("k1")); cloudCode(t, err) != CloudErrUnavailable {
		t.Fatalf("not committed: %v", err)
	}
	a.st.write = writeAtomic
	if st := a.CloudStatus(); st.Outstanding != 0 || st.IssuedLastHour != 0 {
		t.Fatalf("rolled-back issuance visible: %+v", st)
	}
	if _, _, err := a.IssueCloudCode(issueReq("k1")); err != nil {
		t.Fatalf("retry after an uncommitted write: %v", err)
	}
}

func TestCloudRedemptionPKCE(t *testing.T) {
	a, _ := enabledAuthority(t, nil)
	code, _, err := a.IssueCloudCode(issueReq("k1"))
	if err != nil {
		t.Fatal(err)
	}
	var reqErr *requestError
	if _, err := a.Redeem(code, "phone", ClientAndroid, ""); !errors.As(err, &reqErr) {
		t.Fatalf("missing verifier: %v", err)
	}
	if _, err := a.Redeem(code, "phone", ClientAndroid, "short"); !errors.As(err, &reqErr) {
		t.Fatalf("malformed verifier: %v", err)
	}
	if _, err := a.Redeem(code, "phone", ClientAndroid, testVerifier+"!"); !errors.As(err, &reqErr) {
		t.Fatalf("bad charset verifier: %v", err)
	}
	// Request errors left the code usable.
	tok, err := a.Redeem(code, "phone", ClientAndroid, testVerifier)
	if err != nil {
		t.Fatalf("correct verifier: %v", err)
	}
	if tok.Scope != PermRead || tok.ResourceScope.Kind != ResourceWorkgroups || tok.ResourceScope.Workgroups[0] != "wg1" {
		t.Fatalf("grant scope %+v", tok)
	}
	list := a.List()
	if len(list) != 1 || list[0].Origin != OriginCloud || list[0].CloudRequester != testOwner || list[0].Label != "phone" {
		t.Fatalf("provenance %+v", list)
	}

	// A wrong verifier consumes the code.
	code2, _, err := a.IssueCloudCode(issueReq("k2"))
	if err != nil {
		t.Fatal(err)
	}
	wrong := strings.Repeat("a", 43)
	if _, err := a.Redeem(code2, "x", ClientWeb, wrong); !errors.Is(err, errInvalidCode) {
		t.Fatalf("wrong verifier: %v", err)
	}
	if _, err := a.Redeem(code2, "x", ClientWeb, testVerifier); !errors.Is(err, errInvalidCode) {
		t.Fatalf("code survived a wrong verifier: %v", err)
	}
}

func TestCloudProvenanceSurvivesClientLabel(t *testing.T) {
	a, _ := enabledAuthority(t, nil)
	code, _, _ := a.IssueCloudCode(issueReq("k1"))
	if _, err := a.Redeem(code, "manual enrollment", ClientWeb, testVerifier); err != nil {
		t.Fatal(err)
	}
	if c := a.List()[0]; c.Origin != OriginCloud || c.CloudRequester != testOwner {
		t.Fatalf("label erased provenance: %+v", c)
	}
	manual := enroll(t, a, []string{PermRead}, allSessions)
	for _, c := range a.List() {
		if c.ClientID == manual.ClientID && c.Origin != "" {
			t.Fatalf("manual grant marked %q", c.Origin)
		}
	}
}

func TestManualCodeRefusesStrayVerifierWithoutConsuming(t *testing.T) {
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	code, _, _ := a.NewCode("x", []string{PermRead}, allSessions, 0)
	var reqErr *requestError
	if _, err := a.Redeem(code, "c", ClientWeb, testVerifier); !errors.As(err, &reqErr) {
		t.Fatalf("stray verifier: %v", err)
	}
	if _, err := a.RedeemCode(code, "c", ClientWeb); err != nil {
		t.Fatalf("manual code consumed by a request error: %v", err)
	}
}

func TestCloudPolicyChangesInvalidateOutstandingCodes(t *testing.T) {
	for name, change := range map[string]func(*Authority) error{
		"disable": func(a *Authority) error { _, err := a.DisableCloud("test"); return err },
		"lower ceiling": func(a *Authority) error {
			_, err := a.EnableCloud(cloudPolicyReq([]string{PermRead}, wg1))
			return err
		},
		"re-pin owner": func(a *Authority) error {
			p := cloudPolicyReq([]string{PermRead, PermSteer}, ResourceScope{Kind: ResourceWorkgroups, Workgroups: []string{"wg1", "wg2"}})
			p.OwnerPin = "new_owner"
			_, err := a.EnableCloud(p)
			return err
		},
		"identity reset": func(a *Authority) error { _, err := a.ResetIdentity(); return err },
	} {
		t.Run(name, func(t *testing.T) {
			a, _ := enabledAuthority(t, nil)
			code, _, err := a.IssueCloudCode(issueReq("k1"))
			if err != nil {
				t.Fatal(err)
			}
			redeemed, _, _ := a.IssueCloudCode(issueReq("k2"))
			tok, err := a.Redeem(redeemed, "kept", ClientWeb, testVerifier)
			if err != nil {
				t.Fatal(err)
			}
			gen := a.CloudStatus().Generation
			if err := change(a); err != nil {
				t.Fatal(err)
			}
			if a.CloudStatus().Generation <= gen {
				t.Fatal("generation not bumped")
			}
			if _, err := a.Redeem(code, "x", ClientWeb, testVerifier); !errors.Is(err, errInvalidCode) {
				t.Fatalf("outstanding code survived %s: %v", name, err)
			}
			if a.CloudStatus().Outstanding != 0 {
				t.Fatal("stale codes still counted")
			}
			if _, _, err := a.IssueCloudCode(issueReq("k1")); err == nil {
				t.Fatal("same key reissued after a policy change")
			}
			if name == "identity reset" {
				if a.CloudStatus().Enabled {
					t.Fatal("identity reset left cloud issuance enabled")
				}
				return
			}
			// Policy changes do not revoke grants already issued.
			if _, err := a.Refresh(tok.RefreshToken); err != nil {
				t.Fatalf("existing grant harmed by %s: %v", name, err)
			}
		})
	}
}

func TestCloudDisableRaceWithIssuance(t *testing.T) {
	a, _ := enabledAuthority(t, nil)
	var wg sync.WaitGroup
	codes := make(chan string, 64)
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := issueReq("race-" + string(rune('A'+i)))
			if code, _, err := a.IssueCloudCode(req); err == nil {
				codes <- code
			}
		}()
	}
	wg.Add(1)
	go func() { defer wg.Done(); _, _ = a.DisableCloud("race") }()
	wg.Wait()
	close(codes)
	for code := range codes {
		if _, err := a.Redeem(code, "x", ClientWeb, testVerifier); err == nil {
			t.Fatal("a code issued around a disable redeemed after it")
		}
	}
}

func TestCloudPolicyPersistsAcrossRestart(t *testing.T) {
	root := t.TempDir()
	a, err := OpenAuthority(root, Lifetimes{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.EnableCloud(cloudPolicyReq([]string{PermRead}, wg1)); err != nil {
		t.Fatal(err)
	}
	code, _, err := a.IssueCloudCode(issueReq("k1"))
	if err != nil {
		t.Fatal(err)
	}
	a.Close()
	b := openTestAuthority(t, root, Lifetimes{}, nil)
	again, _, err := b.IssueCloudCode(issueReq("k1"))
	if err != nil || again != code {
		t.Fatalf("replay after restart: %v", err)
	}
	if _, err := b.Redeem(code, "x", ClientWeb, testVerifier); err != nil {
		t.Fatalf("redeem after restart: %v", err)
	}
	if fi, _ := os.Stat(filepath.Join(root, grantsFile)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("grants mode %v", fi.Mode())
	}
}

func TestTokenEndpointPKCE(t *testing.T) {
	a, _ := enabledAuthority(t, nil)
	s := newTestServer(t, a, newFake())
	code, _, _ := a.IssueCloudCode(issueReq("k1"))
	post := func(v url.Values) (int, OAuthError) {
		w := do(s, http.MethodPost, PathToken, "", form(v), "Content-Type", formCT)
		var e OAuthError
		_ = json.Unmarshal(w.Body.Bytes(), &e)
		return w.Code, e
	}
	base := url.Values{"grant_type": {GrantTypeEnrollmentCode}, "code": {code}, "client_label": {"Pixel"}, "client_kind": {ClientAndroid}}
	if st, e := post(base); st != http.StatusBadRequest || e.Error != OAuthInvalidRequest {
		t.Fatalf("no verifier: %d %+v", st, e)
	}
	withV := url.Values{}
	for k, v := range base {
		withV[k] = v
	}
	withV.Set("code_verifier", testVerifier)
	w := do(s, http.MethodPost, PathToken, "", form(withV), "Content-Type", formCT)
	if w.Code != http.StatusOK {
		t.Fatalf("pkce redemption: %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), testVerifier) {
		t.Fatal("verifier echoed")
	}
	// Repeated code_verifier is refused by the form parser.
	code2, _, _ := a.IssueCloudCode(issueReq("k2"))
	body := form(url.Values{"grant_type": {GrantTypeEnrollmentCode}, "code": {code2}, "client_kind": {ClientWeb}}) +
		"&code_verifier=" + testVerifier + "&code_verifier=" + testVerifier
	if w := do(s, http.MethodPost, PathToken, "", body, "Content-Type", formCT); w.Code != http.StatusBadRequest {
		t.Fatalf("repeated verifier: %d", w.Code)
	}
}

func TestCloudAdminRequiresHostProof(t *testing.T) {
	h := newHostAuthority(t)
	root := shortRoot(t)
	a := startAdmin(t, root, h)
	ctx := context.Background()
	enable := AdminRequest{Op: OpCloudEnable, Cloud: &CloudPolicyRequest{Permissions: []string{PermRead}, Resource: wg1, OwnerPin: testOwner, ProviderPin: testProvider}}
	before, _ := os.ReadFile(filepath.Join(root, grantsFile))
	forged := h.session
	forged.Kind = access.SubjectHost
	issue := issueReq("k1")
	for _, req := range []AdminRequest{enable, {Op: OpCloudDisable}, {Op: OpCloudStatus}, {Op: OpIssueCloudCode, Issue: &issue}} {
		if _, err := AdminCall(ctx, root, forged, req); err == nil {
			t.Errorf("%s with a session credential succeeded", req.Op)
		}
		if _, err := AdminCall(ctx, root, h.session, req); err == nil {
			t.Errorf("%s with an unmodified session credential succeeded", req.Op)
		}
	}
	after, _ := os.ReadFile(filepath.Join(root, grantsFile))
	if !bytes.Equal(before, after) || a.CloudStatus().Enabled {
		t.Fatal("a refused caller changed the cloud policy")
	}

	resp, err := AdminCall(ctx, root, h.host, enable)
	if err != nil || resp.Cloud == nil || !resp.Cloud.Enabled {
		t.Fatalf("host enable: %v %+v", err, resp)
	}
	resp, err = AdminCall(ctx, root, h.host, AdminRequest{Op: OpIssueCloudCode, Issue: &issue})
	if err != nil {
		t.Fatal(err)
	}
	b, err := DecodeBundle(resp.Bundle, time.Now())
	if err != nil || b.ServerID != a.ServerID() || b.Endpoints[0] != testEndpoint {
		t.Fatalf("bundle %v %+v", err, b)
	}
	bad := issueReq("k2")
	bad.Requester = "intruder"
	resp, err = AdminCall(ctx, root, h.host, AdminRequest{Op: OpIssueCloudCode, Issue: &bad})
	if err == nil || resp.Code != CloudErrOwnerMismatch || resp.Bundle != "" {
		t.Fatalf("typed refusal: %v %+v", err, resp)
	}
	if _, err := AdminCall(ctx, root, h.host, AdminRequest{Op: OpCloudDisable}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Redeem(b.Code, "x", ClientWeb, testVerifier); err == nil {
		t.Fatal("code survived disable over the admin channel")
	}
}

func TestOfflineCloudAdmin(t *testing.T) {
	h := newHostAuthority(t)
	root := shortRoot(t)
	a, err := OpenAuthority(root, Lifetimes{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	a.Close()
	ctx := context.Background()
	enable := AdminRequest{Op: OpCloudEnable, Cloud: &CloudPolicyRequest{Permissions: []string{PermRead}, Resource: wg1, OwnerPin: testOwner, ProviderPin: testProvider}}
	if _, err := OfflineAdminister(ctx, root, h.host, h.verify, enable); err != nil {
		t.Fatal(err)
	}
	issue := issueReq("k1")
	if _, err := OfflineAdminister(ctx, root, h.host, h.verify, AdminRequest{Op: OpIssueCloudCode, Issue: &issue}); err == nil {
		t.Fatal("offline issuance succeeded")
	}
	resp, err := OfflineAdminister(ctx, root, h.host, h.verify, AdminRequest{Op: OpCloudStatus})
	if err != nil || !resp.Cloud.Enabled || resp.Cloud.OwnerPin != testOwner {
		t.Fatalf("offline status: %v %+v", err, resp.Cloud)
	}
}
