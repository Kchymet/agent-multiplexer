package nativetui

import (
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"amux/internal/core"
)

// Frame caching. Bubble Tea calls View after every message, and the daemon
// streams a message for every chunk of output of every open pane — hidden
// tabs and agents included — so an idle-looking TUI re-rendered its whole rail
// (hundreds of rows through lipgloss) many times a second.
//
// Pane bytes never change the screen by themselves: Feed hands them to the
// terminal's parsing goroutine, which then signals termDataMsg. So a frame is
// classified by what it can have changed:
//
//   - pane output/reset frames (frameDataOnly) change nothing visible;
//   - termDataMsg changes the main pane only if the shown terminal parsed new
//     output (shownData), and the rail only if a reap changed the layout;
//   - every other message may change anything, and invalidates both.
//
// View then returns the previous frame when nothing is invalid, and reuses the
// rendered rail when only the main pane changed.
//
// Render hooks: anything that changes what the rail or chrome shows from
// outside an ordinary Update path (a background sampler's result, a periodic
// refresh) must arrive as a tea.Msg — any message that is not pane data
// already invalidates the frame — or call invalidateRail / invalidateView
// from Update. Data shown in the rail that ages with wall time needs no hook
// below a minute: the rail cache also expires each minute.
type frameCache struct {
	// enabled is set by Run. Tests that build a model and mutate it directly
	// render every View from scratch.
	enabled bool
	// viewValid: lastView is still what the screen should show. railValid:
	// rail is the sidebar for the current state at railKey.
	viewValid, railValid bool
	rail                 string
	railKey              railCacheKey
	// shownData is set by a terminal's parsing goroutine when the terminal on
	// screen parsed new output.
	shownData atomic.Bool
}

type railCacheKey struct {
	w, h   int
	minute int64
}

// invalidateRail marks the rail (and so the frame) for re-rendering.
func (m *model) invalidateRail() { m.frames.railValid, m.frames.viewValid = false, false }

// invalidateView marks the frame for re-rendering, keeping the rendered rail.
func (m *model) invalidateView() { m.frames.viewValid = false }

// frameDataOnly reports a daemon frame that carries only pane bytes.
func frameDataOnly(msg tea.Msg) bool {
	f, ok := msg.(frameMsg)
	if !ok || f.f.Snapshot != nil || f.f.Result != nil || f.f.Pane == nil {
		return false
	}
	return f.f.Pane.Type == core.FramePaneOutput || f.f.Pane.Type == core.FramePaneReset
}

// layoutMark is what a data message could change through a reap or a cleared
// selection; a change in it invalidates the whole frame.
type layoutMark struct {
	attached     string
	tab, terms   int
	focus        focus
	selection    *paneSelection
	pending      string
	byPaneLength int
}

func (m *model) layoutMark() layoutMark {
	return layoutMark{attached: m.attached, tab: m.tab, terms: len(m.terms), focus: m.focus,
		selection: m.selection, pending: m.pending, byPaneLength: len(m.byPane)}
}

// sidebar is renderSidebar through the rail cache.
func (m *model) sidebar() string {
	key := railCacheKey{w: m.w, h: m.h, minute: time.Now().Unix() / 60}
	if m.frames.enabled && m.frames.railValid && m.frames.railKey == key {
		return m.frames.rail
	}
	rail := m.renderSidebar()
	if m.frames.enabled {
		m.frames.rail, m.frames.railKey, m.frames.railValid = rail, key, true
	}
	return rail
}
