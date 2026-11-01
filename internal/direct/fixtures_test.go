package direct

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// The direct wire fixtures are the cross-repo contract. Web and Android copy
// docs/direct/fixtures/v1 byte for byte and pin SHA256SUMS. Regenerate with
// UPDATE_DIRECT_FIXTURES=1 go test ./internal/direct -run TestDirectFixtures,
// which is a contract change: tell the client owners.
const fixtureDir = "../../docs/direct/fixtures/v1"

// fixtureNow is the instant the hello fixture was signed.
var fixtureNow = time.Unix(1793000000, 0)

func fixtureIdentity(t *testing.T) *Identity {
	t.Helper()
	d, _ := new(big.Int).SetString("c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee01", 16)
	key := &ecdsa.PrivateKey{D: d}
	key.PublicKey.Curve = elliptic.P256()
	key.PublicKey.X, key.PublicKey.Y = elliptic.P256().ScalarBaseMult(d.Bytes())
	id, err := identityFromKey(key, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

const (
	fixtureNonce    = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"
	fixtureEndpoint = "https://amux.home.example:8443"
	fixtureCode     = "IiMkJSYnKCkqKywtLi8wMTIzNDU2Nzg5Ojs8PT4_QEE"
	fixtureAccess   = AccessTokenPrefix + "QkNERUZHSElKS0xNTk9QUVJTVFVWV1hZWltcXV5fYGE"
	fixtureRefresh  = RefreshTokenPrefix + "YmNkZWZnaGlqa2xtbm9wcXJzdHV2d3h5ent8fX5_gIE"
	fixtureClientID = "c_AAAAAAAAAAAAAAAAAAAAAA"
)

func fixtureFiles(t *testing.T, id *Identity, proof string) map[string][]byte {
	t.Helper()
	js := func(v any) []byte {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		return append(b, '\n')
	}
	bundle, err := EncodeBundle(Bundle{V: 1, ServerID: id.ServerID, Endpoints: []string{fixtureEndpoint}, Code: fixtureCode, ExpiresAt: fixtureNow.Unix() + 600})
	if err != nil {
		t.Fatal(err)
	}
	form := func(v url.Values) []byte { return []byte(v.Encode() + "\n") }
	sessions := SessionsResponse{ServerID: id.ServerID, Sessions: []Session{
		{ID: "771951", Title: "Direct access", Kind: "claude", RootID: "", IsRoot: true, Section: hp.SectionWorkgroups,
			State: hp.StateRunning, Status: "running", Runtime: "claude", Role: hp.RoleCoordinator, Caps: SessionCaps{Prompt: true, Stop: true}},
		{ID: "e1deda", Title: "Backend implementation", Kind: "claude", Mode: "task", Model: "claude-opus-5-5", RootID: "771951",
			Repos: "agent-multiplexer", Section: hp.SectionWorkgroups, State: hp.StateIdle, Status: "idle", StartedAt: 1792999000,
			Runtime: "claude", Caps: SessionCaps{Prompt: true, Stop: false},
			PRs: []hp.SessionPR{{Repo: "owner/agent-multiplexer", Number: 273, URL: "https://github.com/owner/agent-multiplexer/pull/273", Branch: "amux/771951-e1deda", State: "open"}}},
	}}
	textEvent := json.RawMessage(`{"text":"Done.","final":true}`)
	return map[string][]byte{
		"hello-request.json":           js(HelloRequest{Protocol: Protocol, ClientNonce: fixtureNonce, Endpoint: fixtureEndpoint}),
		"hello-response.json":          js(HelloResponse{Proof: proof}),
		"bundle.txt":                   []byte(bundle + "\n"),
		"bundle.json":                  js(Bundle{V: 1, ServerID: id.ServerID, Endpoints: []string{fixtureEndpoint}, Code: fixtureCode, ExpiresAt: fixtureNow.Unix() + 600}),
		"token-request-enrollment.txt": form(url.Values{"grant_type": {GrantTypeEnrollmentCode}, "code": {fixtureCode}, "client_label": {"Pixel"}, "client_kind": {ClientAndroid}}),
		"token-request-refresh.txt":    form(url.Values{"grant_type": {GrantTypeRefresh}, "refresh_token": {fixtureRefresh}}),
		"revoke-request.txt":           form(url.Values{"token": {fixtureRefresh}}),
		"token-response.json": js(TokenResponse{AccessToken: fixtureAccess, TokenType: "Bearer", ExpiresIn: 600, RefreshToken: fixtureRefresh,
			Scope: "read steer", ClientID: fixtureClientID, ServerID: id.ServerID, GrantExpiresAt: fixtureNow.Unix() + 90*86400,
			ResourceScope: ResourceScope{Kind: ResourceWorkgroups, Workgroups: []string{"771951"}}}),
		"token-error.json":        js(OAuthError{Error: OAuthInvalidGrant, Description: "the code or refresh token is invalid, expired, used or revoked"}),
		"api-error.json":          js(APIError{Code: CodeAuthExpired, Error: "access token expired"}),
		"api-error-capacity.json": js(APIError{Code: CodeCapacity, Error: errCapacity.Error()}),
		"token-error-unavailable.json": js(OAuthError{Error: OAuthTemporarilyUnavail,
			Description: "the grant store did not confirm the write; no token was returned, and the presented code or refresh token may already be consumed"}),
		"sessions-response.json": js(sessions),
		"history-response.json": js(HistoryResponse{SessionID: "e1deda", Runtime: "claude", Seq: 42, HeadSeq: 57, HasMore: true,
			Events: []RuntimeEvent{{Type: hp.TypeText, ItemID: "msg_1", Direction: hp.DirOut, Payload: textEvent}}}),
		"stream.sse": []byte("event: events\ndata: " + string(mustJSON(t, StreamBatch{Seq: 58, Runtime: "claude",
			Events: []RuntimeEvent{{Type: hp.TypeTurnStart, Direction: hp.DirOut}}})) + "\n\n: keepalive\n\n" +
			"event: auth\ndata: " + string(mustJSON(t, StreamEnd{Code: CodeAuthExpired})) + "\n\n"),
		"action-request-prompt.json": js(ActionRequest{Verb: VerbPrompt, Fields: map[string]string{"text": "Please run the tests."}, IdemKey: "4f6c1d2e-0b7a-4d3c-9e8f-1a2b3c4d5e6f"}),
		"action-request-stop.json":   js(ActionRequest{Verb: VerbStop, Fields: map[string]string{}, IdemKey: "9a8b7c6d-5e4f-4a3b-2c1d-0e9f8a7b6c5d"}),
		"action-response.json":       js(ActionResponse{OK: true}),
		"watch-auth.json":            js(WatchAuth{Type: "auth", AccessToken: fixtureAccess}),
		"watch-welcome.json":         js(WatchWelcome{Type: "welcome", ServerID: id.ServerID, ExpiresAt: fixtureNow.Unix() + 600}),
		"watch-snapshot.json":        js(WatchSnapshot{Type: "snapshot", Sessions: sessions.Sessions}),
		"ws-close-codes.json":        js(CloseCodes),
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func sums(files map[string][]byte) []byte {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	var b bytes.Buffer
	for _, n := range names {
		h := sha256.Sum256(files[n])
		fmt.Fprintf(&b, "%s  %s\n", hex.EncodeToString(h[:]), n)
	}
	return b.Bytes()
}

func TestDirectFixtures(t *testing.T) {
	id := fixtureIdentity(t)
	var proof string
	if os.Getenv("UPDATE_DIRECT_FIXTURES") == "1" {
		// Keep the committed proof when it still verifies: ES256 signatures are
		// randomized, and re-signing would churn a pinned fixture for nothing.
		if raw, err := os.ReadFile(filepath.Join(fixtureDir, "hello-response.json")); err == nil {
			var old HelloResponse
			if json.Unmarshal(raw, &old) == nil {
				if _, err := VerifyHello(old.Proof, id.ServerID, HelloRequest{Protocol: Protocol, ClientNonce: fixtureNonce, Endpoint: fixtureEndpoint}, fixtureNow); err == nil {
					proof = old.Proof
				}
			}
		}
		if proof == "" {
			var err error
			if proof, err = id.SignHello(HelloRequest{Protocol: Protocol, ClientNonce: fixtureNonce, Endpoint: fixtureEndpoint}, fixtureNow); err != nil {
				t.Fatal(err)
			}
		}
		files := fixtureFiles(t, id, proof)
		files["SHA256SUMS"] = sums(files)
		if err := os.MkdirAll(fixtureDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for n, b := range files {
			if err := os.WriteFile(filepath.Join(fixtureDir, n), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	// The hello proof is randomized ES256, so the committed one is the fixture.
	raw, err := os.ReadFile(filepath.Join(fixtureDir, "hello-response.json"))
	if err != nil {
		t.Fatalf("missing fixtures (regenerate with UPDATE_DIRECT_FIXTURES=1): %v", err)
	}
	var hr HelloResponse
	if err := json.Unmarshal(raw, &hr); err != nil {
		t.Fatal(err)
	}
	want := fixtureFiles(t, id, hr.Proof)
	want["SHA256SUMS"] = sums(want)
	entries, err := os.ReadDir(fixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(want) {
		t.Errorf("fixture dir has %d files, contract has %d", len(entries), len(want))
	}
	for n, b := range want {
		got, err := os.ReadFile(filepath.Join(fixtureDir, n))
		if err != nil {
			t.Errorf("%s: %v", n, err)
			continue
		}
		if !bytes.Equal(got, b) {
			t.Errorf("%s drifted from the wire types; regenerate deliberately (contract change)", n)
		}
	}

	// The committed proof verifies under the reference client check, at the
	// instant it was signed, and fails a minute and a half later.
	sent := HelloRequest{Protocol: Protocol, ClientNonce: fixtureNonce, Endpoint: fixtureEndpoint}
	if _, err := VerifyHello(hr.Proof, id.ServerID, sent, fixtureNow); err != nil {
		t.Fatalf("hello fixture does not verify: %v", err)
	}
	if _, err := VerifyHello(hr.Proof, id.ServerID, sent, fixtureNow.Add(130*time.Second)); err == nil {
		t.Fatal("expired hello fixture verified")
	}
	if b, err := DecodeBundle(strings.TrimSpace(string(want["bundle.txt"])), fixtureNow); err != nil || b.ServerID != id.ServerID {
		t.Fatalf("bundle fixture: %v", err)
	}

	// Every JSON fixture decodes strictly into its wire type.
	strict := map[string]any{
		"hello-request.json": &HelloRequest{}, "hello-response.json": &HelloResponse{}, "bundle.json": &Bundle{},
		"token-response.json": &TokenResponse{}, "token-error.json": &OAuthError{}, "api-error.json": &APIError{}, "api-error-capacity.json": &APIError{}, "token-error-unavailable.json": &OAuthError{},
		"sessions-response.json": &SessionsResponse{}, "history-response.json": &HistoryResponse{},
		"action-request-prompt.json": &ActionRequest{}, "action-request-stop.json": &ActionRequest{},
		"action-response.json": &ActionResponse{}, "watch-auth.json": &WatchAuth{}, "watch-welcome.json": &WatchWelcome{},
		"watch-snapshot.json": &WatchSnapshot{}, "ws-close-codes.json": &map[string]int{},
	}
	for n, v := range strict {
		dec := json.NewDecoder(bytes.NewReader(want[n]))
		dec.DisallowUnknownFields()
		if err := dec.Decode(v); err != nil {
			t.Errorf("%s does not decode strictly: %v", n, err)
		}
		if reflect.ValueOf(v).Elem().IsZero() {
			t.Errorf("%s decoded empty", n)
		}
	}
}
