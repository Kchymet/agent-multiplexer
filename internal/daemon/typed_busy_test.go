package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"amux/internal/composer"
	"amux/internal/composer/panefixture"
	"amux/internal/core"
	"amux/internal/prwatch"
	"amux/internal/vterm"
)

// fixturePane simulates a runtime's composer and paints it with the captured
// or reconstructed pane fixtures (internal/composer/testdata/panes): whatever
// the composer holds selects the screen, so delivery reads real layouts —
// spinners, tool output, queued previews, dialogs — through the production
// parser.
type fixturePane struct {
	mu sync.Mutex
	// empty is painted while the composer is empty; draft while it holds
	// draft.WantText; typed while it holds amux's text (when echo is set).
	empty, draft, typed *panefixture.Fixture
	echo                bool
	// shows, when set, is the text whose presence paints typed (a runtime
	// that collapses it shows a token instead of the words).
	shows     string
	composer  string
	submitted []string
	erased    int
	human     time.Time
}

func (p *fixturePane) apply(b []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := 0; i < len(b); i++ {
		switch {
		case b[i] == '\\' && i+1 < len(b) && b[i+1] == '\r':
			p.composer += "\n"
			i++
		case b[i] == '\r':
			p.submitted = append(p.submitted, p.composer)
			p.composer = ""
		case b[i] == 0x7f:
			if r := []rune(p.composer); len(r) > 0 {
				p.composer = string(r[:len(r)-1])
			}
			p.erased++
		case b[i] == 0x1b:
		default:
			p.composer = string(append([]byte(p.composer), b[i]))
		}
	}
}

func (p *fixturePane) screen() vterm.Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	f := p.empty
	switch {
	case p.composer == "":
	case p.typed != nil && p.echo && composer.Same(p.composer, p.showText()):
		f = p.typed
	case p.draft != nil && composer.Same(p.composer, p.draft.WantText):
		f = p.draft
	case p.typed != nil && !p.echo && composer.Same(p.composer, p.showText()):
		// The runtime has not painted amux's text (yet): the empty screen.
	default:
		// Content no fixture shows (amux's text plus someone's words): paint
		// it in the runtime's plain composer layout so it reads as a draft.
		body := strings.ReplaceAll(p.composer, "\n", "\r\n  ")
		if f.Runtime == composer.RuntimeCodex {
			return vterm.Capture([]byte("\x1b[2J\x1b[H• Working (3s • esc to interrupt)\r\n\r\n› "+body+"\r\n\r\n\x1b[2m  ? for shortcuts\x1b[0m"), f.Cols, f.Rows)
		}
		rule := strings.Repeat("─", f.Cols)
		return vterm.Capture([]byte("\x1b[2J\x1b[H✻ Wrangling… (3s · esc to interrupt)\r\n\r\n"+rule+"\r\n❯ "+body+"\r\n"+rule+"\r\n\x1b[2m  ⏵⏵ auto mode on\x1b[0m"), f.Cols, f.Rows)
	}
	return f.Snapshot()
}

func (p *fixturePane) showText() string {
	if p.shows != "" {
		return p.shows
	}
	return p.typed.WantText
}

func (p *fixturePane) state() (string, []string, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.composer, append([]string(nil), p.submitted...), p.erased
}

func mustFixture(t *testing.T, name string) *panefixture.Fixture {
	t.Helper()
	if name == "" {
		return nil
	}
	f, err := panefixture.Load(name)
	if err != nil {
		t.Fatal(err)
	}
	return &f
}

// TestTypedDeliveryAgainstPaneFixtures is the regression table for steering
// busy agents: pane fixture -> what delivery does. A free composer — idle or
// mid-turn, with a spinner, a running tool, a queued message or a labeled
// frame — takes the text and submits it. A draft or a dialog refuses before
// anything is typed, queues the steer, and leaves the pane exactly as it was.
// amux's own leftover text is recognized, cleared and replaced. Text that never
// shows is taken back, so the pane is left clean.
func TestTypedDeliveryAgainstPaneFixtures(t *testing.T) {
	claudeText := mustFixture(t, "claude-busy-amux-typed").WantText
	codexText := mustFixture(t, "codex-busy-draft").WantText
	const (
		submit       = "submit"
		refuseDraft  = "refuse: draft"
		refuseDialog = "refuse: dialog"
		takeBack     = "take back"
	)
	for _, tc := range []struct {
		name, runtime       string
		empty, draft, typed string
		start               string // composer content before delivery
		leftover            bool   // amux remembers start as its own text
		leftText            string // the remembered leftover's text, when it is not start
		text                string // the steer text, when not the runtime default
		noEcho              bool   // the typed text never shows on screen
		want                string
	}{
		{name: "claude idle", runtime: "claude", empty: "claude-idle-empty", typed: "claude-busy-amux-typed", want: submit},
		{name: "claude mid-turn spinner", runtime: "claude", empty: "claude-busy-spinner-empty", typed: "claude-busy-amux-typed", want: submit},
		{name: "claude tool running", runtime: "claude", empty: "claude-busy-tool-running", typed: "claude-busy-amux-typed", want: submit},
		{name: "claude queued message shown", runtime: "claude", empty: "claude-busy-queued-message", typed: "claude-busy-amux-typed", want: submit},
		{name: "claude labeled frame", runtime: "claude", empty: "claude-busy-labeled-rule", typed: "claude-busy-amux-typed", want: submit},
		{name: "claude idle draft", runtime: "claude", empty: "claude-idle-empty", draft: "claude-idle-draft", start: mustFixture(t, "claude-idle-draft").WantText, want: refuseDraft},
		{name: "claude mid-turn draft", runtime: "claude", empty: "claude-busy-spinner-empty", draft: "claude-busy-spinner-draft", start: "also check the flaky test", want: refuseDraft},
		{name: "claude permission dialog", runtime: "claude", empty: "claude-permission-dialog", want: refuseDialog},
		{name: "claude auto-mode dialog", runtime: "claude", empty: "claude-auto-mode-dialog", want: refuseDialog},
		{name: "claude amux leftover recognized", runtime: "claude", empty: "claude-busy-spinner-empty", draft: "claude-busy-amux-typed", typed: "claude-busy-amux-typed", start: claudeText, leftover: true, want: submit},
		{name: "claude same text, not remembered as amux's", runtime: "claude", empty: "claude-busy-spinner-empty", draft: "claude-busy-amux-typed", start: claudeText, want: refuseDraft},
		{name: "claude text never shows", runtime: "claude", empty: "claude-busy-spinner-empty", typed: "claude-busy-amux-typed", noEcho: true, want: takeBack},
		{name: "codex mid-turn (real)", runtime: "codex", empty: "codex-real-busy-empty", typed: "codex-busy-draft", want: submit},
		{name: "codex streaming (real)", runtime: "codex", empty: "codex-real-busy-streaming", typed: "codex-busy-draft", want: submit},
		{name: "codex queued message shown", runtime: "codex", empty: "codex-busy-queued", typed: "codex-busy-draft", want: submit},
		{name: "codex idle draft (real)", runtime: "codex", empty: "codex-real-busy-empty", draft: "codex-real-idle-draft", start: "DRAFT-WHEEL-MUST-STAYx", want: refuseDraft},
		{name: "codex mid-turn draft", runtime: "codex", empty: "codex-real-busy-empty", draft: "codex-busy-draft", start: codexText, want: refuseDraft},
		{name: "codex approval dialog", runtime: "codex", empty: "codex-approval-dialog", want: refuseDialog},
		{name: "codex text never shows", runtime: "codex", empty: "codex-real-busy-empty", typed: "codex-busy-draft", noEcho: true, want: takeBack},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, eng := steerDaemon(t)
			fastTyped(d)
			putSession(t, "a1", tc.runtime)
			in := eng.running("a1")
			pane := &fixturePane{empty: mustFixture(t, tc.empty), draft: mustFixture(t, tc.draft), typed: mustFixture(t, tc.typed),
				echo: !tc.noEcho, composer: tc.start}
			in.fx = pane
			text := claudeText
			if tc.runtime == "codex" {
				text = codexText
			}
			if tc.text != "" {
				text, pane.shows = tc.text, tc.text
			}
			if tc.leftover {
				left := tc.start
				if tc.leftText != "" {
					left = tc.leftText
				}
				d.rememberLeftover("a1", typedLeftover{instance: in, text: left, runes: len([]rune(left))})
			}

			res := d.handle(context.Background(), core.Action{Action: core.ActionSteer, ID: "a1", Fields: map[string]string{
				core.SteerVerb: core.SteerInterject, core.SteerText: text,
			}})
			composerText, submitted, erased := pane.state()
			switch tc.want {
			case submit:
				if !res.OK || len(submitted) != 1 || !composer.Same(submitted[0], text) || composerText != "" {
					t.Fatalf("result=%+v submitted=%q composer=%q, want exactly amux's text submitted", res, submitted, composerText)
				}
				if _, ok := d.leftover("a1", in); ok {
					t.Fatal("a leftover is still remembered after a clean delivery")
				}
			case refuseDraft, refuseDialog:
				word := map[string]string{refuseDraft: "draft", refuseDialog: "dialog"}[tc.want]
				if res.OK || res.Code != codeComposerBusy || !strings.Contains(res.Error, word) || !strings.Contains(res.Error, "queued") {
					t.Fatalf("result=%+v, want a queued composer_busy refusal naming the %s", res, word)
				}
				if composerText != tc.start || len(submitted) != 0 || erased != 0 || strings.Contains(in.written(), text) {
					t.Fatalf("pane touched: composer=%q submitted=%q erased=%d written=%q", composerText, submitted, erased, in.written())
				}
			case takeBack:
				if res.OK || res.Code != codeComposerBusy || !strings.Contains(res.Error, "took it back") || !strings.Contains(res.Error, "nothing of amux's is left") {
					t.Fatalf("result=%+v, want a clear refusal that took amux's text back", res)
				}
				if composerText != "" || len(submitted) != 0 || erased != len([]rune(text)) {
					t.Fatalf("composer=%q submitted=%q erased=%d, want a clean pane (erased exactly %d)", composerText, submitted, erased, len([]rune(text)))
				}
			}
		})
	}
}

// paneSnapshot is everything a test can observe about the pane's input: what
// the composer holds and what the input region reads as.
func paneSnapshot(p *fixturePane, runtime string) string {
	c, _, _ := p.state()
	cl := composer.Classify(runtime, p.screen())
	return fmt.Sprintf("composer=%q region=%s/%q", c, cl.State, cl.Text)
}

// The self-perpetuating failure seen across coordinators: each failed steer
// left amux text in the box, and the next steer read it as a user draft. Each
// sequence here starts from a busy pane, has a steer fail or defer, then
// retries (a CLI resend and/or the deferral queue's flush, repeatedly) and
// requires: the failed attempt leaves the input region exactly as it was, the
// text is delivered exactly once, and nothing is left in the box.
func TestBusySteerSequenceDeliversOnceWithNoResidue(t *testing.T) {
	typedFx := mustFixture(t, "claude-busy-amux-typed")
	text := typedFx.WantText
	userDraft := mustFixture(t, "claude-busy-spinner-draft").WantText
	queueWake := prwatch.Wake{Kind: prwatch.KindSteer, Workgroup: steerQueueGroup("a1"), SessionID: "a1", State: core.SteerInterject, Text: text}
	steer := func(d *Daemon) core.Result {
		return d.handle(context.Background(), core.Action{Action: core.ActionSteer, ID: "a1", Fields: map[string]string{
			core.SteerVerb: core.SteerInterject, core.SteerText: text,
		}})
	}
	flush := func(d *Daemon) prwatch.Delivery {
		return d.deliverPRWake(context.Background(), steerQueueGroup("a1"), []prwatch.Wake{queueWake})[0]
	}
	for _, tc := range []struct {
		name  string
		pane  func() *fixturePane
		free  func(*fixturePane) // what makes the agent able to take it
		retry bool               // a CLI resend after freeing, before the queue flush
	}{
		{
			name: "text never shows mid-turn, then a CLI resend",
			pane: func() *fixturePane {
				return &fixturePane{empty: mustFixture(t, "claude-busy-spinner-empty"), typed: typedFx}
			},
			free:  func(p *fixturePane) { p.mu.Lock(); p.echo = true; p.mu.Unlock() },
			retry: true,
		},
		{
			name: "text never shows mid-turn, then the queue delivers",
			pane: func() *fixturePane {
				return &fixturePane{empty: mustFixture(t, "claude-busy-spinner-empty"), typed: typedFx}
			},
			free: func(p *fixturePane) { p.mu.Lock(); p.echo = true; p.mu.Unlock() },
		},
		{
			name: "user draft mid-turn, then the user sends it",
			pane: func() *fixturePane {
				return &fixturePane{empty: mustFixture(t, "claude-busy-spinner-empty"), draft: mustFixture(t, "claude-busy-spinner-draft"),
					typed: typedFx, echo: true, composer: userDraft}
			},
			free: func(p *fixturePane) { p.apply([]byte("\r")) },
		},
		{
			name: "permission dialog, then it is answered",
			pane: func() *fixturePane {
				return &fixturePane{empty: mustFixture(t, "claude-permission-dialog"), typed: typedFx, echo: true}
			},
			free: func(p *fixturePane) { p.mu.Lock(); p.empty = mustFixture(t, "claude-busy-tool-running"); p.mu.Unlock() },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, eng := steerDaemon(t)
			fastTyped(d)
			putSession(t, "a1", "claude")
			in := eng.running("a1")
			pane := tc.pane()
			in.fx = pane
			_, before, _ := pane.state()
			startSubmits := len(before)

			// 1. The steer fails or defers: nothing changes in the input region.
			was := paneSnapshot(pane, "claude")
			res := steer(d)
			if res.OK || res.Code != codeComposerBusy || !strings.Contains(res.Error, "queued") || !strings.Contains(res.Error, "nothing of amux's is left") {
				t.Fatalf("busy steer = %+v, want a queued composer_busy refusal that left nothing behind", res)
			}
			if now := paneSnapshot(pane, "claude"); now != was {
				t.Fatalf("failed steer changed the input region:\n before %s\n after  %s", was, now)
			}
			// Retries while still busy pile nothing up: the box stays as it was.
			for i := 0; i < 3; i++ {
				if r := steer(d); r.OK || r.Code != codeComposerBusy {
					t.Fatalf("resend %d while busy = %+v", i, r)
				}
				if got := flush(d); got != prwatch.Deferred {
					t.Fatalf("flush %d while busy = %v, want Deferred", i, got)
				}
				if now := paneSnapshot(pane, "claude"); now != was {
					t.Fatalf("retry %d changed the input region:\n before %s\n after  %s", i, was, now)
				}
			}

			// 2. The agent can take it now; the text goes out once, however it is retried.
			tc.free(pane)
			_, freed, _ := pane.state()
			startSubmits = len(freed)
			if tc.retry {
				if r := steer(d); !r.OK {
					t.Fatalf("resend once free = %+v", r)
				}
			}
			for i := 0; i < 3; i++ {
				got := flush(d)
				if want := prwatch.Delivered; i > 0 || tc.retry {
					want = prwatch.Dropped // already delivered: never twice
					if got != want {
						t.Fatalf("flush %d = %v, want %v", i, got, want)
					}
				} else if got != want {
					t.Fatalf("first flush once free = %v, want Delivered", got)
				}
			}
			c, submitted, _ := pane.state()
			mine := submitted[startSubmits:]
			if len(mine) != 1 || !composer.Same(mine[0], text) {
				t.Fatalf("delivered %q, want amux's text exactly once", mine)
			}
			if c != "" {
				t.Fatalf("composer after delivery = %q, want empty", c)
			}
			if _, ok := d.leftover("a1", in); ok {
				t.Fatal("a leftover is still remembered")
			}
		})
	}
}

// Someone types into the box while amux's text is in it: amux leaves the box
// alone (it never deletes from a composer someone touched) and remembers its
// text. Once the person deletes their own addition, the box holds exactly
// amux's earlier text: the next delivery recognizes it as amux's, clears it and
// delivers cleanly — leftover text never reads as a user draft. And when
// someone typed but the box still shows only amux's text, amux takes it back.
func TestLeftoverTextNeverBlocksTheNextSteer(t *testing.T) {
	d, eng := steerDaemon(t)
	fastTyped(d)
	putSession(t, "a1", "claude")
	in := eng.running("a1")
	typed := mustFixture(t, "claude-busy-amux-typed")
	text := typed.WantText
	pane := &fixturePane{empty: mustFixture(t, "claude-busy-spinner-empty"), draft: typed, typed: typed}
	in.fx = pane
	person := func(add string) func() {
		return func() {
			in.onInput = nil
			pane.mu.Lock()
			pane.composer += add
			pane.human = time.Now()
			pane.mu.Unlock()
		}
	}

	// 1. A person adds words mid-delivery: amux's text is left and remembered.
	in.onInput = person(" and mine")
	err := d.steer(context.Background(), core.Action{ID: "a1", Fields: map[string]string{core.SteerVerb: core.SteerInterject, core.SteerText: text}})
	var busy *composerBusyError
	if !errors.As(err, &busy) || !busy.typed {
		t.Fatalf("raced delivery = %v, want composer busy with amux's text left", err)
	}
	if c, s, erased := pane.state(); len(s) != 0 || erased != 0 || !strings.HasSuffix(c, " and mine") {
		t.Fatalf("composer=%q submitted=%q erased=%d, want the person's words untouched", c, s, erased)
	}
	if _, ok := d.leftover("a1", in); !ok {
		t.Fatal("left text not remembered as amux's")
	}

	// 2. While the person's words are there, it is their draft: refused, untouched.
	err = d.steer(context.Background(), core.Action{ID: "a1", Fields: map[string]string{core.SteerVerb: core.SteerInterject, core.SteerText: text}})
	if err == nil || !strings.Contains(err.Error(), "draft") {
		t.Fatalf("delivery into amux text + the person's words = %v, want refused as a draft", err)
	}

	// 3. The person deletes their addition: the box holds exactly amux's text.
	pane.mu.Lock()
	pane.composer = strings.TrimSuffix(pane.composer, " and mine")
	pane.mu.Unlock()
	if err := d.steer(context.Background(), core.Action{ID: "a1", Fields: map[string]string{core.SteerVerb: core.SteerInterject, core.SteerText: text}}); err != nil {
		t.Fatalf("delivery over amux's own leftover: %v", err)
	}
	if c, s, _ := pane.state(); c != "" || len(s) != 1 || !composer.Same(s[0], text) {
		t.Fatalf("composer=%q submitted=%q, want the leftover cleared and the text submitted once", c, s)
	}
	if _, ok := d.leftover("a1", in); ok {
		t.Fatal("leftover still remembered after a clean delivery")
	}

	// 4. A key press that changed nothing in the box (the text never echoed
	// in time): the box shows only amux's text, so amux takes it back.
	pane.mu.Lock()
	pane.echo = false
	pane.mu.Unlock()
	in.onInput = person("")
	err = d.steer(context.Background(), core.Action{ID: "a1", Fields: map[string]string{core.SteerVerb: core.SteerInterject, core.SteerText: text}})
	if !errors.As(err, &busy) || busy.typed || !strings.Contains(err.Error(), "took its text back") {
		t.Fatalf("raced delivery with an unchanged box = %v, want amux's text taken back", err)
	}
	if c, _, _ := pane.state(); c != "" {
		t.Fatalf("composer = %q, want clean", c)
	}
}
