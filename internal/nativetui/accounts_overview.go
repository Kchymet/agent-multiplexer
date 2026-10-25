package nativetui

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

type accountOverviewReplyMsg struct {
	serial   uint64
	snapshot *accountSnapshot
	err      error
}
type accountOverviewTickMsg struct{ serial uint64 }

// Overview queries use a short-lived connection, never a ceremony connection.
func (m *model) refreshAccountOverview() tea.Cmd {
	if m.accounts != nil {
		return nil
	}
	m.accountOverviewSerial++
	serial, call := m.accountOverviewSerial, m.accountCall
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if call == nil {
			transport := &machineAccountTransport{}
			defer transport.close()
			call = transport.call
		}
		response, err := call(ctx, accountRequest{op: "status"})
		if err != nil {
			err = safeAccountError(err)
		}
		return accountOverviewReplyMsg{serial, response.snapshot, err}
	}
}

func (m *model) updateAccountOverview(msg accountOverviewReplyMsg) tea.Cmd {
	if msg.serial != m.accountOverviewSerial || m.accounts != nil {
		return nil
	}
	m.accountOverviewStale = msg.err != nil || msg.snapshot == nil
	if !m.accountOverviewStale {
		m.accountOverview = msg.snapshot
	}
	return tea.Tick(30*time.Second, func(time.Time) tea.Msg { return accountOverviewTickMsg{msg.serial} })
}

func accountHarnessName(harness string) string {
	if harness == "claude" {
		return "Claude"
	}
	return "Codex"
}

func (m *model) accountOverviewLines() []string {
	lines := []string{sectionStyle.Render(" Accounts"+"              ") + sectionKeyStyle.Render("s  ")}
	for _, harness := range []string{"claude", "codex"} {
		state := "loading…"
		if m.accountOverview != nil {
			state = "unavailable"
			for _, p := range m.accountOverview.profiles {
				if p.harness == harness {
					state = accountText(p.state)
					if state == "" {
						state = "unknown"
					}
					break
				}
			}
			if m.accountOverview.legacyDefaults[harness] {
				state = "legacy default"
			}
		}
		if m.accountOverviewStale {
			state = "stale · open to refresh"
		}
		lines = append(lines, padTo(" "+accountHarnessName(harness)+": "+state, sidebarWidth))
	}
	return lines
}
