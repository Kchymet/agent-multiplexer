package direct

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Local device tickets (NewLocalTicket, OpEnrollTicket): minted by the host
// operator with no cloud policy, then claimed and redeemed exactly like a cloud
// device ticket. These are the QR form of `amux direct enroll`.

func plainAuthority(t *testing.T, c *clock) (*Authority, string) {
	t.Helper()
	root := t.TempDir()
	var now func() time.Time
	if c != nil {
		now = c.now
	}
	return openTestAuthority(t, root, Lifetimes{}, now), root
}

func TestLocalTicketNeedsNoCloudAndRedeemsOnce(t *testing.T) {
	a, _ := plainAuthority(t, nil) // cloud enrollment never enabled
	code, exp, err := a.NewLocalTicket("phone", []string{PermRead, PermSteer}, wg1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if exp-a.now().Unix() != int64(DefaultCodeTTL/time.Second) {
		t.Fatalf("local ticket ttl %d, want the manual default", exp-a.now().Unix())
	}
	if _, err := a.ClaimTicket(code, testChallenge, CodeChallengeMethodS256); err != nil {
		t.Fatalf("claim without cloud policy: %v", err)
	}
	tok, err := a.Redeem(code, "Pixel", ClientAndroid, testVerifier)
	if err != nil {
		t.Fatal(err)
	}
	if tok.Scope != "read steer" || tok.ResourceScope.Kind != ResourceWorkgroups || tok.ResourceScope.Workgroups[0] != "wg1" {
		t.Fatalf("grant scope %+v", tok)
	}
	// Provenance is manual: the operator minted it, not the cloud.
	if l := a.List(); len(l) != 1 || l[0].Origin != "" || l[0].CloudRequester != "" {
		t.Fatalf("provenance %+v", l)
	}
	if _, err := a.Redeem(code, "Pixel", ClientAndroid, testVerifier); !errors.Is(err, errInvalidCode) {
		t.Fatalf("second redeem: %v", err)
	}
	if _, err := a.ClaimTicket(code, testChallenge, CodeChallengeMethodS256); !errors.Is(err, errInvalidCode) {
		t.Fatalf("claim after redeem: %v", err)
	}
	if st := a.CloudStatus(); st.Outstanding != 0 {
		t.Fatalf("local ticket counted against the cloud caps: %+v", st)
	}
}

func TestLocalTicketRefusesRedeemWithoutClaim(t *testing.T) {
	a, _ := plainAuthority(t, nil)
	code, _, err := a.NewLocalTicket("phone", []string{PermRead}, allSessions, 0)
	if err != nil {
		t.Fatal(err)
	}
	var reqErr *requestError
	// Neither the v1 path (no verifier) nor a verifier without a claim works.
	if _, err := a.RedeemCode(code, "phone", ClientAndroid); !errors.As(err, &reqErr) {
		t.Fatalf("v1-style redeem of a ticket: %v", err)
	}
	if _, err := a.Redeem(code, "phone", ClientAndroid, testVerifier); !errors.As(err, &reqErr) {
		t.Fatalf("unclaimed redeem with a verifier: %v", err)
	}
	if len(a.List()) != 0 {
		t.Fatal("unclaimed ticket produced a grant")
	}
	// Those refusals left it usable for the claiming device.
	if _, err := a.ClaimTicket(code, testChallenge, CodeChallengeMethodS256); err != nil {
		t.Fatal(err)
	}
	// Claimed: the verifier is now mandatory too.
	if _, err := a.RedeemCode(code, "phone", ClientAndroid); !errors.As(err, &reqErr) {
		t.Fatalf("claimed ticket redeemed without a verifier: %v", err)
	}
	if _, err := a.Redeem(code, "phone", ClientAndroid, testVerifier); err != nil {
		t.Fatalf("redeem after claim: %v", err)
	}
}

func TestLocalTicketClaimBindsOnceAndWrongVerifierConsumes(t *testing.T) {
	a, root := plainAuthority(t, nil)
	code, _, err := a.NewLocalTicket("phone", []string{PermRead}, allSessions, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.ClaimTicket(code, testChallenge, CodeChallengeMethodS256); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ClaimTicket(code, testChallenge, CodeChallengeMethodS256); err != nil {
		t.Fatalf("same-challenge retry: %v", err)
	}
	// Whoever else saw the QR cannot re-bind it, even after a restart.
	if _, err := a.ClaimTicket(code, S256Challenge(otherVerifier), CodeChallengeMethodS256); !errors.Is(err, errInvalidCode) {
		t.Fatalf("second challenge: %v", err)
	}
	a.Close()
	b := openTestAuthority(t, root, Lifetimes{}, nil)
	if _, err := b.ClaimTicket(code, S256Challenge(otherVerifier), CodeChallengeMethodS256); !errors.Is(err, errInvalidCode) {
		t.Fatalf("second challenge after restart: %v", err)
	}
	// A wrong verifier consumes it: not even the claiming device can use it now.
	if _, err := b.Redeem(code, "other", ClientWeb, otherVerifier); !errors.Is(err, errInvalidCode) {
		t.Fatalf("redeem with another verifier: %v", err)
	}
	if _, err := b.Redeem(code, "phone", ClientAndroid, testVerifier); !errors.Is(err, errInvalidCode) {
		t.Fatalf("ticket survived a wrong verifier: %v", err)
	}
	if len(b.List()) != 0 {
		t.Fatal("grant from a refused ticket")
	}
}

func TestLocalTicketExpiry(t *testing.T) {
	t.Run("before claim", func(t *testing.T) {
		c := &clock{t: time.Unix(1_800_000_000, 0)}
		a, _ := plainAuthority(t, c)
		code, _, _ := a.NewLocalTicket("p", []string{PermRead}, allSessions, time.Minute)
		c.add(time.Minute)
		if _, err := a.ClaimTicket(code, testChallenge, CodeChallengeMethodS256); !errors.Is(err, errInvalidCode) {
			t.Fatalf("expired claim: %v", err)
		}
	})
	t.Run("after claim", func(t *testing.T) {
		c := &clock{t: time.Unix(1_800_000_000, 0)}
		a, _ := plainAuthority(t, c)
		code, _, _ := a.NewLocalTicket("p", []string{PermRead}, allSessions, time.Minute)
		if _, err := a.ClaimTicket(code, testChallenge, CodeChallengeMethodS256); err != nil {
			t.Fatal(err)
		}
		c.add(time.Minute)
		if _, err := a.Redeem(code, "p", ClientAndroid, testVerifier); !errors.Is(err, errInvalidCode) {
			t.Fatalf("expired redeem: %v", err)
		}
	})
	t.Run("lifetime bounds as manual codes", func(t *testing.T) {
		a, _ := plainAuthority(t, nil)
		if _, _, err := a.NewLocalTicket("p", []string{PermRead}, allSessions, MaxCodeTTL+time.Second); err == nil {
			t.Fatal("ticket longer than the maximum code lifetime")
		}
		if _, _, err := a.NewLocalTicket("p", []string{PermSteer}, allSessions, 0); err == nil {
			t.Fatal("ticket without read")
		}
		if _, _, err := a.NewLocalTicket("p", []string{PermRead}, ResourceScope{}, 0); err == nil {
			t.Fatal("ticket without an explicit scope")
		}
	})
}

// The cloud policy governs cloud tickets only: turning it off (or never having
// it) neither blocks nor widens a local ticket, and cloud tickets keep their
// policy checks.
func TestLocalTicketIndependentOfCloudPolicy(t *testing.T) {
	a, _ := enabledAuthority(t, nil)
	local, _, err := a.NewLocalTicket("p", []string{PermRead}, allSessions, 0)
	if err != nil {
		t.Fatal(err)
	}
	cloud, _, err := a.IssueCloudCode(ticketReq("k1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.DisableCloud("test"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ClaimTicket(cloud, testChallenge, CodeChallengeMethodS256); !errors.Is(err, errInvalidCode) {
		t.Fatalf("cloud ticket claimed after disable: %v", err)
	}
	if _, err := a.ClaimTicket(local, testChallenge, CodeChallengeMethodS256); err != nil {
		t.Fatalf("local ticket blocked by cloud disable: %v", err)
	}
	if _, err := a.Redeem(local, "p", ClientAndroid, testVerifier); err != nil {
		t.Fatal(err)
	}
}

// Manual (v1) codes are untouched: redeemable without a verifier, refused with
// one, and never claimable.
func TestManualCodeUnchangedByLocalTickets(t *testing.T) {
	a, _ := plainAuthority(t, nil)
	code, _, err := a.NewCode("m", []string{PermRead}, allSessions, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.ClaimTicket(code, testChallenge, CodeChallengeMethodS256); !errors.Is(err, errInvalidCode) {
		t.Fatalf("manual code claimed: %v", err)
	}
	var reqErr *requestError
	if _, err := a.Redeem(code, "m", ClientAndroid, testVerifier); !errors.As(err, &reqErr) {
		t.Fatalf("manual code with a verifier: %v", err)
	}
	if _, err := a.RedeemCode(code, "m", ClientAndroid); err != nil {
		t.Fatalf("manual redeem: %v", err)
	}
}

func TestLocalTicketOverHTTP(t *testing.T) {
	a, _ := plainAuthority(t, nil)
	s := newTestServer(t, a, newFake())
	code, _, err := a.NewLocalTicket("phone", []string{PermRead}, allSessions, 0)
	if err != nil {
		t.Fatal(err)
	}
	post := func(path string, v url.Values) (int, string) {
		w := do(s, http.MethodPost, path, "", form(v), "Content-Type", formCT)
		return w.Code, w.Body.String()
	}
	tokenReq := url.Values{"grant_type": {GrantTypeEnrollmentCode}, "code": {code}, "client_label": {"Pixel"}, "client_kind": {ClientAndroid}, "code_verifier": {testVerifier}}
	if st, body := post(PathToken, tokenReq); st != http.StatusBadRequest || !strings.Contains(body, OAuthInvalidRequest) {
		t.Fatalf("unclaimed redeem: %d %s", st, body)
	}
	claim := url.Values{"code": {code}, "code_challenge": {testChallenge}, "code_challenge_method": {CodeChallengeMethodS256}}
	if st, body := post(PathClaim, claim); st != http.StatusOK {
		t.Fatalf("claim: %d %s", st, body)
	}
	st, body := post(PathToken, tokenReq)
	if st != http.StatusOK {
		t.Fatalf("redeem: %d %s", st, body)
	}
	if strings.Contains(body, code) || strings.Contains(body, testVerifier) {
		t.Fatal("secret echoed")
	}
	if st, _ := post(PathToken, tokenReq); st != http.StatusBadRequest {
		t.Fatalf("second redeem: %d", st)
	}
}

func TestAdminEnrollTicketReturnsV2Bundle(t *testing.T) {
	h := newHostAuthority(t)
	root := shortRoot(t)
	a := startAdmin(t, root, h)
	ctx := context.Background()
	resp, err := AdminCall(ctx, root, h.host, AdminRequest{Op: OpEnrollTicket, Label: "Pixel", Permissions: []string{PermRead}, Resource: allSessions})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(resp.Bundle, BundlePrefixTicket) {
		t.Fatal("enroll_ticket did not return a v2 ticket")
	}
	b, err := DecodeBundle(resp.Bundle, time.Now())
	if err != nil || b.V != 2 || b.Claim != BundleClaimPKCE || b.ServerID != a.ServerID() || b.Endpoints[0] != testEndpoint || b.ExpiresAt != resp.ExpiresAt {
		t.Fatalf("ticket: %v %+v", err, b)
	}
	if _, err := a.ClaimTicket(b.Code, testChallenge, CodeChallengeMethodS256); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Redeem(b.Code, "Pixel", ClientAndroid, testVerifier); err != nil {
		t.Fatal(err)
	}
	// Plain enroll still returns a v1 bundle.
	v1, err := AdminCall(ctx, root, h.host, AdminRequest{Op: OpEnroll, Label: "Pixel", Permissions: []string{PermRead}, Resource: allSessions})
	if err != nil || !strings.HasPrefix(v1.Bundle, BundlePrefix) {
		t.Fatalf("enroll: %v", err)
	}
	if _, err := OfflineAdminister(ctx, root, h.host, h.verify, AdminRequest{Op: OpEnrollTicket}); err == nil {
		t.Fatal("offline ticket enrollment without endpoints succeeded")
	}
}
