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
	account, plan, identitySource   string
	revision                        uint64
	defaultProfile                  bool
	canLogin, canLogout, canDefault bool
	loginHelp                       string
	workloads                       *int
	limits                          []accountLimit
}

type accountLimit struct {
	name, state, source, confidence, reason string
	usedPercent                             *float64
	observedAt, freshUntil, resetsAt        time.Time
}

type accountSnapshot struct {
	profiles  []accountProfile
	canManage bool
}

type accountAttempt struct {
	id, state, method, url, code string
	expiresAt                    time.Time
}

func (a accountAttempt) active() bool {
	return a.id != "" && (a.state == "pending" || a.state == "starting" || a.state == "waiting")
}

type accountRequest struct {
	op, harness, profile, attempt string
	secret                        string // write-only; cleared before returning a reply
	revision                      uint64
}

type accountResponse struct {
	snapshot *accountSnapshot
	attempt  *accountAttempt
}

type accountCaller func(context.Context, accountRequest) (accountResponse, error)

type accountsState struct {
	snapshot accountSnapshot
	harness  string
	selected string // stable profile identity, never a list offset
	attempt  accountAttempt
	pending  string
	notice   string
	stale    bool
	confirm  *accountRequest
	scroll   int
	serial   uint64
	secret   []rune // masked Claude setup-token input, never a generic form field
	broker   *machineAccountTransport
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

func (s *accountsState) profiles() []accountProfile {
	var out []accountProfile
	for _, p := range s.snapshot.profiles {
		if p.harness == s.harness {
			out = append(out, p)
		}
	}
	return out
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
		s.stale = false
		s.selectProfile()
	}
	if a := msg.response.attempt; a != nil {
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
			s.notice = "Login complete. Use d to select this profile for new workloads."
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
	case "default":
		s.notice = "Default updated for new workloads. Existing bindings are unchanged."
	case "logout":
		s.notice = "Logout requested. Refresh status to check local cleanup."
	}
	if s.attempt.active() && s.attempt.method != "setup-token" {
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
	if k.Paste && !(s.attempt.active() && s.attempt.method == "setup-token" && s.pending == "") {
		return m, nil
	}
	secretInput := s.attempt.active() && s.attempt.method == "setup-token"
	if k.String() == "esc" || k.String() == "ctrl+c" || (k.String() == "q" && !secretInput) {
		if s.confirm != nil {
			s.confirm = nil
			return m, nil
		}
		if s.pending != "" && s.pending != "status" && s.pending != "progress" {
			m.status = "Account operation unconfirmed; reopen settings to reconcile."
		}
		s.dispose() // socket close cancels all ceremonies, including a lost start reply
		m.accounts = nil
		return m, nil
	}
	if s.pending != "" {
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
	if k.String() == "pgdown" || k.String() == "pgup" {
		if k.String() == "pgdown" {
			s.scroll += max(1, m.paneRows()-5)
		} else {
			s.scroll = max(0, s.scroll-max(1, m.paneRows()-5))
		}
		return m, nil
	}
	if s.attempt.active() {
		if s.attempt.method == "setup-token" {
			if !s.attempt.expiresAt.IsZero() && !time.Now().Before(s.attempt.expiresAt) {
				s.clearSecret()
				s.notice = "Login expired. Press Esc to close and start again."
				return m, nil
			}
			switch k.Type {
			case tea.KeyEnter:
				if len(s.secret) == 0 {
					s.notice = "Paste the setup token first. Input stays hidden."
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
					s.notice = "Token too long. Input cleared; paste only the setup token."
					return m, nil
				}
				for _, r := range input {
					if r <= ' ' || r >= 127 {
						s.clearSecret()
						s.notice = "Invalid token input. Paste only the setup token."
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
	case "tab", "shift+tab", "left", "right", "h", "l":
		if s.harness == "claude" {
			s.harness = "codex"
		} else {
			s.harness = "claude"
		}
		s.selected, s.scroll = "", 0
		s.selectProfile()
	case "up", "k", "down", "j":
		profiles := s.profiles()
		for i, p := range profiles {
			if p.id == s.selected {
				delta := 1
				if k.String() == "up" || k.String() == "k" {
					delta = -1
				}
				s.selected = profiles[(i+delta+len(profiles))%len(profiles)].id
				s.scroll = 0
				break
			}
		}
	case "r":
		return m, m.requestAccounts(accountRequest{op: "status"})
	case "e", "enter", "d", "x":
		if s.stale || !s.snapshot.canManage {
			s.notice = "Account management unavailable. Refresh status before making changes."
			return m, nil
		}
		p := s.profile()
		switch k.String() {
		case "e", "enter":
			if p != nil && p.canLogin {
				req := s.request("login")
				s.confirm = &req
			} else if p != nil && p.canDefault && p.mode != "central" && k.String() == "e" {
				req := s.request("default")
				s.confirm = &req
			} else {
				s.notice = "Login unavailable here; inspect the profile's login instructions."
			}
		case "d":
			if p != nil && p.canDefault && !p.defaultProfile {
				req := s.request("default")
				s.confirm = &req
			}
		case "x":
			if p != nil && p.canLogout {
				req := s.request("logout")
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
	if state == "available" && (stale || l.freshUntil.IsZero() || !now.Before(l.freshUntil) || l.observedAt.After(now)) {
		state = "stale"
	}
	lines := []string{accountText(l.name) + ": " + accountText(state)}
	if state == "available" || state == "stale" {
		used := "unknown"
		if l.usedPercent != nil {
			used = accountValue(l.usedPercent) + "%"
		}
		lines = append(lines, "  Used: "+used+" of the reported window")
	}
	lines = append(lines, "  Source: "+accountText(l.source)+"  Confidence: "+accountText(l.confidence),
		"  Observed: "+accountTime(l.observedAt)+"  Resets: "+accountTime(l.resetsAt))
	if l.reason != "" {
		lines = append(lines, "  "+accountText(l.reason))
	}
	return lines
}

func (m *model) renderAccounts() string {
	s := m.accounts
	w, h := m.mainWidth(), m.paneRows()
	lines := []string{titleStyle.Render("Machine accounts · " + s.harness), "Tab: Claude / Codex · ↑/↓: profile · PgUp/PgDn: scroll", ""}
	if s.stale {
		lines = append(lines, "STALE — refresh required before changes")
	}
	for _, p := range s.profiles() {
		mark := "  "
		if p.id == s.selected {
			mark = "› "
		}
		label := accountText(p.label)
		if label == "" {
			label = accountText(p.id)
		}
		if p.defaultProfile {
			label += " [machine default]"
		}
		lines = append(lines, mark+label+" · "+accountText(p.mode)+" · "+accountText(p.state))
	}
	if p := s.profile(); p != nil {
		identity := accountText(p.account)
		if identity == "" {
			identity = "unknown (not verified)"
		}
		plan := accountText(p.plan)
		if plan == "" {
			plan = "unknown"
		}
		lines = append(lines, "", "Account: "+identity, "Plan: "+plan+" · Identity source: "+accountText(p.identitySource))
		if p.workloads != nil {
			lines = append(lines, fmt.Sprintf("Bound workloads: %d", *p.workloads))
		}
		if p.loginHelp != "" {
			lines = append(lines, accountText(p.loginHelp))
		}
		lines = append(lines, "", "Subscription limits")
		if len(p.limits) == 0 {
			lines = append(lines, "Unavailable — no account-attributed observation.", "Unknown limits do not mean unlimited usage.")
		}
		for _, l := range p.limits {
			lines = append(lines, accountLimitLines(l, time.Now(), s.stale)...)
		}
	} else {
		lines = append(lines, "No profiles advertised by this machine. Press r to refresh.")
	}
	lines = append(lines, "", "New workloads use the machine default; existing bindings stay fixed.")
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
		if s.attempt.method == "setup-token" {
			input := "(empty)"
			if len(s.secret) > 0 {
				input = "•••••••• (hidden)"
			}
			lines = []string{"Claude machine login", "", "In a trusted host terminal run: claude setup-token", "Paste its token below. Do not paste it into an agent conversation.", "", "Token: " + input, "", "Enter: submit · Ctrl+U: clear · Esc: cancel and close", "Expires: " + accountTime(s.attempt.expiresAt)}
			if !s.attempt.expiresAt.IsZero() && !time.Now().Before(s.attempt.expiresAt) {
				lines = []string{"Claude machine login expired", "Esc: close and start again"}
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
		wrapped = append(wrapped, strings.Split(ansi.Hardwrap(line, max(1, w-2), true), "\n")...)
	}
	// Keep failures and progress visible even when the account details scroll.
	var noticeLines []string
	for _, line := range notices {
		noticeLines = append(noticeLines, strings.Split(ansi.Hardwrap(line, max(1, w-2), true), "\n")...)
	}
	if len(noticeLines) > max(1, h/2) {
		noticeLines = noticeLines[:max(1, h/2)]
	}
	rows := max(1, h-1-len(noticeLines))
	s.scroll = min(s.scroll, max(0, len(wrapped)-rows))
	end := min(len(wrapped), s.scroll+rows)
	body := strings.Join(append(noticeLines, wrapped[s.scroll:end]...), "\n")
	return lipgloss.NewStyle().Width(w).Height(h).MaxWidth(w).MaxHeight(h).Render(body)
}

func accountModeHelp(mode string) string {
	switch mode {
	case "host":
		return "Uses the host login. External account changes affect this source."
	case "shared":
		return "Uses a shared Claude login. External account changes affect this source."
	case "environment":
		return "Credentials come from the launch environment; amux cannot verify their account."
	default:
		return "The machine broker owns login and supplies credentials to authorized workloads."
	}
}

func accountConfirmation(r accountRequest, p *accountProfile) string {
	label := r.harness
	if p != nil {
		label += " / " + accountText(p.label)
	}
	switch r.op {
	case "logout":
		return "Log out " + label + "?\n\nFuture credential delivery stops for workloads bound to this profile. Already delivered tokens may remain valid upstream. Workloads will not switch accounts automatically."
	case "default":
		return "Use " + label + " as the machine default?\n\nOnly new workloads use this default. Existing workload account bindings stay fixed."
	default:
		return "Start login for " + label + "?\n\nComplete the provider's browser/device flow. Esc cancels the attempt."
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
		return "Token rejected. Start login again and paste only the setup token."
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
