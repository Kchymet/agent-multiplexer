package nativetui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/creack/pty"

	"amux/internal/codexcfg"
	"amux/internal/vterm"
)

// This executes the checksum-pinned native Codex TUI, not a fake terminal or an
// App Server-only test. The model response is deterministic local SSE data; all
// terminal bytes, redraws, mouse modes, and composer edits come from real Codex.
// CI sets both variables in the existing pinned-native-runtime job, so missing
// fixture wiring is a failure there rather than a silently skipped test.
func TestCodexEmbeddedScroll(t *testing.T) {
	bin := os.Getenv("AMUX_TEST_CODEX_BIN")
	if bin == "" {
		if os.Getenv("AMUX_REQUIRE_CODEX_SCROLL") == "1" {
			t.Fatal("AMUX_TEST_CODEX_BIN is required")
		}
		t.Skip("set AMUX_TEST_CODEX_BIN to CI-pinned Codex 0.157.1")
	}
	version, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil || strings.TrimSpace(string(version)) != "codex-cli 0.157.1" {
		t.Fatalf("pinned Codex: %q, %v", version, err)
	}
	for _, mode := range []string{"always", "no-alt"} {
		t.Run(mode, func(t *testing.T) { runCodexScroll(t, bin, mode) })
	}
}

type scrollCapture struct {
	mu   sync.Mutex
	data []byte
}

func (c *scrollCapture) write(b []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data = append(c.data, b...)
}
func (c *scrollCapture) bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.data...)
}

func runCodexScroll(t *testing.T, bin, mode string) {
	home := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if r.Method != "POST" {
			http.NotFound(w, r)
			return
		}
		prefix := "CODEX-HISTORY"
		if bytes.Contains(request, []byte("DRAFT-WHEEL-MUST-STAY")) {
			prefix = "NEXT-HISTORY"
		}
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(ev map[string]any) {
			b, _ := json.Marshal(ev)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev["type"], b)
			w.(http.Flusher).Flush()
		}
		send(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_scroll"}})
		send(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "message", "id": "msg_scroll", "role": "assistant", "status": "in_progress", "content": []any{}}})
		send(map[string]any{"type": "response.content_part.added", "item_id": "msg_scroll", "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": ""}})
		var body strings.Builder
		for i := 1; i <= 100; i++ {
			line := fmt.Sprintf("%s-%03d deterministic conversation line for wheel probe.\n", prefix, i)
			body.WriteString(line)
			send(map[string]any{"type": "response.output_text.delta", "item_id": "msg_scroll", "output_index": 0, "content_index": 0, "delta": line})
			select {
			case <-time.After(80 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
		}
		item := map[string]any{"type": "message", "id": "msg_scroll", "role": "assistant", "status": "completed", "content": []map[string]any{{"type": "output_text", "text": body.String()}}}
		send(map[string]any{"type": "response.output_item.done", "item": item})
		send(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_scroll", "status": "completed", "output": []any{item}, "usage": map[string]any{"input_tokens": 20, "output_tokens": 1500, "total_tokens": 1520}}})
	}))
	t.Cleanup(server.Close)
	config := fmt.Sprintf("check_for_update_on_startup = false\nmodel_provider = 'fixture'\nmodel = 'gpt-5.6'\n[model_providers.fixture]\nname = 'fixture'\nbase_url = %q\nwire_api = 'responses'\nrequires_openai_auth = false\n[projects.%q]\ntrust_level = 'trusted'\n", server.URL, home)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	argv := []string{bin, "--no-daemon", "-s", "read-only", "-a", "on-request", "-C", home}
	if mode == "always" {
		argv = codexcfg.FullscreenTUI(argv)
	} else {
		argv = append(argv, "--no-alt-screen")
	}
	argv = append(argv, "Print 100 numbered conversation lines for an embedded terminal scroll probe; do not use tools.")
	cmd := exec.Command(argv[0], argv[1:]...)
	for _, e := range os.Environ() {
		k, _, _ := strings.Cut(e, "=")
		if !strings.HasPrefix(k, "CODEX") && k != "TERM" {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.Env = append(cmd.Env, "CODEX_HOME="+home, "TERM=xterm-256color")
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 100, Rows: 28})
	if err != nil {
		t.Fatal(err)
	}
	var raw, input scrollCapture
	term := vterm.NewRemote(100, 28, func(b []byte) { input.write(b); _, _ = f.Write(b) }, nil)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		b := make([]byte, 32768)
		for {
			n, err := f.Read(b)
			if n > 0 {
				raw.write(b[:n])
				term.Feed(b[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { _ = f.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait(); <-stopped; _ = term.Close() })
	m := railModel(1, 31)
	m.w = 127
	m.attached = "ag00"
	m.focus = focusAgent
	m.terms = map[paneKey]*vterm.Terminal{{m.attached, tabAgent}: term}
	frame := func() string { return ansi.Strip(term.Render()) }
	evidence := os.Getenv("AMUX_CODEX_SCROLL_EVIDENCE")
	save := func(name string) {
		t.Helper()
		if evidence == "" {
			return
		}
		dir := filepath.Join(evidence, mode)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		data := map[string][]byte{name + ".ansi": []byte(m.View()), name + ".txt": []byte(ansi.Strip(m.View())), "pty.raw": raw.bytes(), "input.raw": input.bytes()}
		args, _ := json.MarshalIndent(argv, "", "  ")
		data["argv.json"] = args
		for file, b := range data {
			if err := os.WriteFile(filepath.Join(dir, file), b, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	await := func(label string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(40 * time.Second)
		for time.Now().Before(deadline) {
			if ok() {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		save("failure")
		t.Fatalf("%s: viewport=%+v\n%s", label, term.Viewport(), frame())
	}
	// Establish that an older marker really reached the rendered pane, not just
	// that the HTTP fixture contained it. Later history must recover this text.
	await("older response visibly rendered", func() bool { return strings.Contains(frame(), "CODEX-HISTORY-070") })
	save("older-visible")
	await("completed response", func() bool { return strings.Contains(frame(), "CODEX-HISTORY-100") })
	stable := func() {
		t.Helper()
		last := term.Viewport().Revision
		since := time.Now()
		await("idle rendered pane", func() bool {
			r := term.Viewport().Revision
			if r != last {
				last = r
				since = time.Now()
			}
			return time.Since(since) > 600*time.Millisecond
		})
	}
	stable()
	if term.Viewport().AltScreen != (mode == "always") || term.MouseTracked() != (mode == "always") {
		t.Fatalf("unexpected native modes: %+v mouse=%v", term.Viewport(), term.MouseTracked())
	}
	draft := "DRAFT-WHEEL-MUST-STAY"
	m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(draft)})
	await("draft composer", func() bool { return strings.Contains(frame(), draft) })
	stable()
	save("live-draft")
	if strings.Contains(frame(), "CODEX-HISTORY-070") {
		t.Fatal("target history marker still visible live")
	}
	before := input.bytes()
	ev := paneWheel(tea.MouseButtonWheelUp)
	ev.Shift = mode == "always"
	found := false
	for i := 0; i < 1000; i++ {
		m.handleMouse(ev)
		if strings.Contains(frame(), "CODEX-HISTORY-070") {
			found = true
			break
		}
		v := term.Viewport()
		if v.Offset == 0 || v.Offset == v.HistoryLen {
			break
		}
	}
	if !found {
		save("history-missing")
		t.Fatalf("local TUI wheel did not recover previously visible marker: %+v\n%s", term.Viewport(), frame())
	}
	if !bytes.Equal(before, input.bytes()) {
		t.Fatalf("local wheel changed child input: %q", input.bytes()[len(before):])
	}
	if !strings.Contains(ansi.Strip(m.renderTopBorder()), "↑") {
		t.Fatal("scrolled TUI lacks header indicator")
	}
	save("local-history")
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnd, Alt: true})
	if term.Viewport().Offset != 0 || strings.Contains(ansi.Strip(m.renderTopBorder()), "↑") || !strings.Contains(frame(), draft) {
		t.Fatal("live chord failed to restore exact draft / clear indicator")
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyPgUp, Alt: true})
	if term.Viewport().Offset == 0 {
		t.Fatal("TUI page-up did not scroll")
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	await("actual child input returns live", func() bool { return term.Viewport().Offset == 0 && strings.Contains(frame(), draft+"x") })
	stable()
	save("input-live")
	if mode == "always" {
		before = input.bytes()
		ev.Shift = false
		m.handleMouse(ev)
		await("reporting wheel reaches child", func() bool { return bytes.Contains(input.bytes()[len(before):], []byte("\x1b[<64;3;3M")) })
		stable()
		if !strings.Contains(frame(), draft+"x") {
			t.Fatal("reporting wheel changed draft")
		}
		save("app-wheel")
	}
	// Submit a second synthetic turn, then scroll during its paced output. New
	// child repaint output must return live without any further local input.
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	await("second response started", func() bool { return strings.Contains(frame(), "NEXT-HISTORY-003") })
	m.handleKey(tea.KeyMsg{Type: tea.KeyPgUp, Alt: true})
	if term.Viewport().Offset == 0 {
		t.Fatal("page up while output streams did not scroll")
	}
	await("new child output returns live", func() bool { return term.Viewport().Offset == 0 && strings.Contains(frame(), "NEXT-HISTORY-004") })
	save("output-live")
	t.Logf("real Codex 0.157.1 %s: recovered rendered marker 070; draft preserved; local wheel sent no input; indicator and input/output live returns verified", mode)
}
