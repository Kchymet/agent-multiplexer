package nativetui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

func TestAccountsAPIRejectsOldAndAmbiguousDaemon(t *testing.T) {
	m := accountModel()
	_, cmd := m.handleAccounts(key("a"))
	if cmd != nil || m.accounts.confirm != nil || !strings.Contains(m.accounts.notice, "Upgrade") {
		t.Fatal("old status allowed API login")
	}
	for _, reply := range []*accountAttempt{nil, {id: "old", state: "pending", method: "setup-token"}, {id: "ambiguous", state: "pending", method: "api-key"}, {id: "wrong", state: "pending", method: "setup-token", mode: "api"}} {
		m = accountModel()
		m.accounts.snapshot.profiles[0].canAPI = true
		calls := 0
		m.accountCall = func(_ context.Context, r accountRequest) (accountResponse, error) {
			calls++
			if r.secret != "" {
				t.Fatal("secret sent to incompatible daemon")
			}
			return accountResponse{attempt: reply}, nil
		}
		_, cmd = m.handleAccounts(key("a"))
		if cmd != nil || m.accounts.confirm == nil {
			t.Fatal("missing confirmation")
		}
		_, cmd = m.handleAccounts(key("enter"))
		if cmd == nil {
			t.Fatal("missing login request")
		}
		m.updateAccounts(cmd().(accountsReplyMsg))
		_, cmd = m.handleAccounts(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("synthetic-SECRET"), Paste: true})
		if cmd != nil || m.accounts.secretInput() || calls != 1 || !strings.Contains(m.accounts.notice, "Upgrade") {
			t.Fatal("ambiguous ceremony accepted key input")
		}
	}
}

func TestAccountsBothProvidersVisibleInitially(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {120, 32}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			m := accountModel()
			m.w, m.h = size[0], size[1]
			now := time.Now()
			reset := time.Date(2030, 1, 2, 3, 4, 0, 0, time.UTC)
			for i := range m.accounts.snapshot.profiles {
				p := &m.accounts.snapshot.profiles[i]
				used := 23.0
				if p.harness == "codex" {
					used = 67
				}
				p.limits = []accountLimit{{name: "five_hour", state: "available", usedPercent: &used, observedAt: now, freshUntil: now.Add(time.Minute), resetsAt: reset}, {name: "seven_day", state: "available", usedPercent: &used, observedAt: now, freshUntil: now.Add(time.Minute), resetsAt: reset}}
			}
			m.accountOverview = &m.accounts.snapshot
			view := ansi.Strip(m.View())
			for _, want := range []string{"Claude · subscription · ready", "Codex · subscription · ready", "5h 23% used", "7d 23% used", "5h 67% used", "7d 67% used", "reset 01-02 03:04Z", "fresh", "statusline advisory; identity unverified"} {
				if !strings.Contains(view, want) {
					t.Fatalf("missing %q initially:\n%s", want, view)
				}
			}
			if len(strings.Split(view, "\n")) > m.h {
				t.Fatal("screen overflow")
			}
			for _, line := range strings.Split(view, "\n") {
				if ansi.StringWidth(line) > m.w {
					t.Fatal("width overflow")
				}
			}
			if dir := os.Getenv("AMUX_ACCOUNTS_RENDER_DIR"); dir != "" {
				lines := strings.Split(view, "\n")
				for i := range lines {
					lines[i] = strings.TrimRight(lines[i], " ")
				}
				view = strings.Join(lines, "\n")
				if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("accounts-%dx%d.txt", m.w, m.h)), []byte(view+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

type ambiguousAPIConnection struct{ closed bool }

func (c *ambiguousAPIConnection) Accounts(_ harnessproto.AccountRequest, _ bool) (harnessproto.AccountResult, error) {
	return harnessproto.AccountResult{OK: true, Ceremony: &harnessproto.AccountCeremony{AttemptID: "owned-but-ambiguous", State: "pending", Method: "setup-token"}}, nil
}
func (c *ambiguousAPIConnection) Close() error { c.closed = true; return nil }

func TestAccountsAmbiguousAPICeremonyClosesOwnerTransport(t *testing.T) {
	m := accountModel()
	m.accounts.snapshot.profiles[0].canAPI = true
	conn := &ambiguousAPIConnection{}
	broker := &machineAccountTransport{client: conn}
	m.accounts.broker = broker
	m.handleAccounts(key("a"))
	_, cmd := m.handleAccounts(key("enter"))
	if cmd == nil {
		t.Fatal("no login request")
	}
	reply := cmd().(accountsReplyMsg)
	if reply.err == nil {
		t.Fatal("ambiguous API reply accepted")
	}
	m.updateAccounts(reply)
	if !conn.closed || !broker.closed || m.accounts.broker != nil || m.accounts.attempt.active() {
		t.Fatal("rejected ceremony left owner transport alive")
	}
}
