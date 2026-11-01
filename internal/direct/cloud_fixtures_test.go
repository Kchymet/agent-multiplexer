package direct

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// The cloud-v1 fixtures are the additive contract for cloud-assisted
// enrollment. They live beside, never inside, docs/direct/fixtures/v1, whose
// manifest web and Android pin. Regenerate with
// UPDATE_DIRECT_FIXTURES=1 go test ./internal/direct -run TestCloudFixtures.
const cloudFixtureDir = "../../docs/direct/fixtures/cloud-v1"

// fixtureVerifier is a sample RFC 7636 verifier; fixtureChallenge is its S256.
const (
	fixtureVerifier  = "SVRgcHFxc3R1dnd4eXp7fH1-f4CBgoOEhYaHiImKi4yNjo-QkZKTlJWWl5iZmpucnZ6f"
	fixtureIssueKey  = "6f1c2a9e-4b7d-4e0a-9c3f-2d8e7b6a5c41"
	fixtureRequester = "usr_01HZXK5Q8M"
)

func cloudFixtureFiles(t *testing.T) map[string][]byte {
	t.Helper()
	js := func(v any) []byte { return append(mustIndent(t, v), '\n') }
	form := func(v url.Values) []byte { return []byte(v.Encode() + "\n") }
	return map[string][]byte{
		"token-request-enrollment-pkce.txt": form(url.Values{"grant_type": {GrantTypeEnrollmentCode}, "code": {fixtureCode},
			"client_label": {"Pixel"}, "client_kind": {ClientAndroid}, "code_verifier": {fixtureVerifier}}),
		"token-error-pkce-required.json": js(OAuthError{Error: OAuthInvalidRequest, Description: "code_verifier is required for this code"}),
		// Relay frames (Harness <-> provider), as harnessproto encodes them.
		"direct-enroll-request.json": js(hp.MuxMsg{Type: hp.MDirectEnroll, ReqID: "de1", DirectEnroll: &hp.DirectEnrollRequest{
			IssueKey: fixtureIssueKey, Requester: fixtureRequester, CodeChallenge: S256Challenge(fixtureVerifier),
			CodeChallengeMethod: hp.DirectEnrollMethodS256, Permissions: []string{PermRead, PermSteer},
			Resource: hp.DirectResource{Kind: hp.DirectResourceWorkgroups, Workgroups: []string{"771951"}}, Label: "Pixel"}}),
		"direct-enroll-result.json": js(hp.HarnessMsg{Type: hp.HDirectEnrollResult, ReqID: "de1", DirectEnroll: &hp.DirectEnrollResult{
			Bundle: string(bytes.TrimSpace(mustBundle(t))), ExpiresAt: fixtureNow.Unix() + int64(CloudCodeTTL/time.Second), ServerID: fixtureIdentity(t).ServerID}}),
		"direct-enroll-refused.json": js(hp.HarnessMsg{Type: hp.HDirectEnrollResult, ReqID: "de2", DirectEnroll: &hp.DirectEnrollResult{Error: hp.DirectEnrollErrOwnerMismatch}}),
	}
}

func mustBundle(t *testing.T) []byte {
	t.Helper()
	id := fixtureIdentity(t)
	b, err := EncodeBundle(Bundle{V: 1, ServerID: id.ServerID, Endpoints: []string{fixtureEndpoint}, Code: fixtureCode,
		ExpiresAt: fixtureNow.Unix() + int64(CloudCodeTTL/time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	return []byte(b)
}

func TestCloudFixtures(t *testing.T) {
	if !ValidVerifier(fixtureVerifier) {
		t.Fatal("fixture verifier is not RFC 7636")
	}
	want := cloudFixtureFiles(t)
	want["SHA256SUMS"] = sums(want)
	if os.Getenv("UPDATE_DIRECT_FIXTURES") == "1" {
		if err := os.MkdirAll(cloudFixtureDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for n, b := range want {
			if err := os.WriteFile(filepath.Join(cloudFixtureDir, n), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	entries, err := os.ReadDir(cloudFixtureDir)
	if err != nil {
		t.Fatalf("missing cloud fixtures (regenerate with UPDATE_DIRECT_FIXTURES=1): %v", err)
	}
	if len(entries) != len(want) {
		t.Errorf("cloud fixture dir has %d files, contract has %d", len(entries), len(want))
	}
	for n, b := range want {
		got, err := os.ReadFile(filepath.Join(cloudFixtureDir, n))
		if err != nil || !bytes.Equal(got, b) {
			t.Errorf("%s drifted from the wire types: %v", n, err)
		}
	}
}

// The S1 manifest web and Android pin is untouched by cloud enrollment.
func TestS1FixtureManifestPinned(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(fixtureDir, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	if got := hexSum(raw); got != "a238403245ec643c45bd4869530155ece7cba7e97d4965bfb5ad128880a07b25" {
		t.Fatalf("docs/direct/fixtures/v1/SHA256SUMS changed: %s", got)
	}
	if n := bytes.Count(raw, []byte("\n")); n != 22 {
		t.Fatalf("v1 manifest lists %d payloads, want 22", n)
	}
}

func mustIndent(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func hexSum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
