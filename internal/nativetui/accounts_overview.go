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

// Missing, future, or invalid observations must never imply available budget.
// An expired observation keeps its last-known value and is reported "stale" so
// callers label it with its age instead of collapsing to unknown.
func accountRemaining(l accountLimit, now time.Time, stale bool) (float64, string) {
	if (l.state != "available" && l.state != "stale") || l.usedPercent == nil || math.IsNaN(*l.usedPercent) || math.IsInf(*l.usedPercent, 0) || *l.usedPercent < 0 || *l.usedPercent > 100 {
		return 0, "unknown"
	}
	if l.observedAt.IsZero() || l.observedAt.After(now) {
		return 0, "unknown"
	}
	remaining := 100 - *l.usedPercent
	if stale || l.state == "stale" || l.freshUntil.IsZero() || !now.Before(l.freshUntil) {
		return remaining, "stale"
	}
	return remaining, "fresh"
}

// accountAge is a compact observation age such as "12m ago".
func accountAge(observed, now time.Time) string {
	d := now.Sub(observed)
	switch {
	case observed.IsZero() || d < 0:
		return ""
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd ago", int(d/(24*time.Hour)))
}

// accountOldest is the oldest stale observation behind a rail budget, or zero
// when every displayed window is fresh.
func accountOldest(p accountProfile, now time.Time, stale bool) time.Time {
	var oldest time.Time
	for _, l := range p.limits {
		if _, state := accountRemaining(l, now, stale); state == "stale" && (oldest.IsZero() || l.observedAt.Before(oldest)) {
			oldest = l.observedAt
		}
	}
	return oldest
}

func accountPercent(remaining float64) string {
	return strings.TrimSuffix(fmt.Sprintf("%.1f", remaining), ".0") + "%"
}

func accountWindowName(l accountLimit) string {
	name := strings.Split(accountText(l.name), " (")[0]
	bucket, window, scoped := strings.Cut(name, ":")
	if !scoped {
		window = name
	}
	label := window
	switch {
	case window == "five_hour":
		label = "5h"
	case window == "seven_day":
		label = "7d"
	case window == "primary" || window == "secondary":
		if strings.HasSuffix(l.name, " (300 minutes)") {
			label = "5h"
		}
		if strings.HasSuffix(l.name, " (10080 minutes)") {
			label = "7d"
		}
	}
	// Native rateLimitsByLimitId keys identify different budgets. Keep that
	// category even when the underlying window has a familiar duration.
	if scoped {
		return bucket + ":" + label
	}
	return label
}

func accountRailBudget(p accountProfile, now time.Time, stale bool) string {
	if p.authMode == "api" {
		return "API · not applicable"
	}
	if len(p.limits) == 0 {
		return "budget unknown"
	}
	suffix := ""
	if !accountOldest(p, now, stale).IsZero() {
		suffix = " · stale"
	}
	parts := make([]string, 0, 2)
	for _, name := range []string{"5h", "7d"} {
		for _, l := range p.limits {
			label := accountWindowName(l)
			if label == name || (p.harness == "codex" && label == "codex:"+name) {
				remaining, state := accountRemaining(l, now, stale)
				value := state
				if state != "unknown" {
					value = accountPercent(remaining)
				}
				parts = append(parts, name+" "+value)
				break
			}
		}
	}
	if len(parts) == len(p.limits) {
		return strings.Join(parts, " · ") + suffix
	}
	// Other provider windows cannot be mislabeled as 5h/7d. Show the
	// limiting observation only when every reported window is known.
	remaining := 100.0
	for _, l := range p.limits {
		value, state := accountRemaining(l, now, stale)
		if state == "unknown" {
			return "budget unknown"
		}
		remaining = min(remaining, value)
	}
	return "limiting window " + accountPercent(remaining) + suffix
}

// accountRailDetail is the rail's second budget line, such as
// "2 resets · retry in 37m": the provider's redeemable reset count, and how
// long a provider-requested refresh pause lasts. Empty when neither applies.
func accountRailDetail(p accountProfile, now time.Time) string {
	if p.authMode == "api" {
		return ""
	}
	parts := []string{}
	if r := accountResets(p); r != "" {
		parts = append(parts, r)
	}
	if len(p.limits) > 0 {
		if l := p.limits[0]; !l.retryAt.IsZero() && now.Before(l.retryAt) {
			parts = append(parts, "retry in "+accountDuration(l.retryAt.Sub(now)))
		}
	}
	return strings.Join(parts, " · ")
}

// accountResets is "2 resets", or empty when the provider reported no count.
func accountResets(p accountProfile) string {
	if p.resets == nil {
		return ""
	}
	if *p.resets == 1 {
		return "1 reset"
	}
	return fmt.Sprintf("%d resets", *p.resets)
}

func (m *model) accountOverviewLines() []string {
	lines := []string{sectionStyle.Render(cellPad(" Accounts · left", sidebarWidth-3)) + sectionKeyStyle.Render("s  ")}
	now := time.Now()
	for _, harness := range []string{"claude", "codex"} {
		label, budget, detail := accountHarnessName(harness), "loading…", ""
		if m.accountOverviewStale {
			budget = "stale · open to refresh"
		}
		// A failed status refresh keeps the last snapshot visible, marked stale.
		if m.accountOverview != nil {
			budget = "budget unknown"
			for _, p := range m.accountOverview.profiles {
				if p.harness == harness {
					budget = accountRailBudget(p, now, m.accountOverviewStale)
					detail = accountRailDetail(p, now)
					if m.accountOverviewStale && p.authMode != "api" && !strings.Contains(budget, " · stale") {
						budget = "stale · open to refresh" // nothing last-known to show
					}
					if age := accountAge(accountOldest(p, now, m.accountOverviewStale), now); age != "" && p.authMode != "api" {
						label += " · " + age
					}
					break
				}
			}
			if m.accountOverview.migrationPending[harness] {
				label = accountHarnessName(harness) + ": login required"
			} else if m.accountOverview.legacyDefaults[harness] {
				label = accountHarnessName(harness) + ": legacy default"
			}
		}
		lines = append(lines, cellPad(" "+keyStyle.Render(label), sidebarWidth), cellPad("   "+budget, sidebarWidth))
		if detail != "" {
			lines = append(lines, cellPad("   "+dimStyle.Render(detail), sidebarWidth))
		}
	}
	return lines
}
