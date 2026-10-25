package provider

import (
	"context"
	"errors"
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
	"net"
	"testing"
)

type fakeAccountChannel struct {
	calls  int
	result hp.AccountResult
	err    error
}

func (f *fakeAccountChannel) Call(context.Context, hp.AccountRequest, bool) (hp.AccountResult, error) {
	f.calls++
	return f.result, f.err
}
func (f *fakeAccountChannel) Close() error { return nil }
func TestAccountTransportIndependentGrantsAndSecretExclusion(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		status, manage, ephemeral bool
		req                       hp.AccountRequest
		want                      bool
	}{{"manage cannot rebind arbitrary workload", true, true, false, hp.AccountRequest{Operation: hp.AccountRebind, ProfileID: "codex:central", Revision: 1, WorkloadID: "unpublished-workload"}, false}, {"status", true, false, false, hp.AccountRequest{Operation: hp.AccountStatus}, true}, {"manage alone cannot read", false, true, false, hp.AccountRequest{Operation: hp.AccountStatus}, false}, {"status cannot manage", true, false, false, hp.AccountRequest{Operation: hp.AccountLogout, ProfileID: "codex:central", Revision: 1}, false}, {"safe secret rejected", true, true, false, hp.AccountRequest{Operation: hp.AccountStatus, Secret: "secret"}, false}, {"ceremony", false, true, true, hp.AccountRequest{Operation: hp.AccountLogin, ProfileID: "claude:central", Revision: 1, Initiator: "browser"}, true}, {"missing initiator", false, true, true, hp.AccountRequest{Operation: hp.AccountLogin, ProfileID: "claude:central", Revision: 1}, false}} {
		t.Run(tc.name, func(t *testing.T) {
			left, right := net.Pipe()
			defer left.Close()
			defer right.Close()
			f := &fakeAccountChannel{result: hp.AccountResult{OK: true, Ceremony: &hp.AccountCeremony{AuthURL: "secret-url"}}}
			p := New(Config{AccountStatus: tc.status, AccountManage: tc.manage, OpenAccounts: func(context.Context) (AccountChannel, error) { return f, nil }})
			s := &session{hc: hp.NewConn(left), rtCtx: t.Context()}
			typ := hp.MAccountRequest
			if tc.ephemeral {
				typ = hp.MAccountCeremony
			}
			done := make(chan struct{})
			go func() { defer close(done); p.handleAccount(s, hp.MuxMsg{Type: typ, ReqID: "r", Account: &tc.req}) }()
			m, err := hp.NewConn(right).ReadHarness()
			if err != nil {
				t.Fatal(err)
			}
			<-done
			if m.Account.OK != tc.want {
				t.Fatalf("reply: %+v", m.Account)
			}
			if !tc.ephemeral && m.Account.Ceremony != nil {
				t.Fatal("safe response leaked ceremony")
			}
			if !tc.want && f.calls != 0 {
				t.Fatal("unauthorized effect")
			}
		})
	}
}
func TestNativeAccountErrorsAreNotForwarded(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	f := &fakeAccountChannel{err: errors.New("secret token and callback URL")}
	p := New(Config{AccountStatus: true, OpenAccounts: func(context.Context) (AccountChannel, error) { return f, nil }})
	s := &session{hc: hp.NewConn(left), rtCtx: t.Context()}
	go p.handleAccount(s, hp.MuxMsg{Type: hp.MAccountRequest, ReqID: "r", Account: &hp.AccountRequest{Operation: hp.AccountStatus}})
	m, err := hp.NewConn(right).ReadHarness()
	if err != nil || m.Account.Error != "account_service_unavailable" {
		t.Fatalf("%+v %v", m, err)
	}
}
