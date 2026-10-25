package nativetui

import (
	"context"
	"fmt"
	"math"
	"strings"
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

// Missing, expired, or invalid observations must never imply available budget.
func accountRemaining(l accountLimit, now time.Time, stale bool) (float64, string) {
	if stale || l.state == "stale" {
		return 0, "stale"
	}
	if l.state != "available" || l.usedPercent == nil || math.IsNaN(*l.usedPercent) || math.IsInf(*l.usedPercent, 0) || *l.usedPercent < 0 || *l.usedPercent > 100 {
		return 0, "unknown"
	}
	if l.observedAt.IsZero() || l.observedAt.After(now) || l.freshUntil.IsZero() || !now.Before(l.freshUntil) {
		return 0, "stale"
	}
	return 100 - *l.usedPercent, "fresh"
}

func accountPercent(remaining float64) string {
	return strings.TrimSuffix(fmt.Sprintf("%.1f", remaining), ".0") + "%"
}

func accountWindowName(l accountLimit) string {
	name := strings.Split(accountText(l.name), " (")[0]
	switch {
	case name == "five_hour" || strings.HasSuffix(l.name, " (300 minutes)"):
		return "5h"
	case name == "seven_day" || strings.HasSuffix(l.name, " (10080 minutes)"):
		return "7d"
	}
	return name
}

func accountRailBudget(p accountProfile, now time.Time, stale bool) string {
	if stale {
		return "stale · open to refresh"
	}
	if p.authMode == "api" {
		return "API · not applicable"
	}
	if len(p.limits) == 0 {
		return "budget unknown"
	}
	parts := make([]string, 0, 2)
	for _, name := range []string{"5h", "7d"} {
		for _, l := range p.limits {
			if accountWindowName(l) == name {
				remaining, state := accountRemaining(l, now, false)
				value := state
				if state == "fresh" {
					value = accountPercent(remaining)
				}
				parts = append(parts, name+" "+value)
				break
			}
		}
	}
	if len(parts) == len(p.limits) {
		return strings.Join(parts, " · ")
	}
	// Other provider windows cannot be mislabeled as 5h/7d. Show the
	// limiting observation only when every reported window is known/fresh.
	remaining := 100.0
	for _, l := range p.limits {
		value, state := accountRemaining(l, now, false)
		if state != "fresh" {
			return "budget " + state
		}
		remaining = min(remaining, value)
	}
	return "limiting window " + accountPercent(remaining)
}

func (m *model) accountOverviewLines() []string {
	lines := []string{sectionStyle.Render(cellPad(" Accounts · left", sidebarWidth-3)) + sectionKeyStyle.Render("s  ")}
	for _, harness := range []string{"claude", "codex"} {
		label, budget := accountHarnessName(harness), "loading…"
		if m.accountOverview != nil {
			budget = "budget unknown"
			for _, p := range m.accountOverview.profiles {
				if p.harness == harness {
					budget = accountRailBudget(p, time.Now(), m.accountOverviewStale)
					break
				}
			}
			if m.accountOverview.legacyDefaults[harness] {
				label = accountHarnessName(harness) + ": legacy default"
			}
		}
		if m.accountOverviewStale {
			budget = "stale · open to refresh"
		}
		lines = append(lines, cellPad(" "+keyStyle.Render(label), sidebarWidth), cellPad(" "+budget, sidebarWidth))
	}
	return lines
}
