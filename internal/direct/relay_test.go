package direct

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

func relayReq() hp.DirectEnrollRequest {
	return hp.DirectEnrollRequest{IssueKey: "k1", Requester: testOwner, CodeChallenge: testChallenge,
		CodeChallengeMethod: hp.DirectEnrollMethodS256, Permissions: []string{PermRead},
		Resource: hp.DirectResource{Kind: hp.DirectResourceWorkgroups, Workgroups: []string{"wg1"}}, Label: "Pixel"}
}

// The relay bridge can reach exactly two admin operations, whatever it is asked.
func TestRelayIssuerSendsOnlyTypedOperations(t *testing.T) {
	var ops []AdminRequest
	call := func(_ context.Context, req AdminRequest) (AdminResponse, error) {
		ops = append(ops, req)
		switch req.Op {
		case OpCloudStatus:
			return AdminResponse{OK: true, ServerID: "sid", Endpoints: []string{testEndpoint}, Cloud: &CloudStatus{Enabled: true, OwnerPin: "secret-pin"}}, nil
		}
		return AdminResponse{Code: CloudErrOwnerMismatch, Error: "detailed host message with " + testOwner}, errors.New("refused")
	}
	r := NewRelayIssuer(call)
	caps, err := r.Capabilities(context.Background())
	if err != nil || caps == nil || !caps.Issuance || caps.ServerID != "sid" || caps.Endpoints[0] != testEndpoint {
		t.Fatalf("caps %+v %v", caps, err)
	}
	res := r.Enroll(context.Background(), "prov-registered", relayReq())
	if res.Error != hp.DirectEnrollErrOwnerMismatch || res.Bundle != "" {
		t.Fatalf("refusal %+v", res)
	}
	if len(ops) != 2 || ops[0].Op != OpCloudStatus || ops[1].Op != OpIssueCloudCode {
		t.Fatalf("ops %+v", ops)
	}
	issue := ops[1].Issue
	if issue == nil || issue.ProviderID != "prov-registered" || issue.Requester != testOwner || ops[1].Cloud != nil ||
		ops[1].ClientID != "" || ops[1].Label != "" || len(ops[1].Permissions) != 0 {
		t.Fatalf("issue request %+v", ops[1])
	}
}

func TestRelayIssuerHidesUnknownErrorsAndDisabledPolicy(t *testing.T) {
	for name, call := range map[string]AdminCaller{
		"transport": func(context.Context, AdminRequest) (AdminResponse, error) {
			return AdminResponse{}, errors.New("dial unix /secret/path: connection refused")
		},
		"unknown code": func(context.Context, AdminRequest) (AdminResponse, error) {
			return AdminResponse{Code: "something_new", Error: "x"}, errors.New("x")
		},
		"ok without bundle": func(context.Context, AdminRequest) (AdminResponse, error) { return AdminResponse{OK: true}, nil },
	} {
		res := NewRelayIssuer(call).Enroll(context.Background(), "p", relayReq())
		if res.Error != hp.DirectEnrollErrUnavailable || res.Bundle != "" {
			t.Errorf("%s: %+v", name, res)
		}
	}
	off := NewRelayIssuer(func(context.Context, AdminRequest) (AdminResponse, error) {
		return AdminResponse{OK: true, ServerID: "sid", Endpoints: []string{testEndpoint}, Cloud: &CloudStatus{}}, nil
	})
	if caps, err := off.Capabilities(context.Background()); caps != nil || err != nil {
		t.Fatalf("disabled policy advertised: %+v %v", caps, err)
	}
}

func TestRelayCodesMatchWireVocabulary(t *testing.T) {
	for k, v := range relayCodes {
		if k != v {
			t.Errorf("%s relays as %s", k, v)
		}
	}
}

// End to end over the real host-proven admin socket: relay request in, bundle
// out, redeemable only with the client's verifier.
func TestRelayIssuerEndToEnd(t *testing.T) {
	h := newHostAuthority(t)
	root := shortRoot(t)
	a := startAdmin(t, root, h)
	r := NewRelayIssuer(func(ctx context.Context, req AdminRequest) (AdminResponse, error) {
		return AdminCall(ctx, root, h.host, req)
	})
	ctx := context.Background()
	if caps, err := r.Capabilities(ctx); caps != nil || err != nil {
		t.Fatalf("advertised before enable: %+v %v", caps, err)
	}
	if res := r.Enroll(ctx, testProvider, relayReq()); res.Error != hp.DirectEnrollErrDisabled {
		t.Fatalf("issued before enable: %+v", res)
	}
	if _, err := a.EnableCloud(cloudPolicyReq([]string{PermRead}, wg1)); err != nil {
		t.Fatal(err)
	}
	caps, err := r.Capabilities(ctx)
	if err != nil || caps == nil || caps.ServerID != a.ServerID() {
		t.Fatalf("caps %+v %v", caps, err)
	}
	if res := r.Enroll(ctx, "prov_other", relayReq()); res.Error != hp.DirectEnrollErrProviderMismatch {
		t.Fatalf("other provider: %+v", res)
	}
	res := r.Enroll(ctx, testProvider, relayReq())
	if res.Error != "" || res.ServerID != a.ServerID() {
		t.Fatalf("issue: %+v", res)
	}
	again := r.Enroll(ctx, testProvider, relayReq())
	if again.Bundle != res.Bundle {
		t.Fatal("relay retry minted a second code")
	}
	b, err := DecodeBundle(res.Bundle, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.RedeemCode(b.Code, "x", ClientWeb); err == nil {
		t.Fatal("cloud code redeemed without a verifier")
	}
	tok, err := a.Redeem(b.Code, "Pixel", ClientAndroid, testVerifier)
	if err != nil || !strings.HasPrefix(tok.AccessToken, AccessTokenPrefix) {
		t.Fatalf("redeem: %v", err)
	}
}
