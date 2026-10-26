package nativetui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// Account projections contain display metadata only. Login ceremony data has a
// separate lifetime and is never copied to the dashboard status or agent panes.
type accountProfile struct {
	id, harness, label, mode, state string
	authMode                        string
	account, plan, identitySource   string
	revision                        uint64
	defaultProfile                  bool
	canLogin, canLogout, canDefault bool
	canAPI                          bool
	loginHelp                       string
	statusDetail                    string
	workloads                       *int
	limits                          []accountLimit
	resets                          *int64 // provider-reported redeemable rate-limit resets
}

type accountLimit struct {
	name, state, source, confidence, reason string
	refreshError                            string
	usedPercent                             *float64
	observedAt, freshUntil, resetsAt        time.Time
	retryAt                                 time.Time // provider-requested pause end

}

type accountSnapshot struct {
	profiles         []accountProfile
	canManage        bool
	defaultRevisions map[string]uint64
	legacyDefaults   map[string]bool
}

type accountAttempt struct {
	id, state, method, url, code string
	mode                         string
	expiresAt                    time.Time
}

func (a accountAttempt) active() bool {
	return a.id != "" && (a.state == "pending" || a.state == "starting" || a.state == "waiting")
}

type accountRequest struct {
	op, harness, profile, attempt string
	loginMode                     string
	secret                        string // write-only; cleared before returning a reply
	revision                      uint64
	defaultRevision               uint64
}

type accountResponse struct {
	snapshot *accountSnapshot
	attempt  *accountAttempt
}

type accountCaller func(context.Context, accountRequest) (accountResponse, error)

type accountsState struct {
	snapshot  accountSnapshot
	harness   string
	selected  string // stable profile identity, never a list offset
	attempt   accountAttempt
	pending   string
	notice    string
	stale     bool
	confirm   *accountRequest
	scroll    int
	serial    uint64
	submitted bool
	secret    []rune // masked token/API-key input, never a generic form field
	broker    *machineAccountTransport
}

type accountsReplyMsg struct {
	owner    *accountsState
	serial   uint64
	request  accountRequest
	response accountResponse
	err      error
}

type accountsPollMsg struct {
	owner  *accountsState
	serial uint64
}

type accountsExpiredMsg struct {
	owner   *accountsState
	attempt string
}

func (s *accountsState) secretInput() bool {
	return s.attempt.active() && !s.submitted && (s.attempt.method == "setup-token" || s.attempt.method == "api-key")
}

func (s *accountsState) profiles() []accountProfile {
	for _, p := range s.snapshot.profiles {
		if p.harness == s.harness && p.mode == "central" && p.id == s.harness+":central" {
			return []accountProfile{p}
		}
	}
	return nil
}

func (s *accountsState) profile() *accountProfile {
	for _, p := range s.profiles() {
		if p.id == s.selected {
			return &p
		}
	}
	return nil
}

func (s *accountsState) selectProfile() {
	if s.profile() != nil {
		return
	}
	s.selected = ""
	for _, p := range s.profiles() {
		if s.selected == "" || p.defaultProfile {
			s.selected = p.id
		}
	}
}

func (m *model) openAccounts() tea.Cmd {
	m.status = ""
	m.accounts = &accountsState{harness: "claude"}
	return m.requestAccounts(accountRequest{op: "status"})
}

func (m *model) requestAccounts(req accountRequest) tea.Cmd {
	s := m.accounts
	m.accountOverviewSerial++ // fence a background status fetched before this operation
	m.accountOverviewStale = true
	s.serial++
	s.pending = req.op
	serial, call := s.serial, m.accountCall
	if call == nil {
		if s.broker == nil {
			s.broker = &machineAccountTransport{}
		}
		call = s.broker.call
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		res, err := call(ctx, req)
		if err == nil && req.op == "login" && req.loginMode == "api" && (res.attempt == nil || res.attempt.mode != "api" || res.attempt.method != "api-key" || res.attempt.state != "pending") {
			res = accountResponse{}
			err = errAccountsUnsupported
		}
		req.secret = ""
		if err != nil {
			// Bubble Tea messages themselves must not retain raw provider errors.
			err = safeAccountError(err)
		}
		return accountsReplyMsg{s, serial, req, res, err}
	}
}

func (m *model) updateAccounts(msg accountsReplyMsg) tea.Cmd {
	s := m.accounts
	if s == nil || s != msg.owner || s.serial != msg.serial {
		return nil
	}
	s.pending = ""
	if msg.request.op == "login" {
		s.submitted = false
	}
	if msg.request.op == "complete" {
		s.submitted = true
	}
	if msg.err != nil {
		// Upstream failures can contain tokens or authorization URLs. Only the
		// adapter's bounded error classification may be rendered here.
		s.stale = true
		s.clearSecret()
		if s.broker != nil {
			s.broker.close()
			s.broker = nil
		}
		s.attempt = accountAttempt{}
		s.attempt.url, s.attempt.code = "", ""
		s.notice = accountErrorMessage(msg.err, msg.request.op)

		return nil
	}
	if msg.response.snapshot != nil {
		s.snapshot = *msg.response.snapshot
		m.accountOverview = msg.response.snapshot
		m.accountOverviewStale = false
		s.stale = false
		s.selectProfile()
	}
	if a := msg.response.attempt; a != nil {
		if s.attempt.id != a.id {
			s.scroll = 0
		}
		s.attempt = *a
		if !s.attempt.active() {
			s.attempt.url, s.attempt.code = "", ""
		}
	}

	if msg.request.op != "status" {
		s.notice = ""
	}
	if msg.response.attempt != nil {
		switch s.attempt.state {
		case "succeeded":
			s.notice = "Login complete. Workloads use it from their next launch; running ones keep their current login."
		case "failed":
			s.notice = "Login failed. Review the refreshed profile status before retrying."
		case "cancelled":
			s.notice = "Login cancelled."
		}
	}
	switch msg.request.op {
	case "cancel":
		s.attempt = accountAttempt{}
		s.notice = "Login cancelled."
	case "logout":
		s.notice = "Logout requested. Refresh status to check local cleanup."
	}
	if s.attempt.active() && !s.secretInput() {
		serial := s.serial
		return tea.Tick(time.Second, func(time.Time) tea.Msg { return accountsPollMsg{s, serial} })
	}
	if s.attempt.active() {
		if !s.attempt.expiresAt.IsZero() {
			id := s.attempt.id
			return tea.Tick(max(time.Millisecond, time.Until(s.attempt.expiresAt)), func(time.Time) tea.Msg { return accountsExpiredMsg{s, id} })
		}
		return nil
	}
	if msg.request.op != "status" {
		return m.requestAccounts(accountRequest{op: "status"})
	}
	return nil
}

func (m *model) handleAccounts(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	s := m.accounts
	if k.Paste && !(s.secretInput() && s.pending == "") {
		return m, nil
	}
	secretInput := s.secretInput()
	if k.String() == "esc" || k.String() == "ctrl+c" || (k.String() == "q" && !secretInput) {
		if s.confirm != nil {
			s.confirm = nil
			s.scroll = 0
			return m, nil
		}
		if s.pending != "" && s.pending != "status" && s.pending != "progress" {
			m.status = "Account operation unconfirmed; reopen settings to reconcile."
		}
		s.dispose() // socket close cancels all ceremonies, including a lost start reply
		m.accounts = nil
		return m, m.refreshAccountOverview()
	}
	if s.pending != "" {
		return m, nil
	}
	if k.String() == "pgdown" || k.String() == "pgup" {
		if k.String() == "pgdown" {
			_, h := m.accountsSize()
			s.scroll += max(1, h-6)
		} else {
			_, h := m.accountsSize()
			s.scroll = max(0, s.scroll-max(1, h-6))
		}
		return m, nil
	}
	if s.confirm != nil {
		if k.String() == "enter" || k.String() == "y" {
			req := *s.confirm
			s.confirm = nil
			return m, m.requestAccounts(req)
		}
		if k.String() == "n" {
			s.confirm = nil
		}
		return m, nil
	}
	if s.attempt.active() {
		if s.secretInput() {
			if !s.attempt.expiresAt.IsZero() && !time.Now().Before(s.attempt.expiresAt) {
				s.clearSecret()
				s.notice = "Login expired. Press Esc to close and start again."
				return m, nil
			}
			switch k.Type {
			case tea.KeyEnter:
				if len(s.secret) == 0 {
					s.notice = "Paste the requested credential first. Input stays hidden."
					return m, nil
				}
				req := s.request("complete")
				req.attempt, req.secret = s.attempt.id, string(s.secret)
				s.clearSecret()
				return m, m.requestAccounts(req)
			case tea.KeyBackspace:
				if n := len(s.secret); n > 0 {
					s.secret[n-1] = 0
					s.secret = s.secret[:n-1]
				}
			case tea.KeyCtrlU:
				s.clearSecret()
			case tea.KeyRunes:
				input := strings.TrimSpace(string(k.Runes))
				if len(s.secret)+len(input) > 4096 {
					s.clearSecret()
					s.notice = "Credential too long. Input cleared; paste only the token or API key."
					return m, nil
				}
				for _, r := range input {
					if r <= ' ' || r >= 127 {
						s.clearSecret()
						s.notice = "Invalid credential input. Paste only the requested token or API key."
						return m, nil
					}
					s.secret = append(s.secret, r)
				}
			}
			return m, nil
		}
		if k.String() == "c" {
			s.attempt.url, s.attempt.code = "", ""
			return m, m.requestAccounts(accountRequest{op: "cancel", harness: s.harness, profile: s.selected, attempt: s.attempt.id})
		}
		if k.String() == "r" {
			return m, m.requestAccounts(accountRequest{op: "progress", harness: s.harness, profile: s.selected, attempt: s.attempt.id})
		}
		return m, nil
	}

	switch k.String() {
	case "tab", "shift+tab", "left", "right", "h", "l", "up", "down", "j", "k":
		next := s.harness
		switch k.String() {
		case "up", "k":
			next = "claude"
		case "down", "j":
			next = "codex"
		default:
			if s.harness == "claude" {
				next = "codex"
			} else {
				next = "claude"
			}
		}
		// Vertical movement clamps at each end. Only an actual provider
		// change resets the details viewport, just like Tab.
		if next != s.harness {
			s.harness = next
			s.selected, s.scroll = "", 0
			s.selectProfile()
		}
	case "r":
		return m, m.requestAccounts(accountRequest{op: "status"})
	case "e", "enter", "a", "x":
		if s.stale || !s.snapshot.canManage {
			s.notice = "Account management unavailable. Refresh status before making changes."
			return m, nil
		}
		p := s.profile()
		switch k.String() {
		case "e", "enter", "a":
			if k.String() == "a" && (p == nil || !p.canAPI) {
				s.notice = "API login unsupported. Upgrade the amux daemon and reconnect."
				return m, nil
			}
			if p != nil && p.canLogin {
				req := s.request("login")
				req.loginMode = "subscription"
				if k.String() == "a" {
					req.loginMode = "api"
				}
				s.scroll = 0
				s.confirm = &req
			} else {
				s.notice = "Login unavailable here; inspect the profile's login instructions."
			}
		case "x":
			if p != nil && p.canLogout {
				req := s.request("logout")
				s.scroll = 0
				s.confirm = &req
			}
		}
	}
	return m, nil
}

func (s *accountsState) request(op string) accountRequest {
	r := accountRequest{op: op, harness: s.harness, profile: s.selected}
	if p := s.profile(); p != nil {
		r.revision = p.revision
		if op == "default" {
			r.defaultRevision = s.snapshot.defaultRevisions[p.harness]
		}
	}
	return r
}

func accountText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			return -1
		}
		return r
	}, ansi.Strip(s))
}

func accountTime(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.UTC().Format("2006-01-02 15:04 UTC")
}

func accountValue(v *float64) string {
	if v == nil {
		return "unknown"
	}
	return fmt.Sprintf("%g", *v)
}

func accountLimitLines(l accountLimit, now time.Time, stale bool) []string {
	state := l.state
	if state == "" {
		state = "unavailable"
	}
	if state == "available" {
		if _, s := accountRemaining(l, now, stale); s == "stale" {
			state = "stale"
		}
	}
	lines := []string{accountText(l.name) + ": " + accountText(state)}
	if state == "available" || state == "stale" {
		used := "unknown"
		if l.usedPercent != nil {
			used = accountValue(l.usedPercent) + "%"
		}
		lines = append(lines, "  Used: "+used+" of the reported window")
	}
	observed := accountTime(l.observedAt)
	if age := accountAge(l.observedAt, now); age != "" {
		observed += " (" + age + ")"
	}
	lines = append(lines, "  Source: "+accountText(l.source)+"  Confidence: "+accountText(l.confidence),
		"  Observed: "+observed+"  Resets: "+accountTime(l.resetsAt))
	if l.refreshError != "" {
		lines = append(lines, "  Last refresh failed: "+accountRefreshProblem(l.refreshError)+accountRetry(l, now))
	}
	if l.reason != "" {
		lines = append(lines, "  "+accountText(l.reason))
	}
	return lines
}

func (m *model) renderAccounts() string {
	s := m.accounts
	w, h := m.accountsSize()
	w, h = max(1, w-4), max(1, h-4) // border, horizontal padding, title and footer
	compact := h < 12 || w < 60
	lines := []string{}
	if s.stale {
		lines = append(lines, "STALE — refresh required before changes")
	}
	for _, harness := range []string{"claude", "codex"} {
		var profile *accountProfile
		for _, p := range s.snapshot.profiles {
			if p.id == harness+":central" && p.mode == "central" {
				profile = &p
				break
			}
		}
		mark := "  "
		if s.harness == harness {
			mark = "› "
		}
		if profile == nil {
			lines = append(lines, mark+accountHarnessName(harness)+": unavailable", "  Budget: unknown")
			if !compact {
				lines = append(lines, "")
			}
			continue
		}
		p := *profile
		mode := p.authMode
		if mode == "" {
			mode = "subscription"
		}
		heading := mark + accountHarnessName(harness) + " · " + mode + " · " + accountText(p.state)
		if s.harness == harness {
			heading = selStyle.Render(heading)
		} else {
			heading = titleStyle.Render(heading)
		}
		lines = append(lines, heading)
		if compact {
			budget := accountRailBudget(p, time.Now(), s.stale)
			if detail := accountRailDetail(p, time.Now()); detail != "" {
				budget += " · " + detail
			}
			lines = append(lines, "  "+budget)
			continue
		}
		if mode == "api" {
			lines = append(lines, "  Subscription usage: not applicable")
		} else {
			if len(p.limits) == 0 {
				lines = append(lines, "  Usage: unavailable (unknown)")
			}
			for i, l := range p.limits {
				if i == 2 {
					break // additional windows remain in the selected provider details
				}
				lines = append(lines, accountUsageSummary(l, time.Now(), s.stale))
			}
			if detail := accountRailDetail(p, time.Now()); detail != "" {
				lines = append(lines, "  "+detail)
			}
			source := "provider report"
			if harness == "claude" {
				source = "statusline advisory; identity unverified"
				if len(p.limits) > 0 && (p.limits[0].source == "claude-usage" || p.limits[0].source == "claude-probe") {
					source = "provider report; identity unverified"
				}
			}
			if len(p.limits) > 2 {
				source += fmt.Sprintf(" · %d more windows in details", len(p.limits)-2)
			}
			lines = append(lines, dimStyle.Render("  "+source))
		}
		if s.snapshot.legacyDefaults[harness] {
			lines = append(lines, "  Migrated from an older login: central login required")
		}
		lines = append(lines, "")
	}
	lines = append(lines, dimStyle.Render("Tab/↑↓/jk: select · e: subscription · a: API key"), dimStyle.Render("x: logout · r: refresh"), "Workloads follow this login at their next launch.")
	if p := s.profile(); p != nil {
		identity, plan := accountText(p.account), accountText(p.plan)
		if identity == "" {
			identity = "unknown (not verified)"
		}
		if plan == "" {
			plan = "unknown"
		}
		lines = append(lines, "", "Selected: "+accountHarnessName(p.harness), "Account: "+identity, "Plan: "+plan, accountText(p.loginHelp))
		if compact && s.snapshot.legacyDefaults[p.harness] {
			lines = append(lines, "Migrated from an older login: central login required")
		}
		if p.statusDetail != "" {
			lines = append(lines, p.statusDetail)
		}
		if p.resets != nil {
			lines = append(lines, fmt.Sprintf("Rate-limit resets available: %d", *p.resets))
		}
		for _, l := range p.limits {
			lines = append(lines, accountLimitLines(l, time.Now(), s.stale)...)
		}
	}

	if !s.snapshot.canManage {
		lines = append(lines, "Account management is unavailable on this connection.")
	}
	if s.confirm != nil {
		lines = []string{accountConfirmation(*s.confirm, s.profile()), "", "Enter: confirm · Esc: back"}
	}
	if s.attempt.active() {
		lines = []string{"Login · " + s.harness, "State: " + accountText(s.attempt.state), "Expires: " + accountTime(s.attempt.expiresAt), ""}
		if s.attempt.expiresAt.IsZero() || time.Now().Before(s.attempt.expiresAt) {
			if s.attempt.url != "" {
				lines = append(lines, "Open this provider URL in your browser:", accountText(s.attempt.url))
			}
			if s.attempt.code != "" {
				lines = append(lines, "Device code: "+accountText(s.attempt.code))
			}
		} else {
			lines = append(lines, "Login expired; cancel and start again.")
		}
		lines = append(lines, "", "c: cancel login · Esc: cancel and close")
		if s.secretInput() {
			input := "(empty)"
			if len(s.secret) > 0 {
				input = "•••••••• (hidden)"
			}
			lines = []string{"Claude machine login", "", "In a trusted host terminal run: claude setup-token", "Paste its token below. Do not paste it into an agent conversation.", "", "Token: " + input, "", "Enter: submit · Ctrl+U: clear · Esc: cancel and close", "Expires: " + accountTime(s.attempt.expiresAt)}
			if s.attempt.method == "api-key" {
				lines = []string{accountHarnessName(s.harness) + " API key login", "", "Paste the API key below. Input stays hidden.", "Key: " + input, "", "Enter: submit · Ctrl+U: clear · Esc: cancel", "Expires: " + accountTime(s.attempt.expiresAt)}
			}

			if !s.attempt.expiresAt.IsZero() && !time.Now().Before(s.attempt.expiresAt) {
				lines = []string{accountHarnessName(s.harness) + " machine login expired", "Esc: close and start again"}
			}
		}
	}
	if s.pending == "status" && len(s.snapshot.profiles) == 0 {
		lines = []string{"Machine accounts", "Loading account status…"}
	}
	var notices []string
	if s.pending != "" {
		notices = append(notices, "Waiting for machine: "+s.pending+"…")
	}
	if s.notice != "" {
		notices = append(notices, s.notice)
	}
	// Wrap instead of truncating ceremony URLs. Scrolling keeps every part of a
	// long URL reachable on narrow terminals without emitting terminal escapes.
	var wrapped []string
	for _, line := range lines {
		wrapped = append(wrapped, strings.Split(ansi.Hardwrap(line, w, true), "\n")...)
	}
	// Keep failures and progress visible even when the account details scroll.
	var noticeLines []string
	for _, line := range notices {
		noticeLines = append(noticeLines, strings.Split(ansi.Hardwrap(line, w, true), "\n")...)
	}
	if len(noticeLines) > max(1, h/2) {
		noticeLines = noticeLines[:max(1, h/2)]
	}
	rows := max(1, h-len(noticeLines))
	s.scroll = min(s.scroll, max(0, len(wrapped)-rows))
	end := min(len(wrapped), s.scroll+rows)
	body := strings.Join(append(noticeLines, wrapped[s.scroll:end]...), "\n")
	body = lipgloss.NewStyle().Width(w).Height(h).MaxWidth(w).MaxHeight(h).Render(body)
	footer := fmt.Sprintf("PgUp/PgDn scroll · Esc close  %d–%d/%d", s.scroll+1, end, len(wrapped))
	if s.pending == "" && s.confirm == nil && !s.attempt.active() {
		footer = fmt.Sprintf("↑↓/jk select · PgUp/PgDn scroll · Esc close  %d–%d/%d", s.scroll+1, end, len(wrapped))
		if w < 60 {
			footer = fmt.Sprintf("↑↓/jk select · PgUp/Dn · Esc  %d–%d/%d", s.scroll+1, end, len(wrapped))
		}
	}
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(accent).
		Padding(0, 1).Render(cellPad(titleStyle.Render("Accounts · remaining budget"), w) + "\n" + body + "\n" + cellPad(dimStyle.Render(footer), w))
}

func accountConfirmation(r accountRequest, p *accountProfile) string {
	label := r.harness
	if p != nil {
		label += " / " + accountText(p.label)
	}
	switch r.op {
	case "logout":
		return "Log out " + label + "?\n\nFuture credential delivery stops for every workload. Already delivered tokens may remain valid upstream. Running workloads are not stopped."
	default:
		return "Start " + r.loginMode + " login for " + label + "?\n\nLogin temporarily pauses new credential delivery. Every workload uses the new login, account or mode from its next launch; running workloads keep their current login and are not stopped. Follow the login instructions; Esc cancels the attempt."
	}
}

func (s *accountsState) clearSecret() {
	for i := range s.secret {
		s.secret[i] = 0
	}
	s.secret = nil
}

var (
	errAccountsUnsupported = errors.New("accounts unsupported")
	errAccountsDenied      = errors.New("accounts denied")
	errAccountsConflict    = errors.New("accounts revision conflict")
	errAccountsNative      = errors.New("account native adapter unavailable")
	errAccountsSecret      = errors.New("account token rejected")
	errAccountsAttempt     = errors.New("account attempt unavailable")
)

func safeAccountError(err error) error {
	for _, safe := range []error{errAccountsUnsupported, errAccountsDenied, errAccountsConflict, errAccountsNative, errAccountsSecret, errAccountsAttempt} {
		if errors.Is(err, safe) {
			return safe
		}
	}
	return errors.New("account service unavailable")
}

func accountErrorMessage(err error, op string) string {
	switch {
	case errors.Is(err, errAccountsUnsupported):
		return "Machine account service unavailable or unsupported. Upgrade/reconnect and refresh."
	case errors.Is(err, errAccountsDenied):
		return "Account access denied. Reconnect with an authorized operator account."
	case errors.Is(err, errAccountsConflict):
		return "Account changed elsewhere. Refresh and review before trying again."
	case errors.Is(err, errAccountsNative):
		return "Native login adapter unavailable. Check the installed harness and refresh."
	case errors.Is(err, errAccountsSecret):
		return "Credential rejected. Start login again and paste only the requested token or API key."
	case errors.Is(err, errAccountsAttempt):
		return "Login attempt expired or unavailable. Refresh before starting again."
	case op == "status" || op == "progress":
		return "Account service unavailable. Displayed data is stale; refresh to reconnect."
	default:
		return "Operation unconfirmed. Refresh to reconcile before trying again."
	}
}

func (s *accountsState) dispose() {
	s.clearSecret()
	s.attempt = accountAttempt{}
	if s.broker != nil {
		s.broker.close()
		s.broker = nil
	}
}

// accountsSize leaves the surrounding pane visible at ordinary terminal sizes.
func (m *model) accountsSize() (int, int) {
	return max(6, min(76, m.w-4)), max(6, min(26, m.h-4))
}

// Compact enough for both providers to remain visible on an 80x24 terminal.
func accountUsageSummary(l accountLimit, now time.Time, stale bool) string {
	remaining, state := accountRemaining(l, now, stale)
	name := accountWindowName(l)
	reset := "unknown"
	if !l.resetsAt.IsZero() {
		reset = l.resetsAt.UTC().Format("01-02 15:04Z")
	}
	if state == "unknown" {
		return "  " + name + ": " + state + " · reset " + reset
	}
	filled := int(remaining / 10)
	bar := strings.Repeat("━", filled) + strings.Repeat("─", 10-filled)
	style := keyStyle
	if remaining <= 20 {
		style = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	}
	freshness := "fresh"
	if state == "stale" {
		style = dimStyle
		freshness = strings.TrimSpace("stale " + accountAge(l.observedAt, now))
	}
	return fmt.Sprintf("  %s %s %s · reset %s · %s", name, style.Render(accountPercent(remaining)+" left"), style.Render(bar), reset, freshness)
}
