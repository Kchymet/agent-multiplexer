//go:build livecli

// This file holds the empirical half of the steering contract: the unit tests in
// steer_test.go pin the bytes and their spacing against a fake instance, but only
// a real TUI can answer the question that produced AGE-199 — does the runtime
// treat what amux writes as a submitted message, or as a paste it leaves sitting
// in the composer? The two are indistinguishable on screen and in every fake, and
// the difference is the whole bug.
//
// It is behind the `livecli` build tag because it launches the installed agent
// CLIs, needs them authenticated, and costs a model turn per runtime. CI never
// runs it; a developer changing Keys or steerPayload should:
//
//	go test -tags livecli -run TestLiveSteer -timeout 10m ./internal/daemon
//
// Each runtime the registry claims to steer gets one case, and a runtime whose
// binary is missing skips rather than fails: the check is "the mapping we ship is
// right about the CLI installed here", not "every CLI is installed here".

package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"amux/internal/agent"
	"amux/internal/core"
	"amux/internal/engine"
	"amux/internal/engine/local"
)

// liveBootSettle is how long the spawned TUI is given to paint its composer
// before the payload is typed. It is deliberately far more generous than the
// daemon's own steerStartSettle: this test is about what happens *after* the text
// is delivered, so a boot still in progress would only add noise.
const liveBootSettle = 10 * time.Second

// liveTurnBudget bounds the wait for the submitted turn to reach the transcript.
// A runtime records a user message when it *starts* the turn, so this is boot
// plus first-token latency, not the length of the answer.
const liveTurnBudget = 120 * time.Second

func TestLiveSteerSubmitsPrompt(t *testing.T) {
	// One line is the ordinary prompt; several lines are the reason the payload
	// frames text as a paste at all. A runtime that submits the first case and
	// splits the second at its newline is broken in a way only the second catches.
	bodies := map[string]string{
		"single-line": "how many lines did this prompt arrive as?",
		"multi-line":  "first line of one prompt\nsecond line of the same prompt",
	}
	for _, rt := range liveRuntimes {
		for shape, body := range bodies {
			t.Run(rt.kind+"/"+shape, func(t *testing.T) {
				bin, err := exec.LookPath(rt.bin)
				if err != nil {
					t.Skipf("%s is not installed on this machine", rt.bin)
				}
				keys := agent.HarnessFor(rt.kind).Keys()
				if !keys.Steerable() {
					t.Fatalf("registry reports %s unsteerable: %v", rt.kind, keys)
				}

				// A phrase nothing else can contain, so a hit proves *this* delivery
				// submitted rather than some neighbouring conversation of the same CLI.
				text := "hello from steer " + time.Now().UTC().Format("20060102T150405.000") + " " + body
				payload, err := steerPayload(core.SteerPrompt, keys, map[string]string{core.SteerText: text})
				if err != nil {
					t.Fatal(err)
				}

				home := rt.sandbox(t)
				in, screen := liveInstance(t, bin, home)
				time.Sleep(liveBootSettle)
				in.InputSequence(payload)

				// Matching the text *verbatim* is the assertion: a prompt split at its
				// newline leaves a truncated user turn, which reads as delivered to any
				// check that only looks for the first few words.
				if !waitForSubmittedTurn(home.transcripts, text, liveTurnBudget) {
					t.Fatalf("%s did not submit the steered prompt verbatim — it is unsent in the "+
						"composer, or arrived split.\nlast screen:\n%s", rt.kind, screen())
				}
			})
		}
	}
}

// liveHome is a throwaway installation of one runtime: a private config root that
// is authenticated (borrowed from the developer's own), pre-trusts the working
// directory, and journals its conversations somewhere only this test looks. The
// isolation is what makes the assertion sound — the transcript it searches can
// only have been written by the CLI it just steered.
type liveHome struct {
	dir         string   // working directory the CLI is launched in
	env         []string // config-root overrides for the spawned process
	transcripts string   // root of the JSONL conversation logs it will write
}

// liveRuntimes is the set of runtimes this check covers, each paired with the
// sandbox its CLI needs. A new steerable agent kind belongs here too — Keys for a
// runtime nothing ever drove is a mapping nobody has checked.
var liveRuntimes = []struct {
	kind    string
	bin     string
	sandbox func(*testing.T) liveHome
}{
	{kind: "claude", bin: "claude", sandbox: claudeHome},
	{kind: "codex", bin: "codex", sandbox: codexHome},
}

// claudeHome clones just enough of the developer's Claude Code config for a
// headless run: the credentials (symlinked, never copied) and the account/
// onboarding state, with the throwaway working directory marked trusted. Their
// *settings* are deliberately not carried over — a developer's hooks, status line
// and editor mode have no business running inside a delivery test — and are
// replaced by the two answers that keep the CLI from opening on a dialog, since a
// dialog eating the steered keystrokes would look exactly like the bug under test.
func claudeHome(t *testing.T) liveHome {
	t.Helper()
	src := os.Getenv("CLAUDE_CONFIG_DIR")
	if src == "" {
		home, _ := os.UserHomeDir()
		src = filepath.Join(home, ".claude")
	}
	cfg, dir := t.TempDir(), t.TempDir()

	var state map[string]any
	if b, err := os.ReadFile(filepath.Join(src, ".claude.json")); err == nil {
		_ = json.Unmarshal(b, &state)
	}
	if state == nil {
		t.Skipf("no Claude Code config at %s to borrow authentication from", src)
	}
	state["projects"] = map[string]any{dir: map[string]any{"hasTrustDialogAccepted": true}}
	b, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, ".claude.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	linkCredentials(t, filepath.Join(src, ".credentials.json"), filepath.Join(cfg, ".credentials.json"))
	// Without these 2.1.x opens on a renderer opt-in whose Enter-to-confirm dialog
	// swallows the steered payload.
	if err := os.WriteFile(filepath.Join(cfg, "settings.json"),
		[]byte(`{"tui":"fullscreen","theme":"dark"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	return liveHome{
		dir:         dir,
		env:         append([]string{"CLAUDE_CONFIG_DIR=" + cfg}, detachedFrom("CLAUDE")...),
		transcripts: filepath.Join(cfg, "projects"),
	}
}

// detachedFrom blanks the ambient variables by which a runtime recognises itself
// as nested inside another of its own sessions. It matters because this test is
// most often run *from* an agent session: Claude Code inheriting
// CLAUDE_CODE_CHILD_SESSION stops journalling a transcript, and a check that
// reads the transcript would then fail on a prompt that was in fact submitted.
// Values are appended rather than removed because Spec.Env only adds — a later
// assignment wins, and empty reads as unset to every runtime here.
func detachedFrom(prefix string) []string {
	var out []string
	for _, e := range os.Environ() {
		k, _, ok := strings.Cut(e, "=")
		if ok && strings.HasPrefix(k, prefix) && k != prefix+"_CONFIG_DIR" && k != prefix+"_HOME" {
			out = append(out, k+"=")
		}
	}
	return out
}

// codexHome does the same for Codex: a private CODEX_HOME holding a link to the
// developer's credentials and the config that keeps the CLI from prompting on
// first run.
func codexHome(t *testing.T) liveHome {
	t.Helper()
	src := os.Getenv("CODEX_HOME")
	if src == "" {
		home, _ := os.UserHomeDir()
		src = filepath.Join(home, ".codex")
	}
	cfg, dir := t.TempDir(), t.TempDir()
	if _, err := os.Stat(src); err != nil {
		t.Skipf("no Codex config at %s to borrow authentication from", src)
	}
	linkCredentials(t, filepath.Join(src, "auth.json"), filepath.Join(cfg, "auth.json"))
	for _, name := range []string{"config.toml", "config.json"} {
		if b, err := os.ReadFile(filepath.Join(src, name)); err == nil {
			if err := os.WriteFile(filepath.Join(cfg, name), b, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	return liveHome{
		dir:         dir,
		env:         append([]string{"CODEX_HOME=" + cfg}, detachedFrom("CODEX")...),
		transcripts: filepath.Join(cfg, "sessions"),
	}
}

// linkCredentials points the sandbox at the developer's real credential file
// rather than copying it: a token duplicated into a temp directory outlives the
// test on any machine where TempDir is not cleaned, and the CLI is happy to
// follow the link.
func linkCredentials(t *testing.T, from, to string) {
	t.Helper()
	real, err := filepath.EvalSymlinks(from)
	if err != nil {
		return // no credential file here; the CLI may authenticate some other way
	}
	if err := os.Symlink(real, to); err != nil {
		t.Fatal(err)
	}
}

// liveInstance spawns one real agent CLI through the production engine — the same
// spawn, the same input FIFO, the same delay handling the daemon uses — and
// returns it alongside a snapshot of everything it has painted. Going through
// engine/local rather than a bare PTY is the point: a fix that only worked when
// the writes bypassed the FIFO would not be a fix.
func liveInstance(t *testing.T, bin string, home liveHome) (engine.Instance, func() string) {
	t.Helper()
	eng := local.New()
	t.Cleanup(eng.Shutdown)

	// Output arrives on the engine's pump goroutine while the test reads the
	// snapshot from its own, so the buffer is guarded.
	var mu sync.Mutex
	var out []byte
	sink := engine.Sink{Output: func(b []byte) {
		mu.Lock()
		defer mu.Unlock()
		out = append(out, b...)
	}}

	in, err := eng.Ensure(context.Background(), engine.Spec{
		Key:  engine.Key{AgentID: "live", Tab: 0},
		Dir:  home.dir,
		Env:  home.env,
		Cols: 120, Rows: 40,
		Argv: []string{bin},
	})
	if err != nil {
		t.Fatalf("spawn %s: %v", bin, err)
	}
	t.Cleanup(in.Subscribe(sink))
	return in, func() string {
		mu.Lock()
		defer mu.Unlock()
		return string(out)
	}
}

// waitForSubmittedTurn polls the sandbox's transcripts for a *user* turn carrying
// text. Reading the transcript rather than the screen is what makes this a real
// check: text that only reached the composer paints identically to text that was
// submitted, and AGE-199 is exactly the difference between the two.
func waitForSubmittedTurn(root, text string, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for {
		for _, path := range transcriptsUnder(root) {
			if hasUserTurn(path, text) {
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(2 * time.Second)
	}
}

// transcriptsUnder lists the JSONL conversation logs beneath root. An absent root
// is not an error: it simply means the runtime has not journalled anything yet,
// which is the state this poll exists to wait out.
func transcriptsUnder(root string) []string {
	var paths []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".jsonl") {
			paths = append(paths, p)
		}
		return nil
	})
	return paths
}

// hasUserTurn reports whether path records text as a submitted user message. It
// decodes each record and searches its string fields rather than scanning the raw
// line, because a multi-line prompt is stored JSON-escaped: "a\nb" on disk is
// four characters, and a raw-bytes search would miss the very case bracketed
// paste exists to carry. The role check is equally load-bearing — an assistant
// quoting the phrase back, or a tool result echoing it, must not count, or the
// test would pass on the outcome it is meant to catch.
func hasUserTurn(path, text string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	escaped, err := json.Marshal(text)
	if err != nil {
		return false
	}
	needle := string(escaped[1 : len(escaped)-1]) // the text as it appears inside a JSON string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if !strings.Contains(string(line), needle) {
			continue
		}
		var rec map[string]any
		if json.Unmarshal(line, &rec) == nil && isUserRecord(rec) && containsText(rec, text) {
			return true
		}
	}
	return false
}

// containsText reports whether any string anywhere in a decoded record holds
// text. Records nest the message body differently per runtime (and per content
// block), so the search walks the value rather than naming a path into it.
func containsText(v any, text string) bool {
	switch t := v.(type) {
	case string:
		return strings.Contains(t, text)
	case []any:
		for _, e := range t {
			if containsText(e, text) {
				return true
			}
		}
	case map[string]any:
		for _, e := range t {
			if containsText(e, text) {
				return true
			}
		}
	}
	return false
}

// isUserRecord decides whether a transcript record is the user's turn across both
// CLIs' shapes: Claude Code tags the envelope ("type":"user"), Codex carries a
// role on a nested payload.
func isUserRecord(rec map[string]any) bool {
	if s, _ := rec["type"].(string); s == "user" {
		return true
	}
	if s, _ := rec["role"].(string); s == "user" {
		return true
	}
	for _, k := range []string{"payload", "message"} {
		if nested, ok := rec[k].(map[string]any); ok && isUserRecord(nested) {
			return true
		}
	}
	return false
}
