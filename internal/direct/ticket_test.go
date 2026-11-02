package direct

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Device tickets (CloudIssueRequest.Handoff = HandoffDeviceTicket): minted
// without a challenge, claimed at the host with the redeeming device's own S256
// challenge, then redeemed with that device's verifier.

func ticketReq(key string) CloudIssueRequest {
	r := issueReq(key)
	r.CodeChallenge, r.CodeChallengeMethod, r.Handoff = "", "", HandoffDeviceTicket
	return r
}

const otherVerifier = "otherVerifierotherVerifierotherVerifierotherVerifier1"

func TestTicketIssuanceShape(t *testing.T) {
	a, _ := enabledAuthority(t, nil)
	// A ticket request carrying a challenge is malformed, never a PKCE code.
	withChallenge := ticketReq("k0")
	withChallenge.CodeChallenge, withChallenge.CodeChallengeMethod = testChallenge, CodeChallengeMethodS256
	if _, _, err := a.IssueCloudCode(withChallenge); cloudCode(t, err) != CloudErrInvalidChallenge {
		t.Fatalf("ticket with challenge: %v", err)
	}
	unknown := ticketReq("k0")
	unknown.Handoff = "bearer"
	if _, _, err := a.IssueCloudCode(unknown); cloudCode(t, err) != CloudErrInvalidRequest {
		t.Fatalf("unknown handoff: %v", err)
	}
	// Ordinary issuance without handoff still requires S256.
	noPKCE := issueReq("k0")
	noPKCE.CodeChallenge, noPKCE.CodeChallengeMethod = "", ""
	if _, _, err := a.IssueCloudCode(noPKCE); cloudCode(t, err) != CloudErrInvalidChallenge {
		t.Fatalf("ordinary issuance without challenge: %v", err)
	}
	// Scope ceiling and pins apply to tickets exactly as to codes.
	over := ticketReq("k0")
	over.Resource = ResourceScope{Kind: ResourceAllSessions}
	if _, _, err := a.IssueCloudCode(over); cloudCode(t, err) != CloudErrScope {
		t.Fatalf("ticket above ceiling: %v", err)
	}
	pin := ticketReq("k0")
	pin.Requester = "someone_else"
	if _, _, err := a.IssueCloudCode(pin); cloudCode(t, err) != CloudErrOwnerMismatch {
		t.Fatalf("ticket owner pin: %v", err)
	}

	code, exp, err := a.IssueCloudCode(ticketReq("k1"))
	if err != nil {
		t.Fatal(err)
	}
	if exp-a.now().Unix() != int64(CloudCodeTTL/time.Second) {
		t.Fatalf("ticket ttl %d", exp-a.now().Unix())
	}
	// Same key, same request: the same ticket. Same key as a PKCE request: conflict.
	again, _, err := a.IssueCloudCode(ticketReq("k1"))
	if err != nil || again != code {
		t.Fatalf("idempotent ticket: %v", err)
	}
	if _, _, err := a.IssueCloudCode(issueReq("k1")); cloudCode(t, err) != CloudErrIdemConflict {
		t.Fatalf("ticket key reused for a PKCE code: %v", err)
	}
	if st := a.CloudStatus(); st.Outstanding != 1 {
		t.Fatalf("ticket not counted as outstanding: %+v", st)
	}
}

func TestTicketCannotRedeemUnclaimed(t *testing.T) {
	a, _ := enabledAuthority(t, nil)
	code, _, err := a.IssueCloudCode(ticketReq("k1"))
	if err != nil {
		t.Fatal(err)
	}
	var reqErr *requestError
	for _, v := range []string{"", testVerifier} {
		if _, err := a.Redeem(code, "phone", ClientAndroid, v); !errors.As(err, &reqErr) {
			t.Fatalf("unclaimed redeem with %q: %v", v, err)
		}
	}
	if _, err := a.RedeemCode(code, "phone", ClientAndroid); !errors.As(err, &reqErr) {
		t.Fatalf("unclaimed manual-style redeem: %v", err)
	}
	if len(a.List()) != 0 {
		t.Fatal("unclaimed ticket produced a grant")
	}
	// The refusals left it usable for the device that claims it.
	if _, err := a.ClaimTicket(code, testChallenge, CodeChallengeMethodS256); err != nil {
		t.Fatalf("claim after refused redeems: %v", err)
	}
	if _, err := a.Redeem(code, "phone", ClientAndroid, testVerifier); err != nil {
		t.Fatalf("redeem after claim: %v", err)
	}
}

func TestTicketClaimBindsOnceAndIsIdempotent(t *testing.T) {
	a, root := enabledAuthority(t, nil)
	code, _, err := a.IssueCloudCode(ticketReq("k1"))
	if err != nil {
		t.Fatal(err)
	}
	var reqErr *requestError
	if _, err := a.ClaimTicket(code, testChallenge, "plain"); !errors.As(err, &reqErr) {
		t.Fatalf("plain method: %v", err)
	}
	if _, err := a.ClaimTicket(code, "short", CodeChallengeMethodS256); !errors.As(err, &reqErr) {
		t.Fatalf("malformed challenge: %v", err)
	}
	writes := countWrites(a)
	if _, err := a.ClaimTicket(code, testChallenge, CodeChallengeMethodS256); err != nil {
		t.Fatal(err)
	}
	if writes.Load() != 1 {
		t.Fatalf("claim not committed: %d writes", writes.Load())
	}
	// Same challenge (a lost answer): succeeds again, writes nothing.
	if _, err := a.ClaimTicket(code, testChallenge, CodeChallengeMethodS256); err != nil {
		t.Fatalf("same-challenge retry: %v", err)
	}
	if writes.Load() != 1 {
		t.Fatal("idempotent claim wrote")
	}
	// Anyone else who saw the ticket (the browser that showed the QR) can
	// neither re-claim it nor redeem it with their own verifier.
	if _, err := a.ClaimTicket(code, S256Challenge(otherVerifier), CodeChallengeMethodS256); !errors.Is(err, errInvalidCode) {
		t.Fatalf("second challenge: %v", err)
	}
	// The claim is durable across a restart.
	a.Close()
	b := openTestAuthority(t, root, Lifetimes{}, nil)
	if _, err := b.ClaimTicket(code, S256Challenge(otherVerifier), CodeChallengeMethodS256); !errors.Is(err, errInvalidCode) {
		t.Fatalf("second challenge after restart: %v", err)
	}
	if _, err := b.Redeem(code, "browser", ClientWeb, otherVerifier); !errors.Is(err, errInvalidCode) {
		t.Fatalf("redeem with another verifier: %v", err)
	}
	// The wrong verifier consumed it (cloud PKCE rule): not even the phone can
	// redeem it now, and no grant exists.
	if _, err := b.Redeem(code, "phone", ClientAndroid, testVerifier); !errors.Is(err, errInvalidCode) {
		t.Fatalf("ticket survived a wrong verifier: %v", err)
	}
	if len(b.List()) != 0 {
		t.Fatal("grant from a refused ticket")
	}
}

func TestTicketRedeemedOnceWithDeviceScope(t *testing.T) {
	a, _ := enabledAuthority(t, nil)
	r := ticketReq("k1")
	r.Permissions = []string{PermRead, PermSteer}
	code, _, err := a.IssueCloudCode(r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.ClaimTicket(code, testChallenge, CodeChallengeMethodS256); err != nil {
		t.Fatal(err)
	}
	tok, err := a.Redeem(code, "Pixel", ClientAndroid, testVerifier)
	if err != nil {
		t.Fatal(err)
	}
	if tok.Scope != "read steer" || tok.ResourceScope.Kind != ResourceWorkgroups || tok.ResourceScope.Workgroups[0] != "wg1" {
		t.Fatalf("grant scope %+v", tok)
	}
	if l := a.List(); len(l) != 1 || l[0].Origin != OriginCloud || l[0].CloudRequester != testOwner {
		t.Fatalf("provenance %+v", l)
	}
	// Single use: neither a second redeem nor a claim works afterwards.
	if _, err := a.Redeem(code, "Pixel", ClientAndroid, testVerifier); !errors.Is(err, errInvalidCode) {
		t.Fatalf("second redeem: %v", err)
	}
	if _, err := a.ClaimTicket(code, testChallenge, CodeChallengeMethodS256); !errors.Is(err, errInvalidCode) {
		t.Fatalf("claim after redeem: %v", err)
	}
	// A same-key re-issue reports the spent key, never a fresh ticket.
	if _, _, err := a.IssueCloudCode(r); cloudCode(t, err) != CloudErrIssueExpired {
		t.Fatalf("re-issue after redeem: %v", err)
	}
}

func TestTicketExpiryAndPolicyChanges(t *testing.T) {
	t.Run("expired before claim", func(t *testing.T) {
		c := &clock{t: time.Unix(1_800_000_000, 0)}
		a, _ := enabledAuthority(t, c)
		code, _, _ := a.IssueCloudCode(ticketReq("k1"))
		c.add(CloudCodeTTL)
		if _, err := a.ClaimTicket(code, testChallenge, CodeChallengeMethodS256); !errors.Is(err, errInvalidCode) {
			t.Fatalf("expired claim: %v", err)
		}
	})
	t.Run("expired after claim", func(t *testing.T) {
		c := &clock{t: time.Unix(1_800_000_000, 0)}
		a, _ := enabledAuthority(t, c)
		code, _, _ := a.IssueCloudCode(ticketReq("k1"))
		if _, err := a.ClaimTicket(code, testChallenge, CodeChallengeMethodS256); err != nil {
			t.Fatal(err)
		}
		c.add(CloudCodeTTL)
		if _, err := a.Redeem(code, "p", ClientAndroid, testVerifier); !errors.Is(err, errInvalidCode) {
			t.Fatalf("expired redeem: %v", err)
		}
	})
	for name, change := range map[string]func(*Authority) error{
		"disable":        func(a *Authority) error { _, err := a.DisableCloud("test"); return err },
		"lower ceiling":  func(a *Authority) error { _, err := a.EnableCloud(cloudPolicyReq([]string{PermRead}, wg1)); return err },
		"identity reset": func(a *Authority) error { _, err := a.ResetIdentity(); return err },
	} {
		t.Run(name+" before claim", func(t *testing.T) {
			a, _ := enabledAuthority(t, nil)
			code, _, _ := a.IssueCloudCode(ticketReq("k1"))
			if err := change(a); err != nil {
				t.Fatal(err)
			}
			if _, err := a.ClaimTicket(code, testChallenge, CodeChallengeMethodS256); err == nil {
				t.Fatal("claimed after a policy change")
			}
		})
		t.Run(name+" after claim", func(t *testing.T) {
			a, _ := enabledAuthority(t, nil)
			code, _, _ := a.IssueCloudCode(ticketReq("k1"))
			if _, err := a.ClaimTicket(code, testChallenge, CodeChallengeMethodS256); err != nil {
				t.Fatal(err)
			}
			if err := change(a); err != nil {
				t.Fatal(err)
			}
			if _, err := a.Redeem(code, "p", ClientAndroid, testVerifier); err == nil {
				t.Fatal("redeemed after a policy change")
			}
		})
	}
	t.Run("manual code is not a ticket", func(t *testing.T) {
		a, _ := enabledAuthority(t, nil)
		code, _, err := a.NewCode("m", []string{PermRead}, wg1, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.ClaimTicket(code, testChallenge, CodeChallengeMethodS256); !errors.Is(err, errInvalidCode) {
			t.Fatalf("manual claim: %v", err)
		}
		pkce, _, _ := a.IssueCloudCode(issueReq("k9"))
		if _, err := a.ClaimTicket(pkce, S256Challenge(otherVerifier), CodeChallengeMethodS256); !errors.Is(err, errInvalidCode) {
			t.Fatalf("PKCE code re-bound by claim: %v", err)
		}
		if _, err := a.Redeem(pkce, "p", ClientAndroid, testVerifier); err != nil {
			t.Fatalf("PKCE code after refused claim: %v", err)
		}
	})
}

func TestTicketClaimUncommittedRollsBack(t *testing.T) {
	a, _ := enabledAuthority(t, nil)
	code, _, _ := a.IssueCloudCode(ticketReq("k1"))
	failingWrites(a, false)
	if _, err := a.ClaimTicket(code, testChallenge, CodeChallengeMethodS256); err == nil {
		t.Fatal("claim succeeded without a commit")
	}
	a.st.write = writeAtomic
	// Rolled back: still unclaimed, so a retry (any challenge) binds it.
	if _, err := a.ClaimTicket(code, testChallenge, CodeChallengeMethodS256); err != nil {
		t.Fatalf("retry after uncommitted claim: %v", err)
	}
}

func TestClaimEndpoint(t *testing.T) {
	a, _ := enabledAuthority(t, nil)
	s := newTestServer(t, a, newFake())
	code, _, _ := a.IssueCloudCode(ticketReq("k1"))
	post := func(path string, v url.Values) (int, string) {
		w := do(s, http.MethodPost, path, "", form(v), "Content-Type", formCT)
		return w.Code, w.Body.String()
	}
	claim := url.Values{"code": {code}, "code_challenge": {testChallenge}, "code_challenge_method": {CodeChallengeMethodS256}}
	// URL parameters are refused (the ticket never rides in a URL).
	if w := do(s, http.MethodPost, PathClaim+"?code="+code, "", form(claim), "Content-Type", formCT); w.Code != http.StatusBadRequest {
		t.Fatalf("query claim: %d", w.Code)
	}
	st, body := post(PathClaim, claim)
	if st != http.StatusOK {
		t.Fatalf("claim: %d %s", st, body)
	}
	var cr ClaimResponse
	if err := json.Unmarshal([]byte(body), &cr); err != nil || !cr.Claimed || cr.ExpiresAt == 0 {
		t.Fatalf("claim body %s", body)
	}
	if strings.Contains(body, code) {
		t.Fatal("ticket echoed")
	}
	if st, _ := post(PathClaim, claim); st != http.StatusOK {
		t.Fatalf("idempotent claim: %d", st)
	}
	other := url.Values{"code": {code}, "code_challenge": {S256Challenge(otherVerifier)}, "code_challenge_method": {CodeChallengeMethodS256}}
	if st, body := post(PathClaim, other); st != http.StatusBadRequest || !strings.Contains(body, OAuthInvalidGrant) {
		t.Fatalf("second challenge: %d %s", st, body)
	}
	tokenReq := url.Values{"grant_type": {GrantTypeEnrollmentCode}, "code": {code}, "client_label": {"Pixel"}, "client_kind": {ClientAndroid}, "code_verifier": {testVerifier}}
	st, body = post(PathToken, tokenReq)
	if st != http.StatusOK {
		t.Fatalf("redeem: %d %s", st, body)
	}
	if strings.Contains(body, testVerifier) {
		t.Fatal("verifier echoed")
	}

	// Unclaimed ticket at the token endpoint: invalid_request, not consumed.
	code2, _, _ := a.IssueCloudCode(ticketReq("k2"))
	tokenReq.Set("code", code2)
	if st, body := post(PathToken, tokenReq); st != http.StatusBadRequest || !strings.Contains(body, OAuthInvalidRequest) {
		t.Fatalf("unclaimed redeem: %d %s", st, body)
	}
}

func TestTicketBundleV2(t *testing.T) {
	now := time.Now()
	b := Bundle{V: 2, ServerID: strings.Repeat("A", 43), Endpoints: []string{"https://h.example:8443"}, Code: testChallenge, ExpiresAt: now.Unix() + 60, Claim: BundleClaimPKCE}
	s, err := EncodeBundle(b)
	if err != nil || !strings.HasPrefix(s, BundlePrefixTicket) {
		t.Fatalf("encode v2: %q %v", s, err)
	}
	got, err := DecodeBundle(s, now)
	if err != nil || got.V != 2 || got.Claim != BundleClaimPKCE {
		t.Fatalf("decode v2: %+v %v", got, err)
	}
	// v1 prefix with a v2 body, or a v2 bundle without its claim, is refused.
	b1 := b
	b1.V = 1
	if s1, _ := EncodeBundle(b1); s1 != "" {
		if _, err := DecodeBundle(s1, now); err == nil {
			t.Fatal("v1 bundle carrying a claim accepted")
		}
	}
	b2 := b
	b2.Claim = ""
	if s2, _ := EncodeBundle(b2); s2 != "" {
		if _, err := DecodeBundle(s2, now); err == nil {
			t.Fatal("v2 bundle without claim accepted")
		}
	}
}
