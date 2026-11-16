package harnessproto

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
)

const (
	testRoute  = "dwr_q8Hq0xV0y3Yb2mJ1wQ7p4uZr9tN6cD5eF8gH1iJ2kL3"
	testWake   = "dwk_AAECAwQFBgcICQoLDA0ODw"
	testSecret = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	testGrant  = "c2VjcmV0LWdyYW50LXJlZg"
)

func TestDirectWakeHandleShapes(t *testing.T) {
	if !ValidDirectWakeRouteID(testRoute) || !ValidDirectWakeID(testWake) || !ValidDirectWakeSecret(testSecret) || !validGrantRef(testGrant) {
		t.Fatal("golden handles must be well-formed")
	}
	for _, bad := range []string{"", "dwr_", "dwk_" + testRoute[4:], "dwr_" + testRoute[4:len(testRoute)-1], "dwr_" + strings.Repeat("+", 43)} {
		if ValidDirectWakeRouteID(bad) {
			t.Errorf("route %q accepted", bad)
		}
	}
	if ValidDirectWakeID(testRoute) || ValidDirectWakeSecret(testRoute) {
		t.Error("handle kinds must not be interchangeable")
	}
}

// TestDirectWakeChallengeIsS256 pins the shared fixture: the secret is the
// unpadded base64url STRING of 32 random bytes, and the challenge is the
// unpadded base64url of SHA-256 over that string's ASCII bytes, exactly PKCE
// S256. The vector is RFC 7636 Appendix B, also in
// testdata/direct-wake-v1-challenge.json for clients.
func TestDirectWakeChallengeIsS256(t *testing.T) {
	const rfcChallenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	if got := DirectWakeChallenge(testSecret); got != rfcChallenge {
		t.Fatalf("challenge = %q, want RFC 7636 %q", got, rfcChallenge)
	}
	sum := sha256.Sum256([]byte(testSecret))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != rfcChallenge {
		t.Fatal("hash must be over the encoded string, not the decoded bytes")
	}
	raw, _ := base64.RawURLEncoding.DecodeString(testSecret)
	if len(raw) != 32 {
		t.Fatalf("secret decodes to %d bytes, want 32", len(raw))
	}
	fixture, err := os.ReadFile("testdata/direct-wake-v1-challenge.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		ActivationSecret    string `json:"activation_secret"`
		ActivationChallenge string `json:"activation_challenge"`
	}
	if err := json.Unmarshal(fixture, &f); err != nil || f.ActivationSecret != testSecret || f.ActivationChallenge != rfcChallenge {
		t.Fatalf("fixture %s does not match the pinned vector", fixture)
	}
}

func TestValidDirectWakeFrame(t *testing.T) {
	act := &DirectWake{ServerID: "s", RouteID: testRoute, ActivationSecret: testSecret, GrantRef: testGrant}
	wake := &DirectWake{ServerID: "s", Wakes: []DirectWakeRef{{RouteID: testRoute, WakeID: testWake, GrantRef: testGrant}}}
	routes := &DirectWake{ServerID: "s", Epoch: "e", Final: true, Routes: []DirectWakeRouteRef{{RouteID: testRoute, GrantRef: testGrant, State: DirectWakeStateActivating}}}
	for _, c := range []struct {
		typ string
		w   *DirectWake
	}{{HDirectWakeActivate, act}, {HDirectWake, wake}, {HDirectWakeRoutes, routes}} {
		if !ValidDirectWakeFrame(c.typ, c.w) {
			t.Errorf("%s rejected", c.typ)
		}
	}
	// The secret may ride only on activation.
	leak := *wake
	leak.ActivationSecret = testSecret
	if ValidDirectWakeFrame(HDirectWake, &leak) {
		t.Error("wake frame carrying a secret accepted")
	}
	tooMany := &DirectWake{ServerID: "s"}
	for i := 0; i <= MaxDirectWakeBatch; i++ {
		tooMany.Wakes = append(tooMany.Wakes, wake.Wakes[0])
	}
	if ValidDirectWakeFrame(HDirectWake, tooMany) {
		t.Error("oversized batch accepted")
	}
	if ValidDirectWakeFrame(HDirectWakeRoutes, &DirectWake{ServerID: "s", Routes: routes.Routes}) {
		t.Error("routes without an epoch accepted")
	}
	// Cross-kind fields are refused in every direction.
	crossKind := []struct {
		typ string
		w   DirectWake
	}{
		{HDirectWakeActivate, DirectWake{ServerID: "s", RouteID: testRoute, ActivationSecret: testSecret, GrantRef: testGrant, Epoch: "e"}},
		{HDirectWakeActivate, DirectWake{ServerID: "s", RouteID: testRoute, ActivationSecret: testSecret, GrantRef: testGrant, Final: true}},
		{HDirectWakeActivate, DirectWake{ServerID: "s", RouteID: testRoute, ActivationSecret: testSecret, GrantRef: testGrant, Part: 1}},
		{HDirectWakeActivate, DirectWake{ServerID: "s", RouteID: testRoute, ActivationSecret: testSecret, GrantRef: testGrant, Wakes: wake.Wakes}},
		{HDirectWake, DirectWake{ServerID: "s", Wakes: wake.Wakes, RouteID: testRoute}},
		{HDirectWake, DirectWake{ServerID: "s", Wakes: wake.Wakes, Requester: "u"}},
		{HDirectWake, DirectWake{ServerID: "s", Wakes: wake.Wakes, GrantRef: testGrant}},
		{HDirectWake, DirectWake{ServerID: "s", Wakes: wake.Wakes, Epoch: "e"}},
		{HDirectWakeRoutes, DirectWake{ServerID: "s", Epoch: "e", Routes: routes.Routes, ActivationSecret: testSecret}},
		{HDirectWakeRoutes, DirectWake{ServerID: "s", Epoch: "e", Routes: routes.Routes, RouteID: testRoute}},
		{HDirectWakeRoutes, DirectWake{ServerID: "s", Epoch: "e", Routes: routes.Routes, Requester: "u"}},
		{HDirectWakeRoutes, DirectWake{ServerID: "s", Epoch: "e", Part: MaxDirectWakeRouteParts}},
		{HDirectWakeRoutes, DirectWake{ServerID: "s", Epoch: "e", Routes: []DirectWakeRouteRef{{RouteID: testRoute, GrantRef: testGrant, State: "failed"}}}},
	}
	for i, c := range crossKind {
		if ValidDirectWakeFrame(c.typ, &c.w) {
			t.Errorf("cross-kind case %d (%s) accepted", i, c.typ)
		}
	}
	// An empty routes snapshot (no subscriptions) is valid: it is how a host
	// says "none".
	if !ValidDirectWakeFrame(HDirectWakeRoutes, &DirectWake{ServerID: "s", Epoch: "e", Final: true}) {
		t.Error("empty final snapshot rejected")
	}
	if ValidDirectWakeFrame("nope", act) || ValidDirectWakeFrame(HDirectWake, nil) {
		t.Error("unknown type or nil body accepted")
	}
}

// TestDirectWakeWireCarriesNoContent pins the exact field set of every wake
// wire type. Adding a field (say, a session id or a title) must fail here and
// be reviewed against the contract's metadata-only rule.
func TestDirectWakeWireCarriesNoContent(t *testing.T) {
	want := map[reflect.Type][]string{
		reflect.TypeOf(DirectWake{}):             {"activationSecret", "epoch", "final", "grantRef", "part", "requester", "routeId", "routes", "serverId", "wakes"},
		reflect.TypeOf(DirectWakeRef{}):          {"grantRef", "routeId", "wakeId"},
		reflect.TypeOf(DirectWakeRouteRef{}):     {"grantRef", "routeId", "state"},
		reflect.TypeOf(DirectWakeResult{}):       {"error", "results"},
		reflect.TypeOf(DirectWakeResultItem{}):   {"routeId", "status", "wakeId"},
		reflect.TypeOf(DirectWakeCapabilities{}): {"serverId"},
	}
	for typ, fields := range want {
		var got []string
		for i := 0; i < typ.NumField(); i++ {
			got = append(got, strings.Split(typ.Field(i).Tag.Get("json"), ",")[0])
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, fields) {
			t.Errorf("%s fields = %v, want %v", typ.Name(), got, fields)
		}
	}
}
