package main

import (
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
	"strings"
	"testing"
)

type apiCompatibilityService struct {
	calls    int
	ceremony *hp.AccountCeremony
}

func (s *apiCompatibilityService) Accounts(r hp.AccountRequest, _ bool) (hp.AccountResult, error) {
	s.calls++
	if r.Secret != "" {
		panic("API key sent before capability confirmation")
	}
	return hp.AccountResult{OK: true, Ceremony: s.ceremony}, nil
}

func TestAPILoginRequiresCapabilityAndExplicitCeremonyEcho(t *testing.T) {
	for _, supported := range []bool{false, true} {
		for _, cer := range []*hp.AccountCeremony{nil, {State: "pending", Method: "setup-token"}, {State: "pending", Method: "api-key"}, {State: "pending", Method: "api-key", Mode: "subscription"}, {State: "pending", Method: "api-key", Mode: "api"}} {
			p := hp.AccountProfile{ID: "claude:central", Revision: 1}
			if supported {
				p.SupportedModes = []string{"subscription", "api"}
			}
			service := &apiCompatibilityService{ceremony: cer}
			_, err := startAccountLogin(service, p, "api", "test")
			valid := supported && cer != nil && cer.Mode == "api" && cer.Method == "api-key"
			if valid != (err == nil) {
				t.Fatalf("supported=%v ceremony=%+v error=%v", supported, cer, err)
			}
			if !supported && service.calls != 0 {
				t.Fatal("old daemon started API ceremony")
			}
			if err != nil && !strings.Contains(err.Error(), "upgrade") {
				t.Fatal("missing upgrade guidance")
			}
		}
	}
}
