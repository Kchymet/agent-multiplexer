package nativetui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

func accountFixture() accountSnapshot {
	return projectAccounts(harnessproto.AccountSnapshot{Version: 1, Defaults: map[string]string{"claude": "claude:central", "codex": "codex:central"}, Profiles: []harnessproto.AccountProfile{
		{ID: "claude:central", Harness: "claude", Source: "central", Revision: 7, State: "ready", LoginMethod: "setup-token", Identity: harnessproto.AccountIdentity{State: "unknown", Evidence: "opaque-token"}},
		{ID: "claude:host", Harness: "claude", Source: "host", Revision: 8, State: "ready", LoginMethod: "external", Identity: harnessproto.AccountIdentity{State: "unknown", Evidence: "external"}},
		{ID: "codex:central", Harness: "codex", Source: "central", Revision: 4, State: "ready", LoginMethod: "browser", Identity: harnessproto.AccountIdentity{State: "known", AccountID: "fixture-account", Email: "fixture@example.test", Plan: "fixture-plan", Evidence: "native"}},
	}})
}

func accountModel() *model {
	s := &accountsState{snapshot: accountFixture(), harness: "claude"}
	s.selectProfile()
	return &model{accounts: s, w: 110, h: 32}
}

func TestAccountsNavigationAndConfirmationCaptureRevision(t *testing.T) {
	m := accountModel()
	var calls []accountRequest
	m.accountCall = func(_ context.Context, r accountRequest) (accountResponse, error) {
		calls = append(calls, r)
		return accountResponse{}, nil
	}
	m.handleKey(key("j"))
	if m.accounts.selected != "claude:host" {
		t.Fatal("profile navigation did not select host source")
	}
	m.handleKey(key("d"))
	if m.accounts.confirm == nil || !strings.Contains(accountConfirmation(*m.accounts.confirm, m.accounts.profile()), "Existing workload account bindings stay fixed") {
		t.Fatal("default lacks an impact confirmation")
	}
	// Attempted profile navigation cannot change the reviewed target.
	m.handleKey(key("k"))
	_, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	cmd()
	if len(calls) != 1 || calls[0].op != "default" || calls[0].profile != "claude:host" || calls[0].revision != 8 {
		t.Fatalf("wrong confirmed mutation: %+v", calls)
	}
	if _, cmd := m.handleKey(key("x")); cmd != nil {
		t.Fatal("a second mutation started before the first finished")
	}
}

func TestAccountsProtectedClaudeTokenNeverReachesViewOrReply(t *testing.T) {
	m := accountModel()
	s := m.accounts
	s.attempt = accountAttempt{id: "fixture-attempt", state: "pending", method: "setup-token", expiresAt: time.Now().Add(time.Minute)}
	const secret = "sk-ant-fixture-qcx-token-DO-NOT-DISPLAY"
	m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(secret), Paste: true})
	for _, r := range "qcx" { // these are token input, never quit/cancel/logout hotkeys
		m.handleKey(key(string(r)))
	}
	if string(s.secret) != secret+"qcx" || m.accounts == nil {
		t.Fatal("secret input was interpreted as controls")
	}
	view := m.View()
	if strings.Contains(view, secret) || !strings.Contains(view, "hidden") {
		t.Fatal("protected input was not masked")
	}
	var delivered string
	m.accountCall = func(_ context.Context, req accountRequest) (accountResponse, error) {
		delivered = req.secret
		if req.op != "complete" || req.attempt != "fixture-attempt" {
			t.Fatal("secret sent outside the attempt completion")
		}
		return accountResponse{}, errors.New("provider accidentally echoed " + secret)
	}
	_, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if len(s.secret) != 0 {
		t.Fatal("input survived submission")
	}
	msg := cmd().(accountsReplyMsg)
	if msg.request.secret != "" || delivered != secret+"qcx" {
		t.Fatal("secret must be write-only and absent from reply request")
	}
	m.Update(msg)
	if strings.Contains(m.View(), secret) || strings.Contains(m.status, secret) {
		t.Fatal("raw provider error escaped into routine UI")
	}
}

func TestAccountsSecretCancelClosesConnectionAndClearsBuffer(t *testing.T) {
	m := accountModel()
	s := m.accounts
	s.attempt = accountAttempt{id: "mine", state: "pending", method: "setup-token"}
	s.secret = []rune("fixture-token")
	backing := s.secret
	broker := &machineAccountTransport{}
	s.broker = broker
	m.handleKey(key("esc"))
	for _, r := range backing {
		if r != 0 {
			t.Fatal("cancel did not clear the input buffer")
		}
	}
	if m.accounts != nil || !broker.closed {
		t.Fatal("cancel did not close the ceremony owner connection")
	}
}

func TestAccountsCloseDuringLoginDiscardsLateAttempt(t *testing.T) {
	m := accountModel()
	s := m.accounts
	broker := &machineAccountTransport{}
	s.broker = broker
	m.accountCall = func(_ context.Context, req accountRequest) (accountResponse, error) {
		return accountResponse{attempt: &accountAttempt{id: "late", state: "pending", method: "browser", url: "https://auth.openai.com/authorize?state=fixture"}}, nil
	}
	start := m.requestAccounts(s.request("login"))
	m.handleKey(key("esc"))
	if m.accounts != nil || !broker.closed {
		t.Fatal("loading-time dismissal left ceremony owner connected")
	}
	m.Update(start())
	if m.accounts != nil || strings.Contains(m.View(), "state=fixture") {
		t.Fatal("late response reopened the modal or exposed ceremony")
	}
	// Opening another modal must not accept a reply from the first instance.
	m.openAccounts()
	current := m.accounts
	m.Update(accountsReplyMsg{owner: s, serial: 1, response: accountResponse{attempt: &accountAttempt{id: "late", state: "pending"}}})
	if m.accounts != current || current.attempt.id != "" {
		t.Fatal("old reply contaminated a newly opened modal")
	}
}

func TestAccountsDisconnectClearsCeremonyAndFencesReplies(t *testing.T) {
	m := accountModel()
	s := m.accounts
	s.serial = 3
	s.attempt = accountAttempt{id: "old", state: "pending", url: "https://auth.openai.com/authorize?state=fixture"}
	s.secret = []rune("fixture-secret")
	m.Update(disconnectedMsg{})
	m.Update(accountsReplyMsg{owner: s, serial: 3, response: accountResponse{attempt: &accountAttempt{id: "old", state: "pending", url: "https://auth.openai.com/authorize?state=fixture"}}})
	if s.attempt.id != "" || len(s.secret) > 0 || !s.stale || strings.Contains(m.View(), "state=fixture") {
		t.Fatal("disconnected ceremony leaked or late callback was accepted")
	}
	m.handleKey(key("x"))
	if s.confirm != nil {
		t.Fatal("stale status allowed logout")
	}
}

func TestAccountsWireRequestsKeepSecretsOnCeremonyOnly(t *testing.T) {
	for _, op := range []string{"status", "login", "progress", "cancel", "complete", "default", "logout", "disconnect"} {
		t.Run(op, func(t *testing.T) {
			r := accountRequest{op: op, profile: "claude:central", revision: 7}
			if op == "progress" || op == "cancel" || op == "complete" {
				r.attempt = "fixture-attempt"
			}
			if op == "complete" {
				r.secret = "fixture-token"
			}
			if op == "disconnect" {
				r.profile, r.revision = "", 0
			}
			wire, ceremony, err := accountWireRequest(r)
			if err != nil {
				t.Fatal(err)
			}
			if op == "status" && (wire.ProfileID != "" || wire.Secret != "" || ceremony) {
				t.Fatal("status carried ceremony fields")
			}
			if !harnessproto.ValidateAccountRequest(wire, ceremony) {
				t.Fatalf("wire contract rejects %s", op)
			}
			if (wire.Secret != "") != (op == "complete") {
				t.Fatal("secret crossed a non-completion boundary")
			}
		})
	}
}

func TestAccountsLimitsProvenanceFreshnessAndAttribution(t *testing.T) {
	now := time.Now()
	p := harnessproto.AccountProfile{ID: "codex:central", Harness: "codex", Source: "central", DeliveryEpoch: 3,
		Identity: harnessproto.AccountIdentity{State: "known"}, Limits: harnessproto.AccountLimits{
			State: "known", Source: "codex-native", ProfileID: "codex:central", DeliveryEpoch: 3, ObservedAt: now.Add(-time.Minute).UnixMilli(),
			Windows: []harnessproto.AccountLimitWindow{{Name: "Weekly", UsedPercent: 0, ResetsAt: now.Add(time.Hour).Unix()}},
		}}
	project := func() accountLimit {
		return projectAccounts(harnessproto.AccountSnapshot{Profiles: []harnessproto.AccountProfile{p}}).profiles[0].limits[0]
	}
	l := project()
	text := strings.Join(accountLimitLines(l, now, false), "\n")
	for _, want := range []string{"available", "Used: 0%", "codex-native", "provider report", "Observed:", "Resets:"} {
		if !strings.Contains(text, want) {
			t.Fatalf("limit view missing %q: %s", want, text)
		}
	}
	if text := strings.Join(accountLimitLines(l, now.Add(6*time.Minute), false), "\n"); !strings.Contains(text, "stale") {
		t.Fatal("old limit appeared current")
	}
	for _, invalidate := range []func(){
		func() { p.Limits.DeliveryEpoch++ },
		func() { p.Limits.DeliveryEpoch = 3; p.Limits.ProfileID = "other" },
		func() { p.Limits.ProfileID = p.ID; p.Identity.State = "unknown" },
	} {
		invalidate()
		if got := project(); got.state != "unavailable" || got.usedPercent != nil {
			t.Fatal("unattributed quota displayed as account usage")
		}
	}
}

func TestAccountsCeremonyURLValidationAndTerminalErasure(t *testing.T) {
	for _, raw := range []string{"https://evil.test/", "https://auth.openai.com.evil.test/", "https://user:token@auth.openai.com/", "http://auth.openai.com/", "https://auth.openai.com:444/", "https://auth.openai.com/\x1b[2J", "https://auth.openai.com/#token"} {
		if approvedAccountURL(raw) {
			t.Errorf("accepted unsafe ceremony URL %q", raw)
		}
	}
	if !approvedAccountURL("https://auth.openai.com/authorize?state=fixture") {
		t.Fatal("rejected provider URL")
	}
	a := projectAccountCeremony(harnessproto.AccountCeremony{AttemptID: "done", State: "complete", AuthURL: "https://auth.openai.com/authorize?state=fixture"})
	if a.url != "" {
		t.Fatal("terminal ceremony retained URL")
	}
	m := accountModel()
	m.accounts.snapshot.profiles[0].account = "fixture\x1b[2J\a\u202eevil"
	if got := accountText(m.accounts.snapshot.profiles[0].account); strings.ContainsAny(got, "\x1b\a\u202e") {
		t.Fatal("account metadata retained terminal/bidi controls")
	}
}

func TestAccountsNarrowViewportAndSecretPasteIsolation(t *testing.T) {
	m := accountModel()
	m.w, m.h = 52, 12
	m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x"), Paste: true})
	if m.accounts.confirm != nil {
		t.Fatal("paste triggered logout")
	}
	for i := 0; i < 5; i++ {
		view := m.renderAccounts()
		if len(strings.Split(view, "\n")) > m.paneRows() {
			t.Fatal("account view overflowed height")
		}
		for _, line := range strings.Split(view, "\n") {
			if ansi.StringWidth(line) > m.mainWidth() {
				t.Fatalf("account view overflowed width: %q", line)
			}
		}
		m.handleKey(key("pgdown"))
	}
}

func TestAccountsFailureMessagesNeverEchoRawError(t *testing.T) {
	for _, op := range []string{"status", "progress", "complete", "login", "logout"} {
		text := accountErrorMessage(fmt.Errorf("https://provider.test?secret=fixture-token"), op)
		if strings.Contains(text, "fixture-token") || strings.Contains(text, "https://") {
			t.Fatal("failure message disclosed native error")
		}
	}
}

func TestAccountsTokenPasteRejectsMultilineAndOversizedInput(t *testing.T) {
	for _, input := range []string{"fixture-token\nsecond-line", "fixture-token\rsecond-line", "fixture\x1b[2Jtoken", strings.Repeat("a", 4097)} {
		m := accountModel()
		m.accounts.attempt = accountAttempt{id: "mine", state: "pending", method: "setup-token"}
		m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(input), Paste: true})
		if len(m.accounts.secret) != 0 || m.accounts.notice == "" {
			t.Fatal("malformed paste was silently changed into a token")
		}
		if strings.Contains(m.View(), "fixture-token") {
			t.Fatal("rejected paste appeared in the view")
		}
	}
	m := accountModel()
	m.accounts.attempt = accountAttempt{id: "mine", state: "pending", method: "setup-token"}
	m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("  fixture-token\r\n"), Paste: true})
	if string(m.accounts.secret) != "fixture-token" {
		t.Fatal("clipboard's trailing newline should not invalidate a single token")
	}
}

func TestAccountsTokenExpiresWithoutAnotherKeystroke(t *testing.T) {
	m := accountModel()
	s := m.accounts
	s.attempt = accountAttempt{id: "expiring", state: "pending", method: "setup-token"}
	s.secret = []rune("fixture-token")
	m.Update(accountsExpiredMsg{owner: s, attempt: "expiring"})
	if len(s.secret) != 0 || s.attempt.id != "" || !s.stale {
		t.Fatal("expired ceremony retained token input")
	}
}

func TestAccountsUnknownUsageNeverAppearsAsZeroOrUnlimited(t *testing.T) {
	l := accountLimit{name: "Weekly", state: "unknown", source: "unsupported"}
	text := strings.Join(accountLimitLines(l, time.Now(), false), "\n")
	if strings.Contains(text, "0%") || strings.Contains(text, "unlimited") || !strings.Contains(text, "unknown") {
		t.Fatal("unknown percentage was presented as measured usage")
	}
}

type blockedAccountConnection struct {
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (c *blockedAccountConnection) Accounts(harnessproto.AccountRequest, bool) (harnessproto.AccountResult, error) {
	close(c.started)
	<-c.closed
	return harnessproto.AccountResult{}, errors.New("fixture network error with fixture-token")
}

func (c *blockedAccountConnection) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

func TestAccountsDismissalInterruptsPendingConnection(t *testing.T) {
	m := accountModel()
	c := &blockedAccountConnection{started: make(chan struct{}), closed: make(chan struct{})}
	m.accounts.broker = &machineAccountTransport{client: c}
	cmd := m.requestAccounts(m.accounts.request("login"))
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	<-c.started
	m.handleKey(key("esc"))
	select {
	case msg := <-done:
		if strings.Contains(fmt.Sprintf("%+v", msg), "fixture-token") {
			t.Fatal("raw connection error retained in event")
		}
		m.Update(msg)
	case <-time.After(time.Second):
		t.Fatal("dismissal did not interrupt pending account request")
	}
	if m.accounts != nil {
		t.Fatal("interrupted request reopened modal")
	}
}

func TestAccountsTimeoutClosesCeremonyOwner(t *testing.T) {
	c := &blockedAccountConnection{started: make(chan struct{}), closed: make(chan struct{})}
	transport := &machineAccountTransport{client: c}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := transport.call(ctx, accountRequest{op: "status"})
		done <- err
	}()
	<-c.started
	cancel()
	select {
	case err := <-done:
		if err == nil || strings.Contains(err.Error(), "fixture-token") {
			t.Fatal("timeout did not return a safe failure")
		}
	case <-time.After(time.Second):
		t.Fatal("context timeout did not close the owner connection")
	}
}
