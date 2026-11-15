package nativetui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// loadState is where the dashboard stands in getting its session inventory
// from the daemon. Until the first snapshot lands the rail has nothing true to
// say — an empty sessions slice then means "not loaded", not "no sessions" — so
// it shows the loading state instead of the empty-section hints.
//
// The zero value is loadReady: a model built directly (as tests do) renders its
// sessions as given. Run starts in loadConnecting.
type loadState int

const (
	loadReady      loadState = iota // a snapshot arrived on the current connection
	loadConnecting                  // dialing the daemon; no snapshot yet
	loadWaiting                     // connected; awaiting the first snapshot
	loadOffline                     // the daemon is unreachable and no snapshot has arrived; retrying
	loadStale                       // the connection dropped after a snapshot; showing the last one while reconnecting
)

// loading reports whether there is no snapshot to show yet.
func (m *model) loading() bool {
	return m.load == loadConnecting || m.load == loadWaiting || m.load == loadOffline
}

// onConnected advances the load state for a fresh connection: the inventory
// is still to come, but a stale rail keeps showing until it does.
func (m *model) onConnected() {
	m.loadErr = ""
	if m.loading() {
		m.load = loadWaiting
	}
}

// onDisconnected records a failed dial or a dropped connection. Before any
// snapshot that is "daemon unreachable"; after one the rail keeps its last
// inventory, marked stale.
func (m *model) onDisconnected(err error) {
	m.loadErr = ""
	if err != nil {
		m.loadErr = err.Error()
	}
	if m.loading() {
		m.load = loadOffline
	} else {
		m.load = loadStale
	}
}

// onSnapshot marks the inventory as loaded; an empty one is now a genuine
// empty result and renders the normal empty-section hints.
func (m *model) onSnapshot() {
	m.load, m.loadErr = loadReady, ""
}

// loadingTitle is the one-line headline for the current load state, or "".
func (m *model) loadingTitle() string {
	switch m.load {
	case loadConnecting:
		return "Connecting to daemon…"
	case loadWaiting:
		return "Loading sessions…"
	case loadOffline:
		return "Daemon unreachable — retrying…"
	case loadStale:
		return "Daemon offline — reconnecting…"
	}
	return ""
}

// loadingRail is the rail body shown in place of the session list while
// loading: the headline, and the last connection error when there is one.
func (m *model) loadingRail() []string {
	lines := []string{"", dimStyle.Render(" " + truncate(m.loadingTitle(), sidebarWidth-1))}
	if m.load == loadOffline && m.loadErr != "" {
		lines = append(lines, dimStyle.Render(" "+truncate(m.loadErr, sidebarWidth-1)))
	}
	return lines
}

// renderLoadingMain is the main-pane placeholder while there is no snapshot,
// or while a stale rail waits on a reconnect with no pane open.
func (m *model) renderLoadingMain() string {
	lines := []string{m.loadingTitle()}
	if m.loadErr != "" && (m.load == loadOffline || m.load == loadStale) {
		lines = append(lines, truncate(m.loadErr, m.mainWidth()))
	}
	return lipgloss.NewStyle().
		Width(m.mainWidth()).Height(m.paneRows()).
		Align(lipgloss.Center, lipgloss.Center).
		Render(dimStyle.Render(strings.Join(lines, "\n")))
}
