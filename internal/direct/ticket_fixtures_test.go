package direct

import (
	"bytes"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// The ticket-v1 fixtures are the additive contract for device tickets
// (docs/direct-cloud-enrollment.md "Device tickets"). They live beside the
// cloud-v1 and v1 manifests, which stay byte-for-byte unchanged. Regenerate with
// UPDATE_DIRECT_FIXTURES=1 go test ./internal/direct -run TestTicketFixtures.
const ticketFixtureDir = "../../docs/direct/fixtures/ticket-v1"

func ticketFixtureBundle(t *testing.T) string {
	t.Helper()
	b, err := EncodeBundle(Bundle{V: 2, ServerID: fixtureIdentity(t).ServerID, Endpoints: []string{fixtureEndpoint}, Code: fixtureCode,
		ExpiresAt: fixtureNow.Unix() + int64(CloudCodeTTL/time.Second), Claim: BundleClaimPKCE})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func ticketFixtureFiles(t *testing.T) map[string][]byte {
	t.Helper()
	js := func(v any) []byte { return append(mustIndent(t, v), '\n') }
	form := func(v url.Values) []byte { return []byte(v.Encode() + "\n") }
	bundle := ticketFixtureBundle(t)
	exp := fixtureNow.Unix() + int64(CloudCodeTTL/time.Second)
	return map[string][]byte{
		"bundle-ticket.txt": []byte(bundle + "\n"),
		"bundle-ticket.json": js(Bundle{V: 2, ServerID: fixtureIdentity(t).ServerID, Endpoints: []string{fixtureEndpoint}, Code: fixtureCode,
			ExpiresAt: exp, Claim: BundleClaimPKCE}),
		"direct-enroll-ticket-request.json": js(hp.MuxMsg{Type: hp.MDirectEnroll, ReqID: "dt1", DirectEnroll: &hp.DirectEnrollRequest{
			IssueKey: fixtureIssueKey, Requester: fixtureRequester, Permissions: []string{PermRead},
			Resource: hp.DirectResource{Kind: hp.DirectResourceWorkgroups, Workgroups: []string{"771951"}}, Label: "Phone",
			Handoff: hp.DirectHandoffDeviceTicket}}),
		"direct-enroll-ticket-result.json": js(hp.HarnessMsg{Type: hp.HDirectEnrollResult, ReqID: "dt1", DirectEnroll: &hp.DirectEnrollResult{
			Bundle: bundle, ExpiresAt: exp, ServerID: fixtureIdentity(t).ServerID}}),
		"claim-request.txt": form(url.Values{"code": {fixtureCode}, "code_challenge": {S256Challenge(fixtureVerifier)},
			"code_challenge_method": {CodeChallengeMethodS256}}),
		"claim-response.json":    js(ClaimResponse{Claimed: true, ExpiresAt: exp}),
		"claim-error-taken.json": js(OAuthError{Error: OAuthInvalidGrant, Description: "the ticket is invalid, expired, used or claimed by another device"}),
		"token-error-unclaimed.json": js(OAuthError{Error: OAuthInvalidRequest,
			Description: "this device ticket must be claimed with a code_challenge before it is redeemed"}),
		// Redemption after a claim is the unchanged cloud PKCE grant
		// (cloud-v1/token-request-enrollment-pkce.txt).
	}
}

func TestTicketFixtures(t *testing.T) {
	want := ticketFixtureFiles(t)
	want["SHA256SUMS"] = sums(want)
	if os.Getenv("UPDATE_DIRECT_FIXTURES") == "1" {
		if err := os.MkdirAll(ticketFixtureDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for n, b := range want {
			if err := os.WriteFile(filepath.Join(ticketFixtureDir, n), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	entries, err := os.ReadDir(ticketFixtureDir)
	if err != nil {
		t.Fatalf("missing ticket fixtures (regenerate with UPDATE_DIRECT_FIXTURES=1): %v", err)
	}
	if len(entries) != len(want) {
		t.Errorf("ticket fixture dir has %d files, contract has %d", len(entries), len(want))
	}
	for n, b := range want {
		got, err := os.ReadFile(filepath.Join(ticketFixtureDir, n))
		if err != nil || !bytes.Equal(got, b) {
			t.Errorf("%s drifted from the wire types: %v", n, err)
		}
	}
	// The fixture bundle decodes as a v2 ticket bundle.
	if b, err := DecodeBundle(ticketFixtureBundle(t), fixtureNow); err != nil || b.V != 2 || b.Claim != BundleClaimPKCE {
		t.Fatalf("fixture bundle: %+v %v", b, err)
	}
}

// The cloud-v1 manifest Harness and Android pin is untouched by device tickets.
func TestCloudFixtureManifestPinned(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(cloudFixtureDir, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	if got := hexSum(raw); got != cloudManifestSum {
		t.Fatalf("docs/direct/fixtures/cloud-v1/SHA256SUMS changed: %s", got)
	}
}

// cloudManifestSum is the sha256 of docs/direct/fixtures/cloud-v1/SHA256SUMS.
const cloudManifestSum = "87b449b9c73ff7557ef31eda630e98c224cfaf9198c1e596a90402f15ca04c12"
